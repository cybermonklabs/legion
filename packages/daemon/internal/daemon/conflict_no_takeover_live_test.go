//go:build e2e

// The live proof's driver for the rule a CI red follows: it takes nobody's turn over. The
// start it makes acts at once — the role the new phase belongs to is prompted while the role whose
// phase ended is still working a turn it has not ended — nothing is sent to that role's connection
// to end its turn, the role is told its phase is over by one message on its own Envoy role topic,
// and a completion the old turn reports once the issue has handed the phase out again is refused
// HANDOFF_STALE_ENTRY, where the completion of the new phase entry, in the turn that entry's task
// starts, is taken. scripts/e2e/conflict-no-takeover.sh builds this driver with `-tags e2e` and
// owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers and the real outbox lanes — the
// phase-ended lane (record.LaneRoleMessage) among them. Facts enter only as production's do:
// GitHub envelopes on the ENVOY_NOTIFICATIONS stream for the settlements, the pushes and the
// synchronizes; the daemon's own HTTP routes for each role's registration, readiness, grants and
// completions; and the daemon's worker stream for each role's turns, which this driver speaks as
// `legion worker-shim` does (shimwire frames). Every assertion is a read of GET /legion/v1/state,
// of the operator's claim listing, of what the daemon published to the Envoy listener stand-in, of
// what it sent — or did not send — to a role's own connection, or of what the fake runtime was
// asked to launch and suspend. The store is read only for the one thing no route shows: whether
// the outbox still holds the phase-ended row of step 5.
//
// The RPC carries no abort frame, and the daemon has no quiesce of a claim and no abort of a
// connection, so the proof holds the stronger fact that leaves — that the role's connection is
// sent nothing at all while the start acts, and that the start acts while that role's claim is
// still `working` on a delivery it confirmed — rather than the absence of one frame type.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is the harness's REST stand-in for the
// one pull request the proof drives (liveGitHub, in live_issue_proof_test.go), whose base branch
// requires the check `ci` (conflictBranchRules). Dispatch is the board the package's whole-daemon
// live proofs share (liveDispatch, in live_proof_test.go). Envoy's listener is
// conflictListener, the package's noticeListener with the listener's own "no live session holds the
// role" refusal (notify.ErrNoHolder) added for the topics step 5 names, which noticeListener never
// gives. The two Apps' tokens are workflowTokenRecorder's placeholders. No agent runs: every claim
// is launched by the package's fake runtime, which says it provisions each workspace in the claim's
// own pod, as the Agent Sandbox runtime does, so the daemon clones nothing from GitHub, and each
// role the proof acts as is driven over the daemon's own worker stream and HTTP routes. The issue,
// its pull request, its slot and the implementer's claim are seeded through the store, as every
// daemon test seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// conflictBase is the pull request's base branch, the repository's default, and ciRedCheck the one
// check its rulesets name — which this daemon never reads, and the stand-in answers only so a read
// nobody makes would still be served.
const (
	ciRedCheck   = "ci"
	conflictBase = "main"
)

// conflictReason is the reason the workflow gives a conflicting head, which the implementer's
// task, the architect's notice and the tester's phase-ended message all carry (workflow's
// mergeability).
const conflictReason = "the head conflicts with " + conflictBase + ": GitHub runs no checks on it; merge " + conflictBase + " forward"

// The three heads the proof drives: the implementer's seeded head, the fix it pushes for the
// tester's second assignment, and the fix it pushes while the reviewer reviews. Each red is
// settled at a head of its own, since a settlement that replays to the judgement already recorded
// changes nothing (classify.ApplyCheckSettlement).
const (
	conflictFirstHead  = "h1"
	conflictSecondHead = "h2"
	conflictThirdHead  = "h3"
)

// The two handoffs the tester writes: the one its first assignment's turn wrote, whose completion
// the fence refuses, and the one it writes for the work the second assignment's task names. They
// are two commits because the second assignment is for the head the implementer's fix pushed, so
// the verdict of the turn before it answers for nothing there.
const (
	conflictFirstTesterHandoff  = "t1"
	conflictSecondTesterHandoff = "t2"
)

// conflictNoTakeoverLive is one run of the proof: the booted daemon, the one issue it drives, the
// stand-ins the steps read the daemon's own writes back from, and the two roles the driver acts
// as over the worker stream.
type conflictNoTakeoverLive struct {
	liveIssueProof
	listener *conflictListener
	// imp and test are the implementer and the tester as their agents reach the daemon; reviewer
	// is the claim the tester's pass starts, which nothing registers, so its role topic is the one
	// with no live holder in step 5.
	imp, test *liveClaimant
	reviewer  claim.Token
}

// TestAConflictStartsTheNextRoleAtOnceAndTakesNoTurnOverLive drives the five steps of the proof
// against one booted daemon, in the order each step's state follows from the last: the tester
// working the assignment its phase handed it, the red that starts the implementer while that turn
// runs on, the message that tells the tester its phase ended, the old turn's completion refused
// and the new assignment's taken, and last a red whose phase-ended message reaches a topic no
// session holds, which finishes the row and leaves the start acting all the same.
func TestAConflictStartsTheNextRoleAtOnceAndTakesNoTurnOverLive(t *testing.T) {
	r := newConflictNoTakeoverLive(t)
	r.step("step1-the-tester-works-the-assignment-its-phase-handed-it", r.testerWorksItsAssignment)
	r.step("step2-the-red-starts-the-implementer-while-the-testers-turn-runs-on", r.redStartsAtOnce)
	r.step("step3-the-tester-is-told-its-phase-ended-on-its-own-role-topic", r.testerIsToldItsPhaseEnded)
	r.step("step4-the-old-turns-completion-is-refused-and-the-new-assignments-is-taken", r.staleEntryRefused)
	r.step("step5-a-phase-ended-message-no-session-holds-finishes-and-the-start-still-acts", r.noHolderFinishesTheRow)
}

// newConflictNoTakeoverLive boots the daemon over the script's broker with the issue already
// recorded: one root implementing on an open pull request whose base requires ci, holding its
// tree's admission slot, and its implementer's claim as a launch a previous daemon began and never
// recorded, which this boot relaunches. That last one is how the proof reaches a live implementer
// session, serving run 1, without an operator's spawn.
func newConflictNoTakeoverLive(t *testing.T) *conflictNoTakeoverLive {
	t.Helper()
	r := &conflictNoTakeoverLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/conflict-no-takeover.sh",
		conflictBase, "the issue whose CI goes red")}
	r.serveGitHub(&liveGitHub{sha: conflictFirstHead, also: conflictBranchRules()})
	r.listener = newConflictListener(t, &r.liveProof)
	r.reviewer = r.token(claim.RoleReviewer)
	r.seed()
	r.boot(r.github.url, nil)
	return r
}

// seed records the issue as seedImplementing does, on an open pull request at its first head
// whose base requires ci. The implementer's phases row is left unwritten, as a daemon that minted
// no assignment for that phase would leave it, which is why its own completions are not held to
// one (api/handoff.go: either side empty is nothing to compare); every assignment the proof turns
// on is minted live, by the starts the two completions make.
func (r *conflictNoTakeoverLive) seed() {
	r.t.Helper()
	r.seedImplementing(conflictFirstHead)
	r.note("seeded: %s implementing on pull request #%d at %s, base %s requiring [%s], its implementer mid-launch serving run 1",
		r.key, r.number, conflictFirstHead, conflictBase, ciRedCheck)
}

// silent holds that the daemon sends this role's connection nothing at all for within: the RPC
// carries no abort frame, and nothing else it could send would end that role's turn either.
func (c *liveClaimant) silent(within time.Duration) {
	t := c.p.t
	t.Helper()
	if err := c.sh.conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("set the read deadline on the %s's connection: %v", c.role, err)
	}
	line, err := c.sh.lines.ReadBytes('\n')
	var timeout net.Error
	switch {
	case errors.As(err, &timeout) && timeout.Timeout():
		return
	case err != nil:
		t.Fatalf("read the %s's connection: %v", c.role, err)
	default:
		t.Fatalf("the daemon sent the %s %q while its turn ran, want nothing at all", c.role, strings.TrimSpace(string(line)))
	}
}

// Step 1. The seeded implementer completes implementing, which hands the testing phase to the
// tester with an assignment of its own and starts it. The tester registers, takes that task and
// works it: a claim working a delivery it confirmed, which is the turn the red of step 2 must
// leave alone.
func (r *conflictNoTakeoverLive) testerWorksItsAssignment() {
	t := r.t
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp.startTurn("")
	if status, failure := r.imp.complete("implemented it", conflictFirstHead, "", ""); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", conflictFirstHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })
	r.note("read back: %s is in %s, status %s", r.key, got.Phase, got.Status)

	r.test = r.claimant(claim.RoleTester, "ses_tester")
	prompt := r.test.takeTask("Phase: testing")
	r.note("read back: the tester was launched, registered and prompted its testing task (%s)", firstLine(prompt.Message))
	working := r.d.claim(r.test.token)
	if working.State != string(supervise.StateWorking) || working.Pending == nil || working.Pending.ConfirmedAt == nil {
		t.Fatalf("the tester is %s holding %+v, want working on a confirmed delivery of its testing task", working.State, working.Pending)
	}
	r.note("read back: the tester's claim is %s on delivery %s, phase %s, confirmed at %s",
		working.State, working.Pending.ID, working.Pending.Phase, working.Pending.ConfirmedAt.UTC().Format(time.RFC3339Nano))
}

// Step 2. CI settles the tester's head red. The issue goes back to implementing and the
// implementer is prompted its fix task at once — while the tester is still working the turn it
// started in step 1, which the daemon neither ends nor asks after: its connection is sent nothing
// at all, its claim stays working on the delivery it confirmed, and its process is not suspended.
// The start waits for no turn: it acts in the drain that moved the phase.
func (r *conflictNoTakeoverLive) redStartsAtOnce() {
	t := r.t
	before := r.d.claim(r.test.token)
	if before.State != string(supervise.StateWorking) {
		t.Fatalf("the tester is %s before the red, want working: the proof turns on a turn that is running", before.State)
	}
	suspendsBefore := len(r.rt.CallsOf("Suspend"))
	r.driveConflict()
	r.note("drove the base of %s past %s, so GitHub answers its head conflicting, while the tester's turn runs", r.key, conflictFirstHead)

	// The start's own act, read where production reads it: the implementer's prompt. The read is
	// bounded by the connection's own five seconds (shim.next), so a start that waited for the
	// tester's turn to end would fail here rather than hang.
	prompt := r.imp.takeTask("the head conflicts with " + conflictBase)
	r.note("read back: the implementer was prompted its fix task while the tester's turn ran (%s)", firstLine(prompt.Message))
	got := r.issue(r.key)
	if got.Phase != phase.Implementing {
		t.Fatalf("%s is in %s after the red, want implementing", r.key, got.Phase)
	}
	r.note("read back: %s is in %s, status %s, merge state %s", r.key, got.Phase, got.Status, got.PullRequest.MergeState)

	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) || held.Pending == nil || held.Pending.ConfirmedAt == nil ||
		before.Pending == nil || held.Pending.ID != before.Pending.ID {
		t.Fatalf("the tester is %s holding %+v when the start acted, want it still working the delivery %+v it confirmed",
			held.State, held.Pending, before.Pending)
	}
	r.note("read back: when the start acted the tester was still %s on delivery %s, the one it confirmed before the red",
		held.State, held.Pending.ID)
	r.test.silent(liveSettle)
	r.note("read back over %s: the daemon sent the tester's connection nothing at all — no abort (the frame is gone), no shutdown, no prompt", liveSettle)
	after := r.d.claim(r.test.token)
	if after.State != string(supervise.StateWorking) || after.Pending == nil || after.Pending.ID != before.Pending.ID {
		t.Fatalf("the tester is %s holding %+v after the red settled, want it still working its own delivery", after.State, after.Pending)
	}
	if suspends := r.rt.CallsOf("Suspend"); len(suspends) != suspendsBefore {
		t.Fatalf("the red suspended %d claims, want none: no role is stopped by it", len(suspends)-suspendsBefore)
	}
	r.note("read back: the tester's claim is still %s, and the fake runtime was asked to suspend nothing (%d suspensions, as before the red)",
		after.State, suspendsBefore)
}

// Step 3. The tester is told, once, on its own Envoy role topic, that its phase ended: the
// phase-ended row (record.PhaseEnded) the red enqueued on the outbox's role-message lane, the red
// itself behind the `Reason:` label that marks it off from the daemon's own sentences. It is
// not the architect's checks-red notice, which the same red sends to another topic altogether.
func (r *conflictNoTakeoverLive) testerIsToldItsPhaseEnded() {
	t := r.t
	topic := notify.RoleTopicPrefix + string(r.test.token)
	var told []listenerPublish
	testwait.Eventually(t, "the phase-ended message on the tester's role topic", func() bool {
		told = r.notices.acceptedOn(topic)
		return len(told) == 1
	})
	want := record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing,
		Reason: conflictReason}.Message(r.key)
	if told[0].Message != want {
		t.Fatalf("the tester was told %q on %s, want %q", told[0].Message, topic, want)
	}
	r.note("read back: one message on %s — %s", topic, told[0].Message)
	architect := r.architectNotices(r.key, "checks-red")
	if len(architect) != 1 {
		t.Fatalf("the architect was told %d checks-red notices, want the red's one", len(architect))
	}
	r.note("read back: the architect's own topic took the red's one checks-red notice (%s), a different message on a different topic",
		architect[0].Payload)
	// Nothing repeats either of them, and nothing else reaches the tester's topic.
	time.Sleep(liveSettle)
	if again := r.notices.acceptedOn(topic); len(again) != 1 {
		t.Fatalf("the tester's topic took %d messages over %s, want the one", len(again), liveSettle)
	}
	r.note("read back over %s: the tester's topic has taken that one message and no other", liveSettle)
}

// Step 4. The implementer fixes and completes, which hands testing out again with a phase entry of
// its own. The tester's first turn is still running and still serving the entry before it, so
// the completion it reports from that turn is refused HANDOFF_STALE_ENTRY and records
// nothing, and the refusal sends it to the new task's own work: the completion of the handoff that
// task's turn writes, at a commit of its own, is taken.
func (r *conflictNoTakeoverLive) staleEntryRefused() {
	t := r.t
	r.push(conflictFirstHead, conflictSecondHead, "fix the red")
	r.note("drove the implementer's push of %s onto %s and GitHub's synchronize of #%d to it", conflictSecondHead, conflictFirstHead, r.number)
	if status, failure := r.imp.complete("fixed the red", conflictSecondHead, "", ""); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", conflictSecondHead)
	got := r.waitFor("the issue back in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })
	r.note("read back: %s is in %s again — a second testing phase, with a second assignment for the tester", r.key, got.Phase)

	// The tester's first turn has run throughout, and still serves the assignment the first testing
	// phase handed it: its completion is of that assignment, not this one.
	stale := r.d.claim(r.test.token)
	if stale.State != string(supervise.StateWorking) {
		t.Fatalf("the tester is %s, want it still working the turn it began in step 1", stale.State)
	}
	status, failure := r.test.complete("tested it", conflictFirstTesterHandoff, "pass", "")
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ENTRY" {
		t.Fatalf("the completion from the tester's old turn = %d %s %s, want 409 HANDOFF_STALE_ENTRY", status, failure.Code, failure.Error)
	}
	r.note("read back: the completion the tester reported from its first turn = 409 %s — %s", failure.Code, failure.Error)
	if after := r.issue(r.key); after.Phase != phase.Testing {
		t.Fatalf("%s is in %s after the refused completion, want testing: a refusal records nothing", r.key, after.Phase)
	}
	r.note("read back: %s is still in testing — the refusal recorded nothing", r.key)

	// The turn ends; the task of the new assignment, waiting for it, is sent. That task is work of
	// its own — the head the fix pushed — so the tester completes it at the handoff it wrote for
	// that work, never at the commit the refused completion named, and it is taken.
	r.test.endTurn()
	prompt := r.test.takeTask("Phase: testing")
	r.note("read back: the tester's first turn ended and the second assignment's task reached it (%s)", firstLine(prompt.Message))
	if status, failure := r.test.complete("tested the fix", conflictSecondTesterHandoff, "pass", ""); status != http.StatusOK {
		t.Fatalf("the completion from the tester's new turn = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.test.endTurn()
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("read back: the completion of the second assignment's own handoff (%s), in the turn its task started, was taken: %s is in %s",
		conflictSecondTesterHandoff, r.key, moved.Phase)
}

// Step 5. The reviewer's claim was started by the tester's pass and nothing registered it, so no
// session holds its role topic. A red in reviewing ends the reviewer's phase: the phase-ended row
// is published to a topic the listener answers "no live session holds the role" (notify.ErrNoHolder),
// which finishes the row rather than retrying it for ever, and the start acts all the same — the
// implementer is prompted the new fix task.
func (r *conflictNoTakeoverLive) noHolderFinishesTheRow() {
	t := r.t
	topic := notify.RoleTopicPrefix + string(r.reviewer)
	r.listener.noHolder(topic)
	r.awaitLaunch(r.reviewer)
	r.note("the reviewer's claim was launched by the tester's pass and never registered; its topic %s now answers 404 unclaimed, as the listener answers for a role no live session holds", topic)

	r.push(conflictSecondHead, conflictThirdHead, "fix the red")
	r.driveConflict()
	r.note("drove the base of %s past %s, so GitHub answers its head conflicting, while the issue is in reviewing", r.key, conflictThirdHead)

	prompt := r.imp.takeTask("the head conflicts with " + conflictBase)
	r.note("read back: the start still acted — the implementer was prompted its fix task (%s)", firstLine(prompt.Message))
	if got := r.issue(r.key); got.Phase != phase.Implementing {
		t.Fatalf("%s is in %s after the red in reviewing, want implementing", r.key, got.Phase)
	}
	refused := r.listener.refusedNoHolder(topic)
	if len(refused) != 1 {
		t.Fatalf("the listener refused %d publishes to %s for want of a holder, want the phase-ended row's one", len(refused), topic)
	}
	r.note("read back: the listener answered the phase-ended publish to %s 404 unclaimed — %s", topic, refused[0].Message)
	// The row is finished, not retried: nothing of its kind is left in the outbox, and no second
	// attempt comes over the window a retry's first backoff (one second) falls well inside.
	testwait.Eventually(t, "the phase-ended row finished", func() bool { return r.outboxRows("phase_ended") == 0 })
	time.Sleep(liveSettle)
	if again := r.listener.refusedNoHolder(topic); len(again) != 1 {
		t.Fatalf("the listener was asked %d times over %s, want the one attempt: the row is finished, not retried", len(again), liveSettle)
	}
	if rows := r.outboxRows("phase_ended"); rows != 0 {
		t.Fatalf("the outbox holds %d phase_ended rows of %s, want none: the row finished undelivered", rows, r.key)
	}
	r.note("read back over %s: one publish attempt, and the outbox holds no phase_ended row of %s — the row finished undelivered",
		liveSettle, r.key)
	if accepted := r.notices.acceptedOn(topic); len(accepted) != 0 {
		t.Fatalf("the listener took %d messages on %s, want none: no session holds it", len(accepted), topic)
	}
	r.note("read back: nothing was delivered on %s", topic)
}

// outboxRows is how many rows of kind the outbox holds for the proof's issue. A finished row is
// deleted (record.Postgres.FinishOutbox), and no route shows the table.
func (r *conflictNoTakeoverLive) outboxRows(kind string) int {
	r.t.Helper()
	var rows int
	if err := r.st.Pool().QueryRow(context.Background(),
		"select count(*) from outbox where issue = $1 and kind = $2", r.key, kind).Scan(&rows); err != nil {
		r.t.Fatalf("read the outbox rows of %s: %v", r.key, err)
	}
	return rows
}

// driveConflict makes GitHub answer the pull request's head as conflicting with its base and
// publishes the push onto that base that asks the daemon to read it (workflow's basePush, which
// queues the merge-state read askMergeStateFor). The read applies the
// intake.PullRequestMergeability the daemon makes of GitHub's answer, and a conflicting head is
// the one thing a pull request's own state still moves an issue for (workflow's mergeability).
func (r *conflictNoTakeoverLive) driveConflict() {
	r.t.Helper()
	r.github.conflicts(true)
	r.publishBasePush()
}

// conflictListener is the Envoy listener as this proof's daemon reaches it: the package's
// noticeListener, which takes every publish, with the one answer it does not give — the 404 the
// listener answers for a role topic no live session holds (writeRoleHolderError,
// packages/envoy/cmd/listener/api.go, decoded by notify's ErrNoHolder) — added for the topics a
// step names. Everything else, the architect's notices and the tester's own message included, goes
// to noticeListener, so the harness's reads of it (architectNotices) are the reads of this one.
type conflictListener struct {
	t        *testing.T
	notices  *noticeListener
	mu       sync.Mutex
	unheld   map[string]bool
	refusals map[string][]listenerPublish
}

// newConflictListener puts this listener in front of the proof's noticeListener and points the
// configuration the daemon boots with at it.
func newConflictListener(t *testing.T, proof *liveProof) *conflictListener {
	t.Helper()
	l := &conflictListener{t: t, notices: proof.notices, unheld: map[string]bool{}, refusals: map[string][]listenerPublish{}}
	server := httptest.NewServer(l)
	t.Cleanup(server.Close)
	proof.cfg.EnvoyURL = server.URL
	return l
}

// noHolder makes the listener answer every later publish to topic as it answers one to a role no
// live session holds.
func (l *conflictListener) noHolder(topic string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unheld[topic] = true
}

// refusedNoHolder is every publish the listener refused on topic for want of a holder, oldest
// first.
func (l *conflictListener) refusedNoHolder(topic string) []listenerPublish {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]listenerPublish(nil), l.refusals[topic]...)
}

func (l *conflictListener) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/messages/publish" {
		l.notices.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		l.t.Errorf("read the publish the daemon sent: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var published listenerPublish
	if err := json.Unmarshal(body, &published); err != nil {
		l.t.Errorf("decode the publish %s: %v", body, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	unheld := l.unheld[published.Topic]
	if unheld {
		l.refusals[published.Topic] = append(l.refusals[published.Topic], published)
	}
	l.mu.Unlock()
	if !unheld {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		l.notices.ServeHTTP(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if err := json.NewEncoder(w).Encode(map[string]string{
		"error": "no live session holds the role", "reason": "unclaimed",
	}); err != nil {
		l.t.Errorf("write the listener's no-holder refusal: %v", err)
	}
}

// conflictBranchRules is what this proof's GitHub answers beyond the harness's stand-in
// (liveGitHub's also): the base branch's rulesets requiring ci (requiredchecks.Required's
// /rules/branches/<base>) and the branch itself as unprotected (its /branches/<base>, which a 404
// would make an error that keeps the set last read).
func conflictBranchRules() map[string]any {
	return map[string]any{
		"/rules/branches/" + conflictBase: []map[string]any{
			{"type": "pull_request"},
			{"type": "required_status_checks", "parameters": map[string]any{
				"required_status_checks": []map[string]string{{"context": ciRedCheck}},
			}},
		},
		"/branches/" + conflictBase: map[string]any{"name": conflictBase, "protected": false},
	}
}
