package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// releaseRecordRefused is the real release store whose record of a stopped claim on the release's
// mark (ReleaseSuspendedClaim) fails while err is set. The stop's row then fails after its
// suspension, as it does when that record's transaction fails or the daemon stops before it, and
// the row stays queued for its retry. Every other call reaches the real store.
type releaseRecordRefused struct {
	outboxTrees
	err error
}

func (s *releaseRecordRefused) ReleaseSuspendedClaim(ctx context.Context, project, issue string, role claim.Role, row int64) error {
	if s.err != nil {
		return s.err
	}
	return s.outboxTrees.ReleaseSuspendedClaim(ctx, project, issue, role, row)
}

// queueRoleMessageWake takes a role message's wake of role while the release stands and queues
// the start the workflow's wake queues for it (workflow's wakeRow): the wake's kind and detail,
// and WakeTask for a session the release stopped. It answers that start's task.
func (r *idleRig) queueRoleMessageWake(t *testing.T, role claim.Role) string {
	t.Helper()
	ctx := context.Background()
	token := mustClaimToken(t, r.issue.Key, role)
	if taken, err := r.st.TakePodWake(ctx, "legion", r.issue.Key, token, pods.WakeRoleMessage, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the role message's wake of the %s = %t, %v; want it taken", role, taken, err)
	}
	task := workflow.WakeTask(pods.WakeRoleMessage, string(token), workflow.StoppedByRelease)
	enqueueOutbox(t, r.pool, r.issues, mustOutboxRow(t, r.issue.Key, record.SuperviseRequest{Op: "start", Tree: r.issue.Tree,
		Role: role, Generation: 1, Task: task, WakeKind: pods.WakeRoleMessage, WakeDetail: string(token)}, time.Now().UTC()))
	return task
}

// startsOf is the task of each start of role the outbox holds for the rig's issue, in id order.
func (r *idleRig) startsOf(t *testing.T, role claim.Role) []string {
	t.Helper()
	var tasks []string
	for _, start := range startRequestsOf(t, r.pool, r.issue.Key) {
		if start.Role == role {
			tasks = append(tasks, start.Task)
		}
	}
	return tasks
}

// runRow runs the rig issue's one outbox row of kind, and of the supervise op where op is not "",
// as the outbox's lane runs it.
func (r *idleRig) runRow(t *testing.T, kind, op string) {
	t.Helper()
	ctx := context.Background()
	var row record.OutboxRow
	if err := r.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = $2
		and ($3 = '' or payload->>'op' = $3)`, r.issue.Key, kind, op).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		t.Fatalf("read the %s %s row: %v", kind, op, err)
	}
	if err := r.runner.execute(ctx, row); err != nil {
		t.Fatalf("run the %s %s row: %v", kind, op, err)
	}
}

// wakePair is what one resume of a role pairs with: the release it names (pod_resumes.release_id)
// and the pause that holds it (pod_resumes.pause_id), each 0 for none.
type wakePair struct{ release, pause int64 }

// wakePairs is what each resume of role pairs with, for the rig's issue, in id order.
func (r *idleRig) wakePairs(t *testing.T, role claim.Role) []wakePair {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `select release_id, coalesce(pause_id, 0) from pod_resumes
		where issue = $1 and role = $2 order by id`, r.issue.Key, string(role))
	if err != nil {
		t.Fatalf("read the resumes of the %s: %v", role, err)
	}
	defer rows.Close()
	var pairs []wakePair
	for rows.Next() {
		var pair wakePair
		if err := rows.Scan(&pair.release, &pair.pause); err != nil {
			t.Fatalf("read a resume of the %s: %v", role, err)
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the resumes of the %s: %v", role, err)
	}
	return pairs
}

// R4 of dispatch://INFRA-3589, the woken role's wake text is true. A cancel tells the wake's start
// of each role its release stopped and no cancel resumes in the words of a release that did not
// finish, and leaves the start of a role the release did not stop as it was.
//
// The first row is the test. The release's stop suspends the implementer as `released`, and its
// record of that on the mark fails (ReleaseSuspendedClaim), so the mark names no role and
// the stop's row stays queued. A role message wakes the implementer, and its start says that the
// pod was released. The stop's retry reads that wake as a wake of a role outside the mark's set,
// so the release is overtaken and cancelled. The pod never went away, so the start must say that
// the release did not finish, with no word of a pod that went away. No cancel resumes a claim its
// mark does not name while a start of it is queued, so that start is the implementer's one start,
// and it brings the role back in the same pod. An earlier release of the pod is recorded, so the
// wake's resume names that release. This release took no pod, so the cancel keeps the resume and
// makes it name release 0. Before the cancel, the test reads the start's words of a released pod
// and the resume's earlier release, so each check after the cancel can fail.
//
// The second row is the control. An earlier release left the architect `released`, and this
// release did not stop it: its claim is no claim this release suspended, and its resume is no
// resume of this release. A role message wakes it while the release stands, which cancels the
// release. That earlier release did take the architect's pod, so its start's words that its pod
// was released are true, and the cancel leaves that start's task exactly as it was.
func TestACancelTellsTheWakeOfEachRoleItsReleaseStoppedInTheCancelsWords(t *testing.T) {
	t.Parallel()
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. "
	t.Run("a role the release stopped and its mark does not record", func(t *testing.T) {
		rig := newIdleRig(t)
		ctx := context.Background()
		// An earlier release of the issue's pod, recorded before this release: a wake taken while
		// this release stands pairs with it (store.LatestPodRelease).
		earlier, _, err := rig.st.RecordPodRelease(ctx, store.PodRelease{Project: "legion", Issue: rig.issue.Key, Generation: 1,
			Roles: []string{string(claim.RoleImplementer)}, At: time.Now().UTC().Add(-2 * time.Hour)})
		if err != nil {
			t.Fatalf("record an earlier release of the issue's pod: %v", err)
		}
		refused := &releaseRecordRefused{outboxTrees: rig.st, err: errors.New("the record of the stopped claim did not commit")}
		rig.runner.trees = refused
		rig.w.releaseIdlePod(ctx, rig.issue.Key)
		holdTeardownBack(t, rig)
		rig.drain(t)
		var suspended []string
		if err := rig.pool.QueryRow(ctx, `select suspended from issue_releases where issue = $1`, rig.issue.Key).Scan(&suspended); err != nil {
			t.Fatalf("read the release's mark: %v", err)
		}
		if got := rig.machine.Claim(); !got.Released() || len(suspended) != 0 {
			t.Fatalf("the implementer = %s by %s and the mark records %v, want it released and recorded on no mark", got.State, got.SuspendedBy, suspended)
		}

		released := rig.queueRoleMessageWake(t, claim.RoleImplementer)
		// The controls of the checks after the cancel. The wake's start says that the pod was
		// released and that the message came while the pod was gone, and its resume pairs with the
		// earlier release.
		if starts := rig.startsOf(t, claim.RoleImplementer); len(starts) != 1 || !strings.Contains(starts[0], "had been released") ||
			!strings.Contains(starts[0], "while your pod was gone") {
			t.Fatalf("the implementer's starts before the cancel = %q, want its wake's one start, whose task says that the pod had been released and that the message came while the pod was gone",
				starts)
		}
		if pairs := rig.wakePairs(t, claim.RoleImplementer); !slices.Equal(pairs, []wakePair{{release: earlier}}) {
			t.Fatalf("the implementer's resumes before the cancel = %+v, want its wake's one resume, paired with the earlier release %d and held by no pause",
				pairs, earlier)
		}
		refused.err = nil
		rig.runRow(t, "supervise", "release")
		if rig.marked(t) {
			t.Fatal("the stop's retry left the release marked, want it cancelled: the wake is of a role outside the mark's set")
		}
		token := mustClaimToken(t, rig.issue.Key, claim.RoleImplementer)
		want := workflow.TakenOverWakeTask(pods.WakeRoleMessage, string(token), released)
		starts := rig.startsOf(t, claim.RoleImplementer)
		if len(starts) != 1 || starts[0] != want || !strings.HasPrefix(starts[0], cancelled) ||
			strings.Contains(starts[0], "had been released") || strings.Contains(starts[0], "while your pod was gone") {
			t.Fatalf("the implementer's starts after the cancel = %q, want its wake's one start with the task %q: the release did not finish, and the pod never went away",
				starts, want)
		}
		if pairs := rig.wakePairs(t, claim.RoleImplementer); !slices.Equal(pairs, []wakePair{{release: 0}}) {
			t.Fatalf("the implementer's resumes after the cancel = %+v, want its wake's one resume, paired with release 0 and held by no pause: the release took no pod, and the wake's start still resumes the claim",
				pairs)
		}

		resumes := len(rig.rt.CallsOf("Resume"))
		if _, err := rig.pool.Exec(ctx, `update outbox set next_at = now() where issue = $1`, rig.issue.Key); err != nil {
			t.Fatalf("make the issue's rows due: %v", err)
		}
		rig.drain(t)
		if got := rig.machine.Claim(); got.Released() {
			t.Fatalf("the implementer after the drain = %s by %s, want its wake's start to have brought it back", got.State, got.SuspendedBy)
		}
		if n := len(rig.rt.CallsOf("Resume")) - resumes; n != 1 {
			t.Fatalf("the implementer was resumed %d times after the cancel, want once", n)
		}
	})
	t.Run("a role the release did not stop", func(t *testing.T) {
		rig := newIdleRig(t)
		ctx := context.Background()
		rig.releasedArchitect(t)
		rig.w.releaseIdlePod(ctx, rig.issue.Key)
		holdTeardownBack(t, rig)
		rig.drain(t)
		if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
			t.Fatalf("the implementer = %s by %s and the mark stands = %t, want it released under a release still running",
				got.State, got.SuspendedBy, rig.marked(t))
		}

		released := rig.queueRoleMessageWake(t, claim.RoleArchitect)
		rig.runRow(t, "issue_release", "")
		if rig.marked(t) {
			t.Fatal("the teardown left the release marked, want it cancelled: the architect's wake is of a role outside the mark's set")
		}
		if starts := rig.startsOf(t, claim.RoleArchitect); len(starts) != 1 || starts[0] != released {
			t.Fatalf("the architect's starts after the cancel = %q, want its wake's one start with its task as it was, %q: the earlier release did take its pod",
				starts, released)
		}
		if starts := rig.startsOf(t, claim.RoleImplementer); len(starts) != 1 || starts[0] != "" {
			t.Fatalf("the implementer's starts after the cancel = %q, want the cancel's one resume, with no task", starts)
		}
	})
}

// A cancel resumes the claims its own release suspended and no others, and the resume is a row
// of the issue's lane rather than a relaunch held in memory: a release that did not finish is
// undone by one transaction that queues those resumes and deletes the mark together.
//
// The pod here holds an architect an earlier release left released, which no wake has brought
// back. Resuming it would put a role into a pod that has no work for it, with no wake and no
// task saying why it is there.
func TestACancelResumesOnlyTheRolesItsOwnReleaseSuspended(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	architect := rig.releasedArchitect(t)

	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePod(ctx, rig.issue.Key) })
	if want := []string{"issue_bundle", "supervise:release", "issue_release"}; !slices.Equal(queued, want) {
		t.Fatalf("queued rows = %v, want the workspace check, the implementer's stop and the teardown: the architect is already released", queued)
	}
	rig.rt.beforeAuthorize = func() {
		if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
			t.Errorf("move the issue's generation on: %v", err)
		}
	}

	rig.drain(t)

	if got := architect.Claim(); !got.Released() {
		t.Fatalf("the architect = %s by %s, want the release that never touched it to have left it released",
			got.State, got.SuspendedBy)
	}
	if got := rig.machine.Claim(); got.State == supervise.StateSuspended {
		t.Fatalf("the implementer = %s by %s, want the claim this release suspended resumed", got.State, got.SuspendedBy)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
}

// A release can be cancelled after a wake it lets run: a CI settlement woke the implementer after
// the release recorded its claim as released, and then the issue's run moved on, so the teardown
// cancels the release. The pod never went away, so the wake's resume and its start go, and the
// cancel's own resume is the role's one start (store.CancelIssueRelease). A CI settlement reaches
// its woken role only through the wake's task, so that start carries the settlement's text under
// a preamble that says the release did not finish, built by the outbox's own builder
// (releaseResume), and nothing in it says that the pod was released.
func TestACancelAfterASettlementWakeTellsTheRoleTheSettlement(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}

	// The settlement's wake, as the workflow takes it (wakeReleased): the resume, and the start
	// that carries the wake's task and the wake's kind (workflow's wakeRow).
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	wake, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the wake's start: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rig.issue.Key, string(wake.Payload)); err != nil {
		t.Fatalf("queue the wake's start: %v", err)
	}

	// The issue's run moves on, so the teardown's store read cancels the release, and the
	// outbox carries the cancel out.
	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	starts := startRequestsOf(t, rig.pool, rig.issue.Key)
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	const settled = "The wake is a CI settlement on your pull request's head: head1 failed build. Read the checks before you decide anything."
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != cancelled+settled {
		t.Fatalf("queued starts = %+v, want the cancel's one start of the implementer with the task %q: the settlement reaches the role only through it, and the pod never went away",
			starts, cancelled+settled)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes = %+v, want none: the release was cancelled, so no released pod came back", records.Resumes)
	}
}

// The same wake and cancel while the project is paused (decision 12 of dispatch://INFRA-3759): the
// CI settlement woke the implementer as a held wake of the pause, after the release recorded its
// claim, and then the issue's run moved on, so the teardown cancels the release. The cancel keeps
// that wake as the launch the pause owes the role, and the release took no pod. So the kept start's
// task is the task of a wake of a role the pause stopped (workflow.PausedWakeTask, which the
// outbox's cancel hands the store): the settlement's text under the pause's words, and no text
// that says the pod was released. The woken role's wake text is true (dispatch://INFRA-3589).
func TestACancelWhileThePausedProjectTellsTheKeptWakeThatThePauseStoppedTheRole(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	pause := openLegionPause(t, rig.pool, time.Now().UTC())

	// The settlement's wake, as the workflow takes it while the project is paused (wakeReleased):
	// a held wake of the pause, and its start, whose task WakeTask wrote for a session the release
	// stopped.
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	const detail = "head1 failed build"
	wake, err := record.NewOutboxRow(rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks, WakeDetail: detail, Pause: pause}, time.Now().UTC())
	if err != nil {
		t.Fatalf("encode the wake's start: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '')`, string(wake.Kind), rig.issue.Key, string(wake.Payload)); err != nil {
		t.Fatalf("queue the wake's start: %v", err)
	}

	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	starts := startRequestsOf(t, rig.pool, rig.issue.Key)
	want := workflow.WakeTask(pods.WakeChecks, detail, workflow.StoppedByPause)
	if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || starts[0].Task != want ||
		starts[0].WakeKind != pods.WakeChecks || starts[0].Pause != pause {
		t.Fatalf("queued starts = %+v, want the kept wake's one start of the implementer, of the %s wake held by pause %d, with the task %q: the release took no pod, and the pause owes the role its launch",
			starts, pods.WakeChecks, pause, want)
	}
	if _, held, open, err := rig.st.OpenPodResume(ctx, "legion", rig.issue.Key, claim.RoleImplementer); err != nil || !open || held != pause {
		t.Fatalf("the implementer's open resume = pause %d, open %t, %v; want the kept wake, held by pause %d", held, open, err, pause)
	}
}

// A cancel after a CI settlement's wake, as in TestACancelAfterASettlementWakeTellsTheRoleTheSettlement,
// with a production check's look of the implementer queued beside the wake's start (the reviewer's
// read-ahead of dispatch://INFRA-3589, finding 2). The look names no phase and waits for no slot,
// as the wake's start does, but it carries no wake's kind (record.SuperviseRequest.WakeKind): it
// is work the pod was given, not a wake. So the cancel deletes the wake's resume and the wake's
// start and keeps the look as it was queued. The cancel's own start tells the role the
// settlement, and nothing of the look's task.
func TestACancelAfterASettlementWakeKeepsACheckLookQueuedForTheRole(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	holdTeardownBack(t, rig)
	rig.drain(t)
	if got := rig.machine.Claim(); !got.Released() || !rig.marked(t) {
		t.Fatalf("the claim = %s by %s and the mark stands = %t, want it released under a release still running",
			got.State, got.SuspendedBy, rig.marked(t))
	}
	if taken, err := rig.st.TakePodWake(ctx, "legion", rig.issue.Key, rig.token, pods.WakeChecks, time.Now().UTC(), 0); err != nil || !taken {
		t.Fatalf("take the settlement's wake = %t, %v; want it taken", taken, err)
	}
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease),
		WakeKind: pods.WakeChecks, WakeDetail: "head1 failed build"}, time.Now().UTC()))
	const lookTask = "Production check of LEGION-208: the look the merge's deploy woke."
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	enqueueOutbox(t, rig.pool, rig.issues, mustOutboxRow(t, rig.issue.Key, record.SuperviseRequest{Op: "start", Tree: rig.issue.Tree,
		Role: claim.RoleImplementer, Generation: 1, Task: lookTask, CheckLook: 1, CheckOpenedAt: opened}, time.Now().UTC()))

	if _, err := rig.pool.Exec(ctx, `update issues set generation = 2 where key = $1`, rig.issue.Key); err != nil {
		t.Fatalf("move the issue's generation on: %v", err)
	}
	var teardown record.OutboxRow
	if err := rig.pool.QueryRow(ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'issue_release'`,
		rig.issue.Key).Scan(&teardown.ID, &teardown.Kind, &teardown.Issue, &teardown.Payload); err != nil {
		t.Fatalf("read the teardown row: %v", err)
	}
	if err := rig.runner.execute(ctx, teardown); err != nil {
		t.Fatalf("run the teardown: %v", err)
	}
	if rig.marked(t) {
		t.Fatal("the teardown left the release marked, want it cancelled")
	}

	starts := startRequestsOf(t, rig.pool, rig.issue.Key)
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	const settled = "The wake is a CI settlement on your pull request's head: head1 failed build. Read the checks before you decide anything."
	if len(starts) != 2 || starts[0].CheckLook != 1 || !starts[0].CheckOpenedAt.Equal(opened) || starts[0].Task != lookTask ||
		starts[1].Role != claim.RoleImplementer || starts[1].CheckLook != 0 || starts[1].Task != cancelled+settled {
		t.Fatalf("queued starts = %+v, want the check's look as it was queued and then the cancel's start of the implementer with the task %q: the look is no wake's start",
			starts, cancelled+settled)
	}
	if records := rig.records(t); len(records.Resumes) != 0 {
		t.Fatalf("resumes = %+v, want none: the release was cancelled, so no released pod came back", records.Resumes)
	}
}

// A release cancelled because its issue left the workflow queues no resume, and the issue's
// close then deletes its Sandbox, which is what takes the pod's role Secrets — its launcher
// tokens and its `gh` credentials — with it.
//
// The sequence is the one the lane runs. The release's workspace check and role stop have run, so
// the implementer is suspended as `released`; the root is parked under the teardown's pod lock, so
// the linger's own rows — the role suspends and the issue_suspend that deletes the Sandbox —
// are queued above the release's; the teardown then reads the issue as no longer the workflow's
// and cancels. A resume queued for the claim the release suspended would land above the close's
// rows and supersede them: store.IssueSuspension would read a start newer than its own row,
// finish the close without acting, and leave the roles of a parked issue running in a Sandbox
// nothing deletes.
func TestACancelOfAnIssueThatLeftTheWorkflowLetsItsCloseDeleteTheSandbox(t *testing.T) {
	t.Parallel()
	rig := newIdleRig(t)
	ctx := context.Background()
	engine := workflow.New(rig.issues, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour}, quietLogger())
	// The cancel names the roles it queued a resume for, which is the one place that set is
	// read after the fact: a resume the lane has already run leaves no row behind.
	logs := &syncBuffer{}
	rig.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	rig.runner.handlers = []intake.Handler{engine}
	rig.w.releaseIdlePod(ctx, rig.issue.Key)

	// Under the teardown's pod lock, before the store is asked: a person parks the root, which
	// lingers its tree and queues the close's own rows. The close's SuspendIssue runs through
	// the same hook, and parks nothing a second time.
	var parked sync.Once
	rig.rt.beforeAuthorize = func() {
		parked.Do(func() {
			if _, err := intake.ApplyFact(ctx, rig.pool, "dispatch", "park", intake.DispatchIssue{
				Key: rig.issue.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "backlog", Title: rig.issue.Title, Rank: rig.issue.Rank}, engine); err != nil {
				t.Errorf("park the root: %v", err)
			}
		})
	}

	rig.drain(t)

	if !strings.Contains(logs.String(), "resumed=[]") {
		t.Fatalf("the cancel queued a resume; want none, since the claims of a parked issue are its close's and a start above the close's rows supersedes them: %s",
			logs.String())
	}
	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the close's one: its role Secrets go with the Sandbox", suspended)
	}
	if asked := rig.rt.teardownsAsked(); !slices.Equal(asked, []string{"ReleaseIssue", "SuspendIssue"}) {
		t.Fatalf("teardowns asked = %v, want the release's, which the store refused, and then the close's", asked)
	}
	if rig.marked(t) {
		t.Fatal("the cancelled release left its mark behind")
	}
	var left int
	if err := rig.pool.QueryRow(ctx, `select count(*) from outbox where issue = $1
		and kind in ('supervise', 'issue_suspend', 'issue_release')`, rig.issue.Key).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d stop rows of the pod remain, want the release's and the close's every one finished", left)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateSuspended || got.SuspendedBy != supervise.SuspendedByDaemon {
		t.Fatalf("the implementer = %s by %s, want it suspended by the close that took its pod",
			got.State, got.SuspendedBy)
	}
}
