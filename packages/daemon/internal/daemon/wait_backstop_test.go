package daemon

// The idle release's backstop and a phase worker's wait or hold (dispatch://INFRA-2040 R1 and
// R3): a release arms no backstop for a worker whose wait stands, since the wait's time wakes it
// instead, and drops the one an earlier release left; a held merger's backstop is armed as any
// release arms one; and the idle release takes the pod of a worker whose wait stands. Each case
// runs a real release through the outbox's executor over a real Postgres and the runtime fake.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseBackstop is the backstop_minutes the releases below arm a backstop for.
const releaseBackstop = time.Hour

// armedBackstop is one backstop row of an issue that has not fired, as the release armed it.
type armedBackstop struct {
	role  claim.Role
	dueAt time.Time
}

// openBackstops is every backstop of issue that has not fired, oldest first: what a cancel leaves
// (store.CancelPodBackstops deletes the ones it cancels).
func openBackstops(t *testing.T, pool *store.Pool, issue string) []armedBackstop {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select role, due_at from pod_wake_backstops
		where issue = $1 and not fired order by id`, issue)
	if err != nil {
		t.Fatalf("read the backstops of %s: %v", issue, err)
	}
	defer rows.Close()
	var armed []armedBackstop
	for rows.Next() {
		var backstop armedBackstop
		if err := rows.Scan(&backstop.role, &backstop.dueAt); err != nil {
			t.Fatalf("read a backstop of %s: %v", issue, err)
		}
		armed = append(armed, backstop)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the backstops of %s: %v", issue, err)
	}
	return armed
}

// releasesAt runs one idle release of the tester's pod to its end at at: its mark and rows
// (markRelease), then the save, the tester's stop and the teardown, which arms the backstop.
func (r *waitDeliveryRig) releasesAt(at time.Time) {
	r.t.Helper()
	r.markRelease()
	r.drainAt(at, record.OutboxKindIssueBundle, record.OutboxKindSupervise, record.OutboxKindIssueRelease)
	if got := r.machine.Claim(); !got.Released() {
		r.t.Fatalf("the tester = %s by %s after the release, want it released", got.State, got.SuspendedBy)
	}
	if r.releaseMarked() {
		r.t.Fatal("the release is still in progress after its rows ran, want it over")
	}
}

// backstopsFired applies every backstop of the project due at at, as the wake tick does
// (fireDueBackstops), and answers the tester's pod resumes after it.
func (r *waitDeliveryRig) backstopsFired(at time.Time) []string {
	r.t.Helper()
	r.w.fireDueBackstops(r.ctx, at)
	return r.resumes()
}

// A release arms no backstop for a worker whose wait stands, and leaves none an earlier release
// armed (R3: no wake from the idle backstop while a wait stands). The tester's first pod is
// released while it has no wait, which arms its backstop; a start row then brings it back with a
// new task, and it still holds that backstop row, which nothing took. It declares a wait in that
// task's turn, and its pod is released again: the release cancels the issue's older backstops
// before it arms anything, and arms none for the waiting tester, so no backstop row is left and
// none wakes it at any time during the wait. The wait's time wakes it instead, which ends the wait;
// and the next release, the wait over, arms its backstop as before.
func TestAReleaseArmsNoBackstopForAWaitingWorkerAndLeavesNoneItHeld(t *testing.T) {
	r := newWaitDeliveryRig(t)
	r.idleRelease()
	r.runner.backstop = releaseBackstop
	taken := r.working(1, "Test it.")
	r.endTurn(taken.ID)

	// Each release is drained a minute ahead of now, so every row its mark queued is due.
	first := time.Now().UTC().Add(time.Minute)
	r.releasesAt(first)
	left := openBackstops(t, r.pool, r.issue.Key)
	if len(left) != 1 || left[0].role != claim.RoleTester {
		t.Fatalf("the backstops after the first release = %+v, want the tester's one", left)
	}

	r.enter(2, "Test the fix.")
	r.runStarts()
	r.boot()
	retest := r.takeTask()
	if got := openBackstops(t, r.pool, r.issue.Key); len(got) != 1 || !got[0].dueAt.Equal(left[0].dueAt) {
		t.Fatalf("the backstops once the start brought the tester back = %+v, want the first release's one still armed", got)
	}
	wait := r.declareUntil("the review of the head", time.Now().Add(3*time.Hour))
	r.endTurn(retest.ID)

	r.releasesAt(first.Add(time.Minute))

	for _, at := range []time.Time{left[0].dueAt, wait.WakeAt.Add(-time.Minute)} {
		if resumes := r.backstopsFired(at); len(resumes) != 0 {
			t.Fatalf("the pod resumes after the wake tick at %s = %v, want none: no backstop wakes a worker whose wait stands", at, resumes)
		}
	}
	if got := openBackstops(t, r.pool, r.issue.Key); len(got) != 0 {
		t.Fatalf("the backstops after the waiting tester's release = %+v, want none: its wait's time wakes it", got)
	}
	if lines := r.lines("arms no backstop", "claim="+string(r.token), "wait="+wait.ID); len(lines) != 1 {
		t.Fatalf("the release logged %d lines saying it armed no backstop for wait %s, want one; the log is:\n%s", len(lines), wait.ID, r.logs)
	}
	if got := r.machine.Claim(); !got.Released() {
		t.Fatalf("the tester = %s by %s during its wait, want it still released", got.State, got.SuspendedBy)
	}
	r.wantStanding()

	r.drainAt(wait.WakeAt, waitTimeKinds...)
	r.wantTimeCame(wait, "wake=true")
	if resumes := r.resumes(); len(resumes) != 1 || resumes[0] != "tester:"+pods.WakeWaitTime {
		t.Fatalf("the pod resumes at the wait's time = %v, want the one wake of its time", resumes)
	}
	r.boot()
	r.endTurn(r.takeTask().ID)

	r.releasesAt(wait.WakeAt.Add(time.Minute))
	if got, due := openBackstops(t, r.pool, r.issue.Key), wait.WakeAt.Add(time.Minute).Add(releaseBackstop); len(got) != 1 ||
		got[0].role != claim.RoleTester || got[0].dueAt.Sub(due).Abs() > time.Millisecond {
		t.Fatalf("the backstops after the release once the wait ended = %+v, want the tester's one, due a backstop after that release", got)
	}
}

// A pod whose only phase worker has a wait that stands is released after the idle period (R3:
// the idle release can release the worker's pod). The tester declares a wait in its task's turn
// and ends the turn; its session reports no background work and no reminder, and the pod has done
// nothing for longer than the idle period. The idle pass releases the pod: the wait's own row
// (wait_due) is no delivery that holds it. The release takes the pod down, leaves the wait
// standing and arms no backstop for the tester.
func TestAPodWhoseOnlyPhaseWorkerWaitsIsReleasedAfterTheIdlePeriod(t *testing.T) {
	r := newWaitDeliveryRig(t)
	rt := r.idleRelease()
	r.runner.backstop = releaseBackstop
	taken := r.working(1, "Test it.")
	wait := r.declare()
	r.endTurn(taken.ID)
	if _, err := r.pool.Exec(r.ctx, `update claims set plugin_contract = $2, activity_at = now(),
		activity_background = false, activity_kinds = '', activity_next_reminder_at = null,
		updated_at = now() - interval '30 minutes' where issue = $1`, r.issue.Key, api.DaemonAPIVersion); err != nil {
		t.Fatalf("report what the tester's session is doing: %v", err)
	}

	r.w.releaseIdlePod(r.ctx, r.issue.Key)

	if !r.releaseMarked() {
		t.Fatalf("the idle pass marked no release of the waiting tester's pod; the log is:\n%s", r.logs)
	}
	r.drainAt(time.Now().Add(time.Minute), record.OutboxKindIssueBundle, record.OutboxKindSupervise, record.OutboxKindIssueRelease)
	if _, suspended := rt.reads(); suspended != 1 {
		t.Fatalf("the runtime took the pod down %d times, want once", suspended)
	}
	if got := r.machine.Claim(); !got.Released() {
		t.Fatalf("the tester = %s by %s after the idle release, want it released", got.State, got.SuspendedBy)
	}
	if standing := r.wantStanding(); standing.ID != wait.ID {
		t.Fatalf("the standing wait is %s after the release, want %s: a pod release ends no wait", standing.ID, wait.ID)
	}
	if got := openBackstops(t, r.pool, r.issue.Key); len(got) != 0 {
		t.Fatalf("the backstops after the release = %+v, want none for a worker whose wait stands", got)
	}
}

// newHeldMergerIdleRig is the idle rig (newIdleRig) with its issue in merging and its one live
// claim the merger's, idle with the merge's task pending, under a deployment that gates the spec,
// whose row holds version 2 with version 1 approved unless open, which approves version 2: the
// closed gate holds the merger. The runner reads that gate (gatedSteps), as the daemon's outbox
// does, and arms a backstop at every release.
func newHeldMergerIdleRig(t *testing.T, open bool) *idleRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "root", Phase: phase.Merging, Generation: 1,
		Status: workflow.PhaseStatus(phase.Merging), Rank: "U", LastDispatchSeq: 1}
	putOutboxIssue(t, pool, records, issue)
	approved := 1
	if open {
		approved = 2
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutGate(ctx, tx, record.DesignGate{Issue: issue.Key, Step: record.DesignStepSpec,
			ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: &approved, EverOpened: true})
	}); err != nil {
		t.Fatalf("register the spec's gate: %v", err)
	}
	st := outboxTreeStore(t, pool, issue.Key)

	rt := &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true,
		bundle: runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "abc", Tips: []string{"abc"},
			At: time.Now().UTC(), PodUID: releasedPodUID}}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Runtime, sup.deps.Store, sup.deps.PluginContract = rt, st, api.DaemonAPIVersion
	sup.store = st

	token := mustClaimToken(t, issue.Key, claim.RoleMerger)
	locator, err := rt.Spawn(ctx, runtime.SpawnSpec{Claim: token, Role: claim.RoleMerger})
	if err != nil {
		t.Fatalf("mint a locator: %v", err)
	}
	rt.sandboxName = sandbox.SandboxName(token)
	pending := supervise.Delivery{ID: "outbox:7", Task: "merge it", Phase: phase.Merging,
		Generation: 1, QueuedAt: time.Now().UTC().Add(-time.Hour)}
	stored := supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleMerger, TreeEpoch: 1, Generation: 1, ServingGeneration: 1,
		State: supervise.StateIdle, Locator: &locator, Session: "ses-merger",
		SessionFile: "/tmp/merger.jsonl", PluginContract: api.DaemonAPIVersion, Pending: &pending}
	if err := st.PutClaimAndDelivery(ctx, stored, pending); err != nil {
		t.Fatalf("seed the idle merger: %v", err)
	}
	if _, err := sup.restore(ctx, []supervise.Claim{stored}); err != nil {
		t.Fatalf("restore the idle merger: %v", err)
	}
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("no machine for %s", token)
	}
	if _, err := pool.Exec(ctx, `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now() - interval '25 minutes', activity_background = false,
		activity_kinds = '', activity_next_reminder_at = null where issue = $1`, issue.Key); err != nil {
		t.Fatalf("backdate the pod: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into listener_reports (project, ready_at) values ($1, $2)
		on conflict (project) do update set ready_at = excluded.ready_at`, "legion", time.Now().UTC()); err != nil {
		t.Fatalf("seed the listener's report: %v", err)
	}
	runner := &outbox{
		pool: pool, records: records, project: "legion", dispatchProject: "LEGION", supervisor: sup, trees: st,
		notices: &outboxPublisher{}, log: quietLogger(), gatedSteps: config.DesignGateRootIssues.Steps(),
		backstop: releaseBackstop, now: func() time.Time { return time.Now().Add(time.Minute) },
	}
	w := &workflowRuntime{pool: pool, projectID: "legion", dispatchProject: "LEGION", log: quietLogger(),
		idleStore: st, idleRuntime: rt, idlePeriod: 10 * time.Minute}
	return &idleRig{pool: pool, st: st, issues: records, runner: runner, sup: sup, rt: rt, w: w,
		issue: issue, token: token, machine: machine}
}

// waitsUnended records a wait of the rig's merger as a declaration records one, on the run and in
// merging, in the session its claim runs: a wait row no end has been written to.
func (r *idleRig) waitsUnended(t *testing.T) record.ClaimWait {
	t.Helper()
	wait := record.ClaimWait{ID: "wait-merger", Project: "legion", Token: r.token, Issue: r.issue.Key, Role: claim.RoleMerger,
		Generation: 1, Phase: phase.Merging, Session: "ses-merger", WaitingOn: "the merge queue",
		WakeAt: time.Now().Add(2 * time.Hour), DeclaredAt: time.Now()}
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		_, err := r.issues.DeclareWait(context.Background(), tx, wait)
		return err
	}); err != nil {
		t.Fatalf("record the merger's wait: %v", err)
	}
	return wait
}

// A release of a held merger's pod arms its backstop, as any release does (R1): when that backstop
// is taken is dispatch://INFRA-3522's, and this asserts only that it is armed. The merger's spec
// is closed, so the gate holds it, and the idle release takes its pod. A held merger with a wait
// row no closing ended — a hold that began with no gate row written, as when `gates.design`
// changes across a restart — has no wait that stands under the hold (workflow.WaitStands), and is
// armed its backstop too. The control is the same merger under an open gate, whose wait stands:
// its release arms none.
func TestAReleaseOfAHeldMergersPodArmsItsBackstop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		open      bool
		waits     bool
		backstops int
	}{
		{name: "a merger the closed gate holds", backstops: 1},
		{name: "a merger the closed gate holds, with a wait row no closing ended", waits: true, backstops: 1},
		{name: "a merger under an open gate, whose wait stands", open: true, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newHeldMergerIdleRig(t, tc.open)
			if tc.waits {
				rig.waitsUnended(t)
			}

			rig.w.releaseIdlePod(ctx, rig.issue.Key)
			rig.drain(t)

			if got := rig.machine.Claim(); !got.Released() {
				t.Fatalf("the merger after the release = %s by %s, want it released", got.State, got.SuspendedBy)
			}
			armed := openBackstops(t, rig.pool, rig.issue.Key)
			if len(armed) != tc.backstops {
				t.Fatalf("the backstops after the release = %+v, want %d", armed, tc.backstops)
			}
			for _, backstop := range armed {
				if backstop.role != claim.RoleMerger {
					t.Fatalf("the backstop armed is the %s's, want the merger's: %s", backstop.role, fmt.Sprint(armed))
				}
			}
		})
	}
}
