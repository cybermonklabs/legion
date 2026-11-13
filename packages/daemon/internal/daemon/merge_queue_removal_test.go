package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's own log lines these cases read, as the workflow and the two merge-queue
// executors write them (workflow's dequeued and mergeQueueReadDone, the daemon's mergeQueueRead
// and mergeQueueRemoval). dequeueReadLine is the start of every line the read executor writes for
// a read it made, one per read, and of no other line: its line for a row it ends without reading
// says `read finished`.
const (
	dequeueReadLine    = "outbox merge-queue read:"
	dequeueReadQueued  = "workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once"
	dequeueReadDropped = "workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read"
	dequeueMessageOwed = "workflow: the merger is owed word that its pull request left the merge queue"
	dequeueLeftMerging = "outbox merge-queue removal finished undelivered: the issue left merging at that head"
	dequeueMergerEnded = "outbox merge-queue removal finished undelivered: the merger's claim has failed or retired"
	dequeueDelivered   = "outbox merge-queue removal delivered to the merger"
)

// dequeueWait bounds each wait of this test on the daemon's own work: an event through the
// intake, a read of GitHub, a fact, and an outbox row's tries, the longest of whose waits here is
// the four seconds before a row's fourth try (the outbox's retryBackoff). It is the bound a fault
// is reported at, far above that, so a slow machine is not a failure and a row that never
// finishes fails its case in seconds rather than at the package's deadline. dequeuePoll is how
// often a wait asks: each question is a read of the database or of the daemon's whole log, so it
// is not asked a hundred times a second.
const (
	dequeueWait = 30 * time.Second
	dequeuePoll = 50 * time.Millisecond
)

// dequeueRetry is the wait between two failed reads of GitHub the daemon of this test makes
// (overrides.mergeStateRetry): half a minute in production (mergeStateLadderWait), so five failed
// reads would take two minutes.
const dequeueRetry = 20 * time.Millisecond

// TestADequeuedPullRequestReachesItsMerger holds that a pull request GitHub takes out of the merge
// queue reaches the issue's merger (dispatch://INFRA-3274/spec R1): the daemon reads the pull
// request once, applies the merge when GitHub reports one, and otherwise tells the merger on its
// own role topic, waking its pod when the idle release took it and holding the message until the
// woken session holds the merger's role. Nothing else tells a merger of the removal: its pull
// request would wait out of the queue with nobody to enqueue it again or send the issue back.
//
// The daemon is the whole daemon (run, through startDaemonWithin) on a database of its own and on
// the NATS server testnats names (LEGION_TEST_NATS_URL and LEGION_TEST_NATS_MONITOR_URL, in a pod
// with no Docker). The event enters as production's does: an Envoy envelope of GitHub's
// pull_request action `dequeued` on notifications.github.acme.widgets.pr.<number>, with the keys
// the listener publishes for every pull_request action (packages/envoy/internal/contracts/
// normalize.go), GitHub's reason for the removal not among them. GitHub is dequeueGitHub, which
// answers GET /pulls/<number> as each case sets it and counts every read. Envoy's listener is
// dequeueListener, which answers that no live session holds a merger's topic (the 404 notify
// reads as ErrNoHolder), and counts each such refusal, until the test says the woken session holds
// it. The project sets `production_check: off` (workflowConfig's project names no check), so the
// close of case 2 opens no check and wakes no role.
//
// A released merger is a claim stored suspended by `released` before the boot, as the idle
// release leaves one: its session kept, and nothing but a wake to bring its pod back. The boot's
// resume by rule starts no released claim (workflow's booted). Admission's slot repair and the
// entry sweep start a claim only when it stands behind the entry its phases row names (admit's
// owedEntryRoles, workflow.OwedEntry), and no case of this test writes a phase row, so every role
// here stands at entry 0 and was given nothing, which is owed nothing. The boot's cutover repair
// does start a released merger in merging once, but only one whose phase row holds a READY head
// (workflow's cutoverMergerRepair), so the repair passes over every merger here too. The only
// resume of a released merger is therefore the delivery's own (wakeForDelivery), which the
// record keeps as one pods.WakeDelivery row of pod_resumes. The idle release of
// idle_rig_test.go's rig is not used: it needs the Agent Sandbox runtime over a Kubernetes
// stand-in and a real listener, and the rows under test read only the claim's state.
//
// The reads of GitHub are counted from the moment the event is published. The daemon's per-issue
// read asks the same path once for every issue with an open pull request as the runtime starts
// (readIssuesAtStart), and logs no line when it ends, so the boot waits until GitHub has counted
// that read and its reviews read, its last call, for every issue before any case publishes. The
// per-issue read asks again only after a recorded handoff, and the merge-state ladder only after
// an opened, synchronize, push, check, review or base-push fact, and no case records or applies
// any of them. Each case then holds the count to the read executor's own lines (one per read), so
// a read anything else made would fail it.
//
// Each case has an issue of its own, each issue its own tree root holding its admission slot, so
// the nine share one daemon:
//
//  1. The removal R1 is about: the issue in merging, the event at the head the record holds,
//     GitHub answering open, the merger's pod released. GitHub is read once; the merger's claim is
//     resumed once; the listener refuses the woken merger's topic at the row's second try, and
//     the session takes the topic only after it; and the session is then told once, of the pull
//     request, the head, and that the event names no reason. A row that ended on that refusal, as
//     a phase-ended message does, would leave the merger it woke untold. The removal moves no
//     phase and writes no slot.
//  2. GitHub's merge queue reports a removal in the same second as the merge it makes, so the read
//     decides: GitHub answers merged, and the issue closes as the webhook's merge would close it,
//     with no message written, nothing published and no pod woken.
//  3. An issue in implementing has no merge to watch, and 4, a removal at a head the record has
//     moved past, is of a head the merger no longer holds: neither reads GitHub, writes a
//     message, publishes or wakes anyone.
//  5. GitHub fails every read: the daemon reads five times, the wait between reads cut through
//     overrides.mergeStateRetry, and then tells the merger once that it could not read the pull
//     request, rather than saying nothing or reading on.
//  6. The issue leaves merging while the message waits for the woken session, by the merger's own
//     backward move as the API applies it, which records no handoff and applies no push: the row
//     ends with nothing delivered, and its line names why, since the merger would read of a phase
//     it no longer works.
//  7. A retired merger will not hold its role again, so a row that waited for it would wait for
//     ever: the row ends undelivered, with nothing published.
//  8. So will a failed merger not, and the row ends the same way. The failure holds the issue's
//     phase (workflow's claimFailed, which the boot replays for the claim stored failed), so a
//     failed merger stands with its issue in merging only once the architect's retry of that
//     phase has put the issue back, and only while the start the retry writes, which relaunches
//     the claim, waits. The test applies that retry as the API applies it, and the issue's closed
//     design gate holds the start (the outbox's mergerWaitsForTheDesignGate), so the claim is
//     still failed when the row reads it.
//  9. The design gate closed and the merger's pod released: a merger the gate holds may not know
//     the gate closed, and woken by the message it could enqueue the pull request again against
//     the gate. The row neither wakes it nor publishes while the gate is closed, and once the test
//     opens the gate, the merger is woken and told once.
func TestADequeuedPullRequestReachesItsMerger(t *testing.T) {
	rig := newDequeueRig(t)
	woken := rig.seed(t, phase.Merging, supervise.StateSuspended, false)
	merged := rig.seed(t, phase.Merging, supervise.StateSuspended, false)
	implementing := rig.seed(t, phase.Implementing, supervise.StateSuspended, false)
	moved := rig.seed(t, phase.Merging, supervise.StateSuspended, false)
	unread := rig.seed(t, phase.Merging, supervise.StateSuspended, false)
	leaves := rig.seed(t, phase.Merging, supervise.StateSuspended, false)
	retired := rig.seed(t, phase.Merging, supervise.StateRetired, false)
	failed := rig.seed(t, phase.Merging, supervise.StateFailed, true)
	gated := rig.seed(t, phase.Merging, supervise.StateSuspended, true)
	rig.boot(t)

	t.Run("a released merger is woken and told once its session holds the role", func(t *testing.T) {
		issue := woken
		slots := rig.slots(t, issue.key)
		reads := rig.publish(t, issue, issue.head)
		rig.await(t, "the listener's refusal of the merger's topic at the message's second try", func() bool {
			return rig.listener.refused(issue.topic) >= 1
		})
		rig.wokenOnce(t, issue)
		rig.listener.holds(issue.topic)
		rig.await(t, "the message's row to finish once the woken session holds the merger's role", func() bool {
			return rig.rows(t, issue.key, record.OutboxKindMergeQueueRemoval) == 0
		})
		told := rig.told(t, issue)
		if strings.Contains(told, "could not read") {
			t.Fatalf("the merger was told %q, want no word of a failed read: GitHub answered the read", told)
		}
		rig.readsWere(t, issue, reads, 1)
		rig.wokenOnce(t, issue)
		if stands := rig.phase(t, issue.key); stands != phase.Merging {
			t.Fatalf("%s stands in %s after the removal, want %s: the merger decides what a removal means, and the daemon moves no phase for it",
				issue.key, stands, phase.Merging)
		}
		if written := rig.slots(t, issue.key); slices.ContainsFunc(written, func(row string) bool { return !slices.Contains(slots, row) }) {
			t.Fatalf("the slots of %s are %v after the removal, were %v: the removal writes no slot", issue.key, written, slots)
		}
	})

	t.Run("a merged answer closes the issue and tells nobody", func(t *testing.T) {
		issue := merged
		rig.github.answer(issue.number, dequeueMerged)
		reads := rig.publish(t, issue, issue.head)
		rig.await(t, "the issue to close on the merge GitHub reported", func() bool {
			return rig.phase(t, issue.key) == phase.Done
		})
		rig.await(t, "the read's row to finish", func() bool {
			return rig.rows(t, issue.key, record.OutboxKindMergeQueueRead) == 0
		})
		rig.readsWere(t, issue, reads, 1)
		rig.nothingOwed(t, issue)
		rig.nothingPublished(t, issue)
		if resumed := rig.podResumes(t, issue.key); resumed != 0 {
			t.Fatalf("%d resumes are recorded for %s, want none: a merged pull request wakes no role", resumed, issue.key)
		}
	})

	t.Run("an issue that is not in merging reads nothing", func(t *testing.T) {
		rig.dropped(t, implementing, implementing.head, "implementing")
	})

	t.Run("a removal at a head the record has moved past reads nothing", func(t *testing.T) {
		rig.dropped(t, moved, dequeueHead(9000+moved.number), moved.head)
	})

	t.Run("after five failed reads the merger is told the daemon could not read the pull request", func(t *testing.T) {
		issue := unread
		rig.github.answer(issue.number, dequeueUnavailable)
		reads := rig.publish(t, issue, issue.head)
		rig.await(t, "the merger woken for the message once the fifth read failed", func() bool {
			return rig.wakes(t, issue.key) >= 1
		})
		rig.listener.holds(issue.topic)
		rig.await(t, "the message's row to finish once the woken session holds the merger's role", func() bool {
			return rig.rows(t, issue.key, record.OutboxKindMergeQueueRemoval) == 0
		})
		if told := rig.told(t, issue); !strings.Contains(told, "could not read the pull request") {
			t.Fatalf("the merger was told %q, want it told that the daemon could not read the pull request", told)
		}
		rig.readsWere(t, issue, reads, mergeStateLadderReads)
		rig.wokenOnce(t, issue)
	})

	t.Run("an issue that leaves merging while the message waits is not told", func(t *testing.T) {
		issue := leaves
		reads := rig.publish(t, issue, issue.head)
		rig.await(t, "the message to wait for the woken merger's session", func() bool {
			return rig.listener.refused(issue.topic) >= 1
		})
		applyFact(t, rig.pool, "api", "backward:"+issue.key, intake.BackwardMove{Issue: issue.key, Requester: claim.RoleMerger,
			To: phase.Implementing, Reason: "the head needs work the merge queue showed"}, rig.engine)
		rig.await(t, "the message's row to finish once the issue left merging", func() bool {
			return rig.rows(t, issue.key, record.OutboxKindMergeQueueRemoval) == 0
		})
		ended := rig.logged(dequeueLeftMerging, issue.key)
		if len(ended) != 1 || !strings.Contains(fmt.Sprint(ended[0]["why"]), string(phase.Implementing)) {
			t.Fatalf("the row's lines %q = %v, want one naming the move to %s", dequeueLeftMerging, ended, phase.Implementing)
		}
		if delivered := rig.listener.accepted(issue.topic); len(delivered) != 0 {
			t.Fatalf("the merger's topic was delivered %v, want nothing: the issue left merging before its merger held the role", delivered)
		}
		if lines := rig.logged(dequeueDelivered, issue.key); len(lines) != 0 {
			t.Fatalf("the row logged %v, want no delivery", lines)
		}
		rig.readsWere(t, issue, reads, 1)
	})

	for _, tc := range []struct {
		name  string
		issue dequeueIssue
		state supervise.ClaimState
		// before puts the issue where the case reads it, nil where the boot already has.
		before func(t *testing.T, issue dequeueIssue)
	}{
		{name: "a retired merger is not told", issue: retired, state: supervise.StateRetired},
		{name: "a failed merger is not told", issue: failed, state: supervise.StateFailed, before: rig.retryHeld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := tc.issue
			if tc.before != nil {
				tc.before(t, issue)
			}
			reads := rig.publish(t, issue, issue.head)
			rig.await(t, "the message's row to end on the merger's claim", func() bool {
				return len(rig.logged(dequeueMergerEnded, issue.key)) == 1 &&
					rig.rows(t, issue.key, record.OutboxKindMergeQueueRemoval) == 0
			})
			if ended := rig.logged(dequeueMergerEnded, issue.key)[0]; ended["state"] != string(tc.state) {
				t.Fatalf("the row ended on a merger whose claim is %v, want %s", ended["state"], tc.state)
			}
			rig.nothingPublished(t, issue)
			if resumed := rig.podResumes(t, issue.key); resumed != 0 {
				t.Fatalf("%d resumes are recorded for %s, want none: a %s claim is not woken", resumed, issue.key, tc.state)
			}
			rig.readsWere(t, issue, reads, 1)
		})
	}

	t.Run("a closed design gate holds the released merger's message until it opens", func(t *testing.T) {
		issue := gated
		reads := rig.publish(t, issue, issue.head)
		rig.await(t, "the message's row held over two tries", func() bool {
			return rig.attempts(t, issue.key, record.OutboxKindMergeQueueRemoval) >= 2
		})
		if held := rig.lastError(t, issue.key, record.OutboxKindMergeQueueRemoval); !strings.Contains(held, "design gate") {
			t.Fatalf("the message's row waits on %q, want the design gate", held)
		}
		if resumed, resumes := rig.podResumes(t, issue.key), len(resumesOf(rig.rt, issue.merger)); resumed != 0 || resumes != 0 {
			t.Fatalf("while the gate is closed %d resumes are recorded and the runtime resumed the merger %d times, want neither", resumed, resumes)
		}
		rig.nothingPublished(t, issue)
		approved := 2
		putOutboxGate(t, rig.pool, rig.records, record.DesignGate{Issue: issue.key, Step: record.DesignStepSpec, ArtifactID: "spec-" + issue.key,
			LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
		rig.await(t, "the merger woken for the message once the gate opened", func() bool {
			return rig.wakes(t, issue.key) >= 1
		})
		rig.listener.holds(issue.topic)
		rig.await(t, "the message's row to finish once the woken session holds the merger's role", func() bool {
			return rig.rows(t, issue.key, record.OutboxKindMergeQueueRemoval) == 0
		})
		rig.told(t, issue)
		rig.readsWere(t, issue, reads, 1)
		rig.wokenOnce(t, issue)
	})
}

// dequeueRig is the daemon this test drives and the stand-ins it reads back from: GitHub, Envoy's
// listener, the runtime the claims launch in, and the daemon's own log.
type dequeueRig struct {
	cfg      config.Config
	o        overrides
	project  string
	pool     *pgxpool.Pool
	records  *record.Postgres
	js       jetstream.JetStream
	github   *dequeueGitHub
	listener *dequeueListener
	rt       *fake.Runtime
	logs     *syncBuffer
	// engine is the workflow engine the daemon runs, built as the daemon builds it, which applies
	// the one fact a case applies of its own (startedAgainEngine).
	engine *workflow.Engine
	issues []dequeueIssue
}

// dequeueIssue is one case's issue: its key, its pull request's number and the head the record
// holds it at, and its merger's claim and that claim's role topic.
type dequeueIssue struct {
	key    string
	number int
	head   string
	merger claim.Token
	topic  string
}

// newDequeueRig readies the daemon's configuration and every stand-in, and boots nothing: the
// cases' issues are seeded first (seed), and then the daemon boots on them (boot). The board is
// startedAgainDispatch's, which answers what every boot asks of Dispatch and leaves every later
// write retrying in its own lane. The runtime is the fake, provisioning each workspace in the
// claim's own pod, so a launch clones nothing.
func newDequeueRig(t *testing.T) *dequeueRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	natsURL := workflowNATS(t)
	cfg := workflowConfig(t, natsURL)
	cfg.PostgresDSN = pool.Config().ConnString()
	cfg.DispatchURL = startedAgainDispatch(t, cfg.Project)
	// Room for every case's tree, so the cap holds no issue of the record back.
	cfg.AdmissionCap = 16
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	rig := &dequeueRig{cfg: cfg, project: project, pool: pool, records: record.NewStore(), js: workflowJetStream(t, natsURL),
		github: newDequeueGitHub(t), rt: fake.NewRuntime(), logs: &syncBuffer{}, engine: startedAgainEngine(cfg)}
	rig.listener, rig.cfg.EnvoyURL = newDequeueListener(t)
	rig.rt.InPod = true
	rig.o = fakeRuntime(rig.rt, &built{})
	rig.o.workflowTokens = &workflowTokenRecorder{}
	rig.o.githubAPI = rig.github.url
	rig.o.mergeStateRetry = dequeueRetry
	return rig
}

// seed records one case's issue before the boot: a tree root of its own standing in stands,
// holding its admission slot, its lifecycle open under the workflow, on an open pull request
// GitHub last read mergeable and clean at the issue's head on main, so the boot's start read
// applies nothing. Its merger's claim stands in merger, where a suspended claim is one the idle
// release suspended: released, its session kept. An issue in implementing has its implementer
// released too. No phase row is written, so no role of the issue is behind its entry and the
// slot repair starts no worker of it (workflow.OwedEntry). gateClosed records the root's design
// gate closed by a newer version of its spec than the one approved.
func (r *dequeueRig) seed(t *testing.T, stands phase.Phase, merger supervise.ClaimState, gateClosed bool) dequeueIssue {
	t.Helper()
	index := len(r.issues) + 1
	key := r.cfg.Project + "-" + strconv.Itoa(index)
	issue := dequeueIssue{key: key, number: 300 + index, head: dequeueHead(300 + index)}
	claims := []supervise.Claim{r.claim(t, key, claim.RoleMerger, merger)}
	issue.merger = claims[0].Token
	issue.topic = notify.RoleTopicPrefix + string(issue.merger)
	if stands == phase.Implementing {
		claims = append(claims, r.claim(t, key, claim.RoleImplementer, supervise.StateSuspended))
	}
	seedWorkflowTree(t, r.cfg.PostgresDSN, r.project, startedAgainIssue(r.cfg, key, key, stands), claims...)
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: startedAgainRepo,
			Number: issue.number, Branch: claim.IssueBranch(key), Base: "main", HeadSHA: issue.head, Failing: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean}); err != nil {
			return err
		}
		if !gateClosed {
			return nil
		}
		approved := 1
		return r.records.PutGate(ctx, tx, record.DesignGate{Issue: key, Step: record.DesignStepSpec, ArtifactID: "spec-" + key,
			LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
	}); err != nil {
		t.Fatalf("record the pull request of %s: %v", key, err)
	}
	r.github.serve(issue)
	r.listener.unheld(issue.topic)
	r.issues = append(r.issues, issue)
	return issue
}

// claim is role's claim on key standing in state, which for a suspended claim is the idle
// release's: suspended by `released`, with the session it ran in kept.
func (r *dequeueRig) claim(t *testing.T, key string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	t.Helper()
	token, err := claim.NewToken(r.project, key, role)
	if err != nil {
		t.Fatal(err)
	}
	held := supervise.Claim{Token: token, Project: r.project, Tree: key, Issue: key, Role: role, Generation: 1, ServingGeneration: 1,
		State: state, Session: "ses_" + string(role) + "_" + key, SessionFile: filepath.Join(r.cfg.StateDir, key+"-"+string(role)+".jsonl")}
	if state == supervise.StateSuspended {
		held.SuspendedBy = supervise.SuspendedByReleased
	}
	return held
}

// dequeueHead is the commit a pull request of this test stands at, by its number.
func dequeueHead(number int) string {
	return fmt.Sprintf("%040x", 0xc0ffee0000+number)
}

// boot runs the daemon over the seeded record until the test ends, and returns once it answers
// /healthz and its start read (readIssuesAtStart) has read every issue's pull request, which is
// the moment the cases' counts of GitHub's reads start from. A failed test prints the end of the
// daemon's log, which is where a row that never finished says why.
func (r *dequeueRig) boot(t *testing.T) {
	t.Helper()
	startDaemonWithin(t, r.cfg, r.o, slog.New(slog.NewJSONHandler(r.logs, nil)), dequeueWait)
	t.Cleanup(func() {
		if t.Failed() {
			log := r.logs.String()
			t.Logf("the end of the daemon's log:\n%s", log[max(0, len(log)-idleRigLogTailBytes):])
		}
	})
	r.await(t, "the start read of every issue's pull request and its reviews (readIssuesAtStart)", func() bool {
		for _, issue := range r.issues {
			if !r.github.startRead(issue.number) {
				return false
			}
		}
		return true
	})
}

// await waits for condition within dequeueWait, or what is left of the test binary's own deadline
// less a second, asking it every dequeuePoll.
func (r *dequeueRig) await(t *testing.T, what string, condition func() bool) {
	t.Helper()
	testwait.WithinPolling(t, testwait.Bounded(t, dequeueWait, time.Second), dequeuePoll, what, condition)
}

// publish publishes GitHub's `dequeued` of issue's pull request at head, as Envoy's listener
// relays it, and returns how many times GitHub had been asked for that pull request before it, the
// mark the case's count of reads starts from.
func (r *dequeueRig) publish(t *testing.T, issue dequeueIssue, head string) int {
	t.Helper()
	reads := r.github.reads(issue.number)
	payload, err := json.Marshal(map[string]string{
		"kind": "pr", "action": "dequeued", "repo": startedAgainRepo, "number": strconv.Itoa(issue.number),
		"title": "the issue the merge queue let go", "author": "legion-implement[bot]",
		"url":      fmt.Sprintf("https://github.com/%s/pull/%d", startedAgainRepo, issue.number),
		"head_sha": head, "before": "", "sender": "github-merge-queue[bot]", "head_ref": claim.IssueBranch(issue.key),
		"head_repo": startedAgainRepo, "base_ref": "main", "merged": "false", "merge_commit_sha": "", "merged_by": "",
		"updated_at": time.Now().UTC().Format(time.RFC3339), "body": "Dispatch: " + issue.key,
	})
	if err != nil {
		t.Fatalf("encode the dequeued payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", strings.ReplaceAll(startedAgainRepo, "/", "."), issue.number)
	publishDaemonGrantEnvelope(t, r.js, subject, "github", r.cfg.Project+"-dequeued-"+issue.key, string(payload))
	return reads
}

// dropped publishes the removal of issue's pull request at head and holds that the workflow drops
// it, its line naming why (the phase or the head, as why), with nothing read, owed, published or
// woken.
func (r *dequeueRig) dropped(t *testing.T, issue dequeueIssue, head, why string) {
	t.Helper()
	reads := r.publish(t, issue, head)
	r.await(t, "the workflow's drop of the removal", func() bool {
		return len(r.logged(dequeueReadDropped, issue.key)) == 1
	})
	if line := r.logged(dequeueReadDropped, issue.key)[0]; !strings.Contains(fmt.Sprint(line["why"]), why) {
		t.Fatalf("the drop says %v, want it to name %s", line["why"], why)
	}
	if queued := r.logged(dequeueReadQueued, issue.key); len(queued) != 0 {
		t.Fatalf("the workflow queued the read %v, want none", queued)
	}
	r.readsWere(t, issue, reads, 0)
	r.nothingOwed(t, issue)
	r.nothingPublished(t, issue)
	if resumed, resumes := r.podResumes(t, issue.key), len(resumesOf(r.rt, issue.merger)); resumed != 0 || resumes != 0 {
		t.Fatalf("%d resumes are recorded for %s and the runtime resumed its merger %d times, want neither", resumed, issue.key, resumes)
	}
}

// retryHeld is the architect's retry of issue's held merging phase (intake.RetryOrEscalate),
// applied through the engine as the API's route applies it, once the boot's replay of the
// merger's stored failure (replayTerminal, workflow's claimFailed) has held the phase. The retry
// puts the issue back in merging and writes the start that relaunches the failed merger, which
// the issue's closed design gate holds, so the merger's claim stays failed.
func (r *dequeueRig) retryHeld(t *testing.T, issue dequeueIssue) {
	t.Helper()
	r.await(t, "the boot's replay of the merger's failure to hold the phase", func() bool {
		return r.phase(t, issue.key) == phase.Held
	})
	applyFact(t, r.pool, "api", "retry:"+issue.key, intake.RetryOrEscalate{Issue: issue.key, Decision: intake.RetryDecision}, r.engine)
	if stands := r.phase(t, issue.key); stands != phase.Merging {
		t.Fatalf("%s stands in %s after the retry of its held phase, want %s", issue.key, stands, phase.Merging)
	}
}

// readsWere holds that GitHub was asked for issue's pull request want times since the mark the
// publish returned, and that the read executor wrote one line for each of those reads: a read the
// executor did not make, such as a per-issue read, would part the two.
func (r *dequeueRig) readsWere(t *testing.T, issue dequeueIssue, mark, want int) {
	t.Helper()
	read := r.github.reads(issue.number) - mark
	lines := len(r.logged(dequeueReadLine, issue.key))
	if read != want || lines != want {
		t.Fatalf("GitHub was read %d times for %s's pull request and the read executor logged %d reads, want %d of each",
			read, issue.key, lines, want)
	}
}

// nothingOwed holds that the workflow wrote issue's merger no message: no line of the row's write,
// and no row.
func (r *dequeueRig) nothingOwed(t *testing.T, issue dequeueIssue) {
	t.Helper()
	if owed := r.logged(dequeueMessageOwed, issue.key); len(owed) != 0 {
		t.Fatalf("the workflow owed %s's merger a message %v, want none", issue.key, owed)
	}
	if rows := r.rows(t, issue.key, record.OutboxKindMergeQueueRemoval); rows != 0 {
		t.Fatalf("the outbox holds %d merge-queue removal rows of %s, want none", rows, issue.key)
	}
}

// nothingPublished holds that nothing reached issue's merger's topic: no publish delivered, and
// none refused.
func (r *dequeueRig) nothingPublished(t *testing.T, issue dequeueIssue) {
	t.Helper()
	if delivered, refused := r.listener.accepted(issue.topic), r.listener.refused(issue.topic); len(delivered) != 0 || refused != 0 {
		t.Fatalf("the merger's topic was delivered %v and refused %d publishes, want nothing published to it", delivered, refused)
	}
}

// told is the one message delivered to issue's merger, which it holds names the pull request, its
// repository and the head, and says that the event names no reason.
func (r *dequeueRig) told(t *testing.T, issue dequeueIssue) string {
	t.Helper()
	delivered := r.listener.accepted(issue.topic)
	if len(delivered) != 1 {
		t.Fatalf("the merger's topic was delivered %d messages %v, want exactly one", len(delivered), delivered)
	}
	message := delivered[0].Message
	for _, want := range []string{fmt.Sprintf("pull request %s#%d", startedAgainRepo, issue.number), "at head " + issue.head, "names no reason"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the merger was told %q, want it to say %q", message, want)
		}
	}
	return message
}

// wokenOnce holds that issue's released merger was resumed through one wake: one delivery wake of
// the merger recorded, no other resume of the issue, and one resume the runtime was asked for.
func (r *dequeueRig) wokenOnce(t *testing.T, issue dequeueIssue) {
	t.Helper()
	wakes, resumed, resumes := r.wakes(t, issue.key), r.podResumes(t, issue.key), len(resumesOf(r.rt, issue.merger))
	if wakes != 1 || resumed != 1 || resumes != 1 {
		t.Fatalf("%s holds %d delivery wakes of its merger among %d resumes, and the runtime resumed the merger %d times; want one of each",
			issue.key, wakes, resumed, resumes)
	}
}

// logged is every line of the daemon's log about issue whose message starts with msg.
func (r *dequeueRig) logged(msg, issue string) []map[string]any {
	var lines []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var entry map[string]any
		if line == "" || json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if text, _ := entry["msg"].(string); strings.HasPrefix(text, msg) && entry["issue"] == issue {
			lines = append(lines, entry)
		}
	}
	return lines
}

// rows is how many rows of kind the outbox holds for issue. A finished row is deleted
// (record.Postgres.FinishOutbox).
func (r *dequeueRig) rows(t *testing.T, issue string, kind record.OutboxKind) int {
	t.Helper()
	var rows int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = $2`,
		issue, string(kind)).Scan(&rows); err != nil {
		t.Fatalf("read the %s rows of %s: %v", kind, issue, err)
	}
	return rows
}

// attempts is how many tries the outbox has made of issue's row of kind, 0 for none.
func (r *dequeueRig) attempts(t *testing.T, issue string, kind record.OutboxKind) int {
	t.Helper()
	var attempts int
	if err := r.pool.QueryRow(context.Background(), `select coalesce(max(attempts), 0) from outbox where issue = $1 and kind = $2`,
		issue, string(kind)).Scan(&attempts); err != nil {
		t.Fatalf("read the tries of %s's %s row: %v", issue, kind, err)
	}
	return attempts
}

// lastError is what issue's row of kind last waited on or failed with.
func (r *dequeueRig) lastError(t *testing.T, issue string, kind record.OutboxKind) string {
	t.Helper()
	var last string
	if err := r.pool.QueryRow(context.Background(), `select coalesce(max(last_error), '') from outbox where issue = $1 and kind = $2`,
		issue, string(kind)).Scan(&last); err != nil {
		t.Fatalf("read the last error of %s's %s row: %v", issue, kind, err)
	}
	return last
}

// wakes is how many delivery wakes of its merger the record keeps for issue: the resume
// wakeForDelivery takes (pods.WakeDelivery).
func (r *dequeueRig) wakes(t *testing.T, issue string) int {
	t.Helper()
	var wakes int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from pod_resumes where issue = $1 and role = $2 and kind = $3`,
		issue, string(claim.RoleMerger), pods.WakeDelivery).Scan(&wakes); err != nil {
		t.Fatalf("read the delivery wakes of %s: %v", issue, err)
	}
	return wakes
}

// podResumes is how many resumes of any role, of any kind, the record keeps for issue.
func (r *dequeueRig) podResumes(t *testing.T, issue string) int {
	t.Helper()
	var resumes int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from pod_resumes where issue = $1`, issue).Scan(&resumes); err != nil {
		t.Fatalf("read the resumes of %s: %v", issue, err)
	}
	return resumes
}

// slots is issue's rows of the slots table, each its index and when it was admitted.
func (r *dequeueRig) slots(t *testing.T, issue string) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `select index, admitted_at from slots where issue = $1 order by index`, issue)
	if err != nil {
		t.Fatalf("read the slots of %s: %v", issue, err)
	}
	defer rows.Close()
	var slots []string
	for rows.Next() {
		var index int
		var admitted time.Time
		if err := rows.Scan(&index, &admitted); err != nil {
			t.Fatalf("read the slots of %s: %v", issue, err)
		}
		slots = append(slots, fmt.Sprintf("%d at %s", index, admitted.UTC().Format(time.RFC3339Nano)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the slots of %s: %v", issue, err)
	}
	return slots
}

// phase is the phase the record holds issue in.
func (r *dequeueRig) phase(t *testing.T, key string) phase.Phase {
	t.Helper()
	var stands phase.Phase
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		issue, err := r.records.Issue(context.Background(), tx, key)
		if err != nil || issue == nil {
			return fmt.Errorf("the record holds %s as %v: %w", key, issue, err)
		}
		stands = issue.Phase
		return nil
	}); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return stands
}

// dequeueAnswer is how GitHub answers the read of one pull request: open at its head, which is
// what every pull request answers until its case says otherwise; merged; or a 502, as GitHub
// answers while it is unavailable.
type dequeueAnswer string

const (
	dequeueOpen        dequeueAnswer = "open"
	dequeueMerged      dequeueAnswer = "merged"
	dequeueUnavailable dequeueAnswer = "unavailable"
)

// dequeueGitHub is GitHub's REST API for acme/widgets as this test's daemon reads it: each case's
// pull request, answered as its case sets it and counted at every read, with its reviews as none;
// its list of open pull requests as empty; and the repository, and any other a launch reads, with
// main its default branch. Anything else is answered 404 and logged, so a read the test did not
// foresee is named in its output rather than reaching github.com.
type dequeueGitHub struct {
	url   string
	mu    sync.Mutex
	pulls map[int]*dequeuePull
}

// dequeuePull is one pull request as dequeueGitHub holds it: the issue's branch and the head it
// stands at, its answer, and the reads of it and of its reviews so far.
type dequeuePull struct {
	issue  dequeueIssue
	answer dequeueAnswer
	reads  int
	// reviewReads counts the reads of its reviews, the per-issue read's last call (readReviews).
	reviewReads int
}

func newDequeueGitHub(t *testing.T) *dequeueGitHub {
	t.Helper()
	g := &dequeueGitHub{pulls: map[int]*dequeuePull{}}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && g.answered(w, r.URL.Path) {
			return
		}
		t.Logf("the GitHub stand-in has no answer for %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	return g
}

// serve makes GitHub hold issue's pull request, open at the issue's head.
func (g *dequeueGitHub) serve(issue dequeueIssue) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[issue.number] = &dequeuePull{issue: issue, answer: dequeueOpen}
}

// answer makes GitHub answer every later read of pull request number as answer says.
func (g *dequeueGitHub) answer(number int, answer dequeueAnswer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number].answer = answer
}

// reads is how many times GitHub was asked for pull request number.
func (g *dequeueGitHub) reads(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pulls[number].reads
}

// startRead is whether the per-issue read has made both its calls of pull request number: the
// pull request, and then its reviews.
func (g *dequeueGitHub) startRead(number int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.pulls[number]
	return pull.reads >= 1 && pull.reviewReads >= 1
}

// answered writes the stand-in's answer to a GET of path, and says whether it had one.
func (g *dequeueGitHub) answered(w http.ResponseWriter, path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rest, ours := strings.CutPrefix(path, "/repos/"+startedAgainRepo)
	if !ours {
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); !isRepository || len(parts) != 2 {
			return false
		}
		return g.write(w, map[string]string{"full_name": other, "default_branch": "main"})
	}
	switch {
	case rest == "":
		return g.write(w, map[string]string{"full_name": startedAgainRepo, "default_branch": "main"})
	case rest == "/pulls":
		return g.write(w, []any{})
	}
	numbered, isPull := strings.CutPrefix(rest, "/pulls/")
	number, tail, _ := strings.Cut(numbered, "/")
	n, err := strconv.Atoi(number)
	pull := g.pulls[n]
	if !isPull || err != nil || pull == nil {
		return false
	}
	switch tail {
	case "reviews":
		pull.reviewReads++
		return g.write(w, []any{})
	case "":
		pull.reads++
	default:
		return false
	}
	answer := map[string]any{
		"number": n, "state": "open", "merged": false, "html_url": fmt.Sprintf("https://github.com/%s/pull/%d", startedAgainRepo, n),
		"mergeable": true, "mergeable_state": "clean", "base": map[string]string{"ref": "main"},
		"head": map[string]string{"ref": claim.IssueBranch(pull.issue.key), "sha": pull.issue.head},
	}
	switch pull.answer {
	case dequeueMerged:
		answer["state"], answer["merged"], answer["merge_commit_sha"] = "closed", true, dequeueHead(7000+n)
	case dequeueUnavailable:
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"message":"Server Error"}`)
		return true
	}
	return g.write(w, answer)
}

// write encodes value as the stand-in's answer, and says it answered.
func (g *dequeueGitHub) write(w http.ResponseWriter, value any) bool {
	_ = json.NewEncoder(w).Encode(value)
	return true
}

// dequeueListener is the Envoy listener as this test's daemon reaches it: the package's
// noticeListener, which takes every publish, with the 404 the listener answers for a role topic no
// live session holds (writeRoleHolderError, packages/envoy/cmd/listener/api.go, which notify
// decodes as ErrNoHolder) for each merger's topic, counted, until the test says a session holds
// that topic (holds). It is conflict_no_takeover_live_test.go's conflictListener in a build of
// the package every test run compiles, which that live proof's is not.
type dequeueListener struct {
	notices  *noticeListener
	mu       sync.Mutex
	held     map[string]bool
	refusals map[string]int
}

// newDequeueListener starts the listener and returns it with its URL, which the daemon's
// configuration names as Envoy's.
func newDequeueListener(t *testing.T) (*dequeueListener, string) {
	t.Helper()
	l := &dequeueListener{notices: newNoticeListener(), held: map[string]bool{}, refusals: map[string]int{}}
	server := httptest.NewServer(l)
	t.Cleanup(server.Close)
	return l, server.URL
}

// unheld makes the listener answer every publish to topic as it answers one to a role no live
// session holds, until holds.
func (l *dequeueListener) unheld(topic string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[topic] = false
}

// holds is the woken session taking topic: every later publish to it is delivered.
func (l *dequeueListener) holds(topic string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[topic] = true
}

// refused is how many publishes to topic the listener refused for want of a holder.
func (l *dequeueListener) refused(topic string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refusals[topic]
}

// accepted is every publish to topic the listener delivered, oldest first.
func (l *dequeueListener) accepted(topic string) []listenerPublish {
	return l.notices.acceptedOn(topic)
}

func (l *dequeueListener) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/messages/publish" {
		l.notices.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	var published listenerPublish
	if err == nil {
		err = json.Unmarshal(body, &published)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"read the publish: %v"}`, err), http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	held, merger := l.held[published.Topic]
	refused := merger && !held
	if refused {
		l.refusals[published.Topic]++
	}
	l.mu.Unlock()
	if !refused {
		req.Body = io.NopCloser(bytes.NewReader(body))
		l.notices.ServeHTTP(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"error":"no live session holds the role","reason":"unclaimed"}`)
}
