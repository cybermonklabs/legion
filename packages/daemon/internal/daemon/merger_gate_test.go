package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A new version of an approved spec closes the tree's design gate, and what may merge is what the
// person's review of that version decides: the merger's start waits while the gate is closed and
// runs when it opens again (dispatch://INFRA-714/spec R5). The start is held, not dropped: the row
// stays on its backoff with nobody spawned, and the same row starts the merger once the approval
// lands.
func TestTheMergersStartWaitsWhileItsTreesDesignGateIsClosed(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "a reviewed root", Phase: phase.Merging, Generation: 1, Status: "retro"}
	putOutboxIssue(t, pool, records, issue)
	// The gate as a new version leaves it: version 4 with the person's approval still at 3.
	approved := 3
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approved})
	sup, runtime := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: outboxTreeStore(t, pool, issue.Tree), tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		// The deployment gates the spec step alone, the list `gates.design: root-issues` names
		// (config.DesignGate.Steps, INFRA-1272): a runner told no step gates nothing, so the
		// start it holds here is held by the step this gate's row belongs to.
		gatedSteps: config.DesignGateRootIssues.Steps(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleMerger,
		Generation: issue.Generation, Phase: phase.Merging, Task: "Merge it."}, time.Now())

	err := runner.execute(context.Background(), row)
	if !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start under a closed gate = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the gate was closed, want none", spawns)
	}

	// The person approves version 4, which opens the gate.
	approvedNow := 4
	putOutboxGate(t, pool, records, record.DesignGate{Issue: issue.Key, ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: &approvedNow})

	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("the merger's start once the gate opened = %v, want it run", err)
	}
	if spawns := len(runtime.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions once the gate opened, want the merger's one", spawns)
	}
}

// putOutboxGate records a tree's design gate.
func putOutboxGate(t *testing.T, pool *store.Pool, records record.Store, gate record.DesignGate) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutGate(context.Background(), tx, gate)
	}); err != nil {
		t.Fatalf("put the design gate of %s: %v", gate.Issue, err)
	}
}

const (
	// heldMergerSpec is the spec document of the held-merger rig's tree, and heldMergerPR and
	// heldMergerHead the number of its issue's pull request and the head the record holds it at.
	heldMergerSpec = "artifact-208"
	heldMergerPR   = 42
	heldMergerHead = "head1"
	// heldMergerTask is the merging task the rig's merger takes.
	heldMergerTask = "Merge the head the review approved."
)

// newHeldMergerRig is the wait-delivery rig with its role the merger of LEGION-208, a tree root,
// under a deployment that gates the spec (`gates.design: root-issues`) and an idle release that
// arms a backstop at each release. The root's architect registers the spec at version 1 and a
// person approves it; the issue then stands in merging with its pull request open at
// heldMergerHead, and the outbox delivers what the gate's moves queued. The gate stands open and
// the merger has no claim yet: a case puts it to work (working) and closes the gate
// (closesTheGate).
func newHeldMergerRig(t *testing.T) *waitDeliveryRig {
	t.Helper()
	r := newWaitDeliveryRigOf(t, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Retro, Generation: 1,
		Status: workflow.PhaseStatus(phase.Retro), Rank: "U", HandedOver: true, LastDispatchSeq: 1},
		claim.RoleMerger, config.DesignGateRootIssues.Steps())
	r.idleRelease()
	r.runner.backstop = testReleaseBackstop
	r.applies("register-spec-1", intake.GateRegistered{Issue: r.issue.Key, Step: record.DesignStepSpec, ArtifactID: heldMergerSpec,
		Version: 1, DispatchVersion: 1, Since: record.EventMark{Seq: 2}})
	r.applies("spec-approved-1", intake.DispatchArtifact{Key: r.issue.Key, ArtifactID: heldMergerSpec, Kind: intake.DispatchArtifactApproved,
		Version: 1, Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: 3}})
	r.movesTo(phase.Merging)
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.PutPullRequest(r.ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: r.issue.Key,
			Repo: "acme/widgets", Number: heldMergerPR, Branch: "legion/" + r.issue.Key, HeadSHA: heldMergerHead,
			Failing: []string{}, MergeState: record.MergeStateClean})
	}); err != nil {
		t.Fatalf("record the pull request of %s: %v", r.issue.Key, err)
	}
	r.drainAt(time.Now().UTC(), record.OutboxKindRoleMessage, record.OutboxKindNotice)
	return r
}

// mergerWorked puts the rig's merger to work on its merging task and ends the turn that took it.
func (r *waitDeliveryRig) mergerWorked() {
	r.t.Helper()
	r.endTurn(r.working(1, heldMergerTask).ID)
}

// closesTheGate is a new version of the spec reaching the record, which closes the tree's design
// gate over the merger in merging (workflow's putGateTold), and the outbox delivering what the
// closing queued for the tree's workers.
func (r *waitDeliveryRig) closesTheGate() {
	r.t.Helper()
	r.applies("spec-version-2", intake.DispatchArtifact{Key: r.issue.Key, ArtifactID: heldMergerSpec, Kind: intake.DispatchArtifactVersion,
		Version: 2, Mark: record.EventMark{Seq: 4}})
	if _, held := workflow.HeldByDesignGate(r.issue, claim.RoleMerger, r.treeGate()); !held {
		r.t.Fatalf("the tree's gate after the spec's version 2 = %+v, want it holding the merger", r.treeGate().Rows)
	}
	r.drainAt(time.Now().UTC(), record.OutboxKindRoleMessage, record.OutboxKindNotice)
}

// opensTheGate is a person's approval of the spec's version 2, which opens the tree's design gate.
func (r *waitDeliveryRig) opensTheGate() {
	r.t.Helper()
	r.applies("spec-approved-2", intake.DispatchArtifact{Key: r.issue.Key, ArtifactID: heldMergerSpec, Kind: intake.DispatchArtifactApproved,
		Version: 2, Actor: dispatch.Actor{Kind: "user", ID: "alice"}, Mark: record.EventMark{Seq: 5}})
	if held, holds := workflow.HeldByDesignGate(r.issue, claim.RoleMerger, r.treeGate()); holds {
		r.t.Fatalf("the tree's gate after the approval of version 2 holds the merger on %+v, want it open", held)
	}
}

// heldMergerSettlement is a CI settlement of the held-merger rig's pull request at its head, every
// check passed.
func heldMergerSettlement() intake.PullRequestChecks {
	return intake.PullRequestChecks{Repo: "acme/widgets", Number: heldMergerPR, HeadSHA: heldMergerHead, Generation: 1,
		Snapshot: heldMergerHead + "-settled", CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}}, Failing: []string{},
		Cancelled: []string{}}
}

// wantReleased asserts the rig's merger is still released and that nothing has woken it: no pod
// resume is recorded.
func (r *waitDeliveryRig) wantReleased(why string) {
	r.t.Helper()
	if got := r.machine.Claim(); !got.Released() {
		r.t.Fatalf("the merger = %s by %s, want it still released: %s", got.State, got.SuspendedBy, why)
	}
	if resumes := r.resumes(); len(resumes) != 0 {
		r.t.Fatalf("the pod resumes = %v, want none: %s", resumes, why)
	}
}

// The outbox's fence holds the merger's phase work while the design gate holds it, and nothing else
// (dispatch://INFRA-2040 R7, D14 of its plan). Each case puts the merger to work, closes the gate
// over it and drives one start of the merger through the outbox's executor:
//
//   - a start that names merging waits on its backoff, and the released merger stays asleep, until
//     the person's approval opens the gate;
//   - a CI settlement's wake, a message to its role and a message to its session, each of a merger
//     whose pod the idle release took during the hold, write a start that names no phase: it passes
//     the fence, brings the merger back and delivers the wake's task;
//   - the resume of a cancelled idle release, a start of no phase and no task, passes the fence and
//     brings the merger back in its pod;
//   - the wait's own start, written when the time of a wait the merger declared under the open gate
//     came and run after the gate closed, passes the fence and delivers the wait's task.
func TestTheDesignGateFenceHoldsTheMergersPhaseWorkAndNothingElse(t *testing.T) {
	t.Run("a start that names merging waits", func(t *testing.T) {
		r := newHeldMergerRig(t)
		r.mergerWorked()
		r.closesTheGate()
		r.releasesAt(time.Now().UTC().Add(time.Minute))
		sent := len(r.prompts())
		r.enter(2, "Merge the head the review approved again.")
		starts := entryStartRows(t, r.pool, r.issue.Key)
		if len(starts) != 1 || starts[0].request.Phase != phase.Merging {
			t.Fatalf("the start rows of %s = %+v, want the merger's one start at merging", r.issue.Key, starts)
		}
		if err := r.runner.execute(r.ctx, starts[0].row); !errors.Is(err, wait.ErrWaiting) || !strings.Contains(err.Error(), heldMergerSpec) {
			t.Fatalf("the merger's start at merging under the closed gate = %v, want it held, naming the gated document", err)
		}
		r.settled()
		r.wantReleased("the fence holds the merger's phase work")
		if got := len(r.prompts()); got != sent {
			t.Fatalf("the merger was sent %d prompts under the closed gate, want the %d it had", got, sent)
		}

		r.opensTheGate()
		if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
			t.Fatalf("the merger's start at merging once the gate opened = %v, want it run", err)
		}
		r.settled()
		if got := r.machine.Claim(); got.Released() {
			t.Fatalf("the merger after its start ran = %s by %s, want it brought back", got.State, got.SuspendedBy)
		}
	})

	for _, tc := range []struct {
		name string
		kind string
		wake func(*waitDeliveryRig)
		// says is what the wake's task says of its cause.
		says func(*waitDeliveryRig) string
	}{
		{name: "a CI settlement's wake", kind: pods.WakeChecks,
			wake: func(r *waitDeliveryRig) { r.applies("settled", heldMergerSettlement()) },
			says: func(*waitDeliveryRig) string {
				return "The wake is a CI settlement on your pull request's head: " + heldMergerHead
			}},
		{name: "a message to its role", kind: pods.WakeRoleMessage,
			wake: func(r *waitDeliveryRig) {
				r.applies("role-message", intake.ListenerWake{Kind: intake.ListenerWakeRole, Role: string(r.token),
					At: time.Now().UTC(), Report: "report-1"})
			},
			says: func(r *waitDeliveryRig) string {
				return "The wake is a message somebody published to your role topic (" + string(r.token) + ")"
			}},
		{name: "a message to its session", kind: pods.WakeAgentMessage,
			wake: func(r *waitDeliveryRig) {
				r.applies("agent-message", intake.ListenerWake{Kind: intake.ListenerWakeAgent, Session: waitingSession,
					At: time.Now().UTC(), Report: "report-2"})
			},
			says: func(*waitDeliveryRig) string {
				return "The wake is a message somebody sent to your session (" + waitingSession + ")"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newHeldMergerRig(t)
			r.mergerWorked()
			r.closesTheGate()
			released := time.Now().UTC().Add(time.Minute)
			r.releasesAt(released)

			tc.wake(r)
			if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "merger:"+tc.kind {
				t.Fatalf("the pod resumes after %s = %v, want its one wake of the merger", tc.name, resumes)
			}
			starts := entryStartRows(t, r.pool, r.issue.Key)
			if len(starts) != 1 || starts[0].request.Phase != "" || starts[0].request.Role != claim.RoleMerger {
				t.Fatalf("the start rows after %s = %+v, want the wake's one start of the merger, naming no phase", tc.name, starts)
			}
			r.drainAt(released.Add(time.Minute), record.OutboxKindSupervise)
			if _, queued := r.outboxRow(starts[0].row.ID); queued {
				t.Fatalf("the wake's start is still queued after the outbox ran it, want it passed through the fence")
			}
			r.boot()
			woken := r.takeTask()
			if says := tc.says(r); !strings.Contains(woken.Task, says) {
				t.Fatalf("the merger was sent %q, want the task of %s, saying %q", woken.Task, tc.name, says)
			}
		})
	}

	t.Run("the resume of a cancelled idle release", func(t *testing.T) {
		r := newHeldMergerRig(t)
		r.mergerWorked()
		r.closesTheGate()
		stopped := time.Now().UTC().Add(time.Minute)
		r.markRelease()
		r.drainAt(stopped, record.OutboxKindIssueBundle, record.OutboxKindSupervise)
		if got := r.machine.Claim(); !got.Released() {
			t.Fatalf("the merger after the release's stop = %s by %s, want it released", got.State, got.SuspendedBy)
		}
		if err := r.runner.cancelRelease(r.ctx, r.issue.Key, r.issue.Tree, workflow.IdleHoldBundleSave, "the case cancels the release"); err != nil {
			t.Fatalf("cancel the release: %v", err)
		}
		starts := entryStartRows(t, r.pool, r.issue.Key)
		if len(starts) != 1 || starts[0].request.Phase != "" || starts[0].request.Task != "" || starts[0].request.Role != claim.RoleMerger {
			t.Fatalf("the start rows after the cancel = %+v, want the merger's one resume: no phase and no task", starts)
		}
		rt := r.sup.deps.Runtime.(*releaseRuntime).Runtime
		resumed := len(resumesOf(rt, r.token))
		r.drainAt(stopped.Add(time.Minute), record.OutboxKindIssueRelease, record.OutboxKindSupervise)
		if _, queued := r.outboxRow(starts[0].row.ID); queued {
			t.Fatalf("the cancel's resume is still queued after the outbox ran it, want it passed through the fence")
		}
		if got := len(resumesOf(rt, r.token)); got != resumed+1 {
			t.Fatalf("the runtime resumed the merger %d times for the cancel's resume, want once", got-resumed)
		}
		r.boot()
		if got := r.machine.Claim(); got.State != supervise.StateReady && got.State != supervise.StateIdle {
			t.Fatalf("the merger after the cancel's resume = %s by %s, want it back and ready in its pod", got.State, got.SuspendedBy)
		}
	})

	t.Run("the wait's own start", func(t *testing.T) {
		r := newHeldMergerRig(t)
		taken := r.working(1, heldMergerTask)
		waited := r.declareUntil("the merge queue's run on the head", time.Now().Add(2*time.Hour))
		r.endTurn(taken.ID)
		r.drainAt(waited.WakeAt, record.OutboxKindWaitDue)
		starts := entryStartRows(t, r.pool, r.issue.Key)
		if len(starts) != 1 || starts[0].request.WaitID != waited.ID || starts[0].request.Phase != "" {
			t.Fatalf("the start rows at the wait's time = %+v, want the wait's own start, naming no phase", starts)
		}
		sent := len(r.prompts())

		r.closesTheGate()
		r.drainAt(waited.WakeAt, record.OutboxKindSupervise)
		if _, queued := r.outboxRow(starts[0].row.ID); queued {
			t.Fatalf("the wait's own start is still queued after the outbox ran it, want it passed through the fence")
		}
		prompts := r.wantPrompts(sent+1, "the wait's own task reaches the merger under the closed gate")
		wantWaitTask(t, prompts[sent], waited)
	})
}

// mergeQueueRemovalRow is the merge-queue removal row of the rig's issue the outbox holds, and
// whether it holds one.
func (r *waitDeliveryRig) mergeQueueRemovalRow() (record.OutboxRow, bool) {
	r.t.Helper()
	var id int64
	err := r.pool.QueryRow(r.ctx, `select id from outbox where issue = $1 and kind = $2`, r.issue.Key,
		string(record.OutboxKindMergeQueueRemoval)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return record.OutboxRow{}, false
	}
	if err != nil {
		r.t.Fatalf("read the merge-queue removal of %s: %v", r.issue.Key, err)
	}
	return r.outboxRow(id)
}

// removalsPublished is every publish of the merge-queue removal row id to the merger's topic.
func (r *waitDeliveryRig) removalsPublished(id int64) []outboxPublish {
	var published []outboxPublish
	for _, publish := range r.runner.notices.(*outboxPublisher).publishes() {
		if publish.topic == notify.RoleTopicPrefix+string(r.token) && publish.key == record.OutboxKey(id) {
			published = append(published, publish)
		}
	}
	return published
}

// A removal from the merge queue during the hold leaves its message to a released merger waiting,
// and the message wakes the merger after the opening (dispatch://INFRA-2040 R7): the row keeps its
// own wait for the gate (mergerHeld), apart from the fence's pass for a start that names no phase,
// since a merger woken by the message during the hold could enqueue the pull request again against
// the gate. The merger works, the gate closes over it and the idle release takes its pod; then
// GitHub's removal of the pull request at its head is told to the merger's role. While the gate is
// closed, each run of the row waits, naming the gated document, wakes nothing and publishes
// nothing. Once the person approves the spec's version 2, the row wakes the merger, and once the
// relaunched session holds the role the message is published to it once.
func TestAMergeQueueRemovalDuringTheHoldWaitsForTheDesignGateToOpen(t *testing.T) {
	r := newHeldMergerRig(t)
	r.mergerWorked()
	r.closesTheGate()
	released := time.Now().UTC().Add(time.Minute)
	r.releasesAt(released)
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.Enqueue(r.ctx, tx, mustOutboxRow(t, r.issue.Key,
			record.MergeQueueRemoval{Number: heldMergerPR, Head: heldMergerHead}, time.Now()))
	}); err != nil {
		t.Fatalf("queue the merge-queue removal: %v", err)
	}
	removal, _ := r.mergeQueueRemovalRow()

	for _, at := range []time.Time{released.Add(time.Minute), released.Add(time.Hour)} {
		r.drainAt(at, record.OutboxKindMergeQueueRemoval)
		r.settled()
		r.wantReleased("the removal's message wakes no merger the gate holds")
		if published := r.removalsPublished(removal.ID); len(published) != 0 {
			t.Fatalf("the removal was published %+v during the hold, want nothing", published)
		}
		row, queued := r.mergeQueueRemovalRow()
		if !queued || !strings.Contains(row.LastError, "design gate") || !strings.Contains(row.LastError, heldMergerSpec) {
			t.Fatalf("the removal's row at %s = %+v (queued %t), want it waiting for the design gate", at, row, queued)
		}
	}

	r.opensTheGate()
	opened := released.Add(2 * time.Hour)
	r.drainAt(opened, record.OutboxKindMergeQueueRemoval)
	if resumes := r.resumes(); len(resumes) == 0 {
		t.Fatal("the pod resumes after the opening = none, want the removal's message to have woken the merger")
	}
	r.boot()
	r.drainAt(opened.Add(time.Hour), record.OutboxKindMergeQueueRemoval)
	if _, queued := r.mergeQueueRemovalRow(); queued {
		t.Fatal("the removal's row is still queued once the woken merger holds its role, want it delivered")
	}
	published := r.removalsPublished(removal.ID)
	want := record.MergeQueueRemoval{Number: heldMergerPR, Head: heldMergerHead}.Message(r.issue.Key, "acme/widgets")
	if len(published) != 1 || published[0].message != want {
		t.Fatalf("the removal was published %+v, want it told to the woken merger once: %q", published, want)
	}
}
