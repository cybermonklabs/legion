package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// eventWaitRow drives one outbox row of runner, one run at a time, on clock, the outbox's clock
// (outbox.now): each run moves the clock to the moment the row is due and drains the outbox there.
// w is the workflow runtime whose hook the claims' machines tell each change of a claim
// (supervise.Deps.ClaimChanged, workflowRuntime.claimChanged), nil where no machine of the row's
// rig tells one; its release runs on the test's goroutine, as one pass of the daemon's loop
// (releaseChanged).
type eventWaitRow struct {
	t      *testing.T
	runner *outbox
	pool   *store.Pool
	clock  *heldClock
	w      *workflowRuntime
	id     int64
	// runs is every moment the row was run at, in order.
	runs []time.Time
}

// eventWaitState is the row as the store holds it: its attempts, when it is due, its wait's step
// and key, and whether the outbox still holds it.
type eventWaitState struct {
	attempts int
	due      time.Time
	step     int
	key      string
	queued   bool
}

func (d *eventWaitRow) state() eventWaitState {
	d.t.Helper()
	var s eventWaitState
	err := d.pool.QueryRow(context.Background(), "select attempts, next_at, wait_step, wait_for from outbox where id = $1", d.id).
		Scan(&s.attempts, &s.due, &s.step, &s.key)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventWaitState{}
	}
	if err != nil {
		d.t.Fatalf("read row %d: %v", d.id, err)
	}
	s.queued = true
	return s
}

// drain runs every row of the outbox due at the clock's moment, once (outbox.RunOnce).
func (d *eventWaitRow) drain() {
	d.t.Helper()
	if err := d.runner.RunOnce(context.Background()); err != nil {
		d.t.Fatalf("drain the outbox: %v", err)
	}
}

// run moves the clock to the row's due moment, where that is ahead, and drains the outbox there,
// and fails the test unless the drain ran the row: it ended, or its attempts rose by one.
func (d *eventWaitRow) run() eventWaitState {
	d.t.Helper()
	before := d.state()
	if !before.queued {
		d.t.Fatalf("row %d is no longer in the outbox, want it waiting", d.id)
	}
	if now := d.clock.Now(); before.due.After(now) {
		d.clock.advance(before.due.Sub(now))
	}
	d.runs = append(d.runs, d.clock.Now())
	d.drain()
	after := d.state()
	if after.queued && after.attempts != before.attempts+1 {
		d.t.Fatalf("the drain at row %d's moment left it at %d attempts, want %d: the row did not run", d.id, after.attempts, before.attempts+1)
	}
	return after
}

// waitsOn runs the row n times and fails the test unless each run left it waiting for key on the
// capped backoff of a wait (dispatch://INFRA-4131/spec#b-r-capped): its step one higher, and due
// record.RoleWaitBackoff of the step it ran at after that run. A row that reached the cap is due
// every RoleWaitBackoffMost, at most 12 runs an hour.
func (d *eventWaitRow) waitsOn(key string, n int) {
	d.t.Helper()
	step := d.state().step
	for range n {
		after := d.run()
		at := d.runs[len(d.runs)-1]
		if !after.queued {
			d.t.Fatalf("row %d left the outbox at its run at step %d, want it waiting for %s", d.id, step, key)
		}
		if after.key != key || after.step != step+1 || !after.due.Equal(at.Add(record.RoleWaitBackoff(step))) {
			d.t.Fatalf("after its run at step %d row %d waits for %q at step %d, due %s after the run; want %q at step %d, due %s after it",
				step, d.id, after.key, after.step, after.due.Sub(at), key, step+1, record.RoleWaitBackoff(step))
		}
		step++
	}
}

// waitsOnToTheCap runs the row, every run on waitsOn's terms, until two of its runs have each left
// it waiting the cap (record.RoleWaitBackoffMost), the most a wait waits, so at most 12 runs an
// hour.
func (d *eventWaitRow) waitsOnToTheCap(key string) {
	d.t.Helper()
	for capped := 0; capped < 2; {
		step := d.state().step
		d.waitsOn(key, 1)
		if record.RoleWaitBackoff(step) == record.RoleWaitBackoffMost {
			capped++
		}
	}
}

// forget takes, without releasing anything, every change of a claim the rig's hook recorded so
// far, so the release after an event is that event's alone.
func (d *eventWaitRow) forget() {
	if d.w != nil && d.w.changed.waiting() > 0 {
		_, _ = d.w.changed.take(context.Background())
	}
}

// release runs one pass of the daemon's release of the changes of claims its hook recorded
// (workflowRuntime.releaseChanged), on the test's goroutine.
func (d *eventWaitRow) release() {
	d.t.Helper()
	if d.w == nil || d.w.changed.waiting() == 0 {
		return
	}
	keys, _ := d.w.changed.take(context.Background())
	if err := d.w.outbox.releaseChanged(context.Background(), keys); err != nil {
		d.t.Fatalf("release the rows waiting for %v: %v", keys, err)
	}
}

// dueAtItsEvent raises event, which records the fact the row waits for, and fails the test unless
// that made the row due at once, its step back at 0 (record.Store.ExpediteWaits), and the first
// drain after it, at the same moment, ran the row (dispatch://INFRA-4131/spec#b-r-wait-event). An
// event that a claim's machine tells reaches the outbox through the hook's release, which runs
// here once event returns. An event written on the wall clock, as the workflow's and admission's
// are, makes due only a row whose moment is after the wall clock (ExpediteWaits moves no row that
// is due already), so a rig whose event is one keeps its clock, and with it the row's moment,
// ahead of the wall clock (E12, reservedTreeRig.start), and such an event leaves the row due at
// the wall clock's moment, behind the rig's clock. A rig's clock still behind the wall clock is
// moved to it.
func (d *eventWaitRow) dueAtItsEvent(event func()) {
	d.t.Helper()
	d.forget()
	event()
	d.release()
	if wall := time.Now().UTC(); wall.After(d.clock.Now()) {
		d.clock.advance(wall.Sub(d.clock.Now()))
	}
	got := d.state()
	if !got.queued {
		d.t.Fatalf("row %d left the outbox at its event, want it due", d.id)
	}
	if got.due.After(d.clock.Now()) || got.step != 0 {
		d.t.Fatalf("after its event row %d is due %s from now at step %d, want due at once at step 0", d.id, got.due.Sub(d.clock.Now()), got.step)
	}
	d.drain()
	if after := d.state(); after.queued && after.attempts != got.attempts+1 {
		d.t.Fatalf("the first drain after its event left row %d at %d attempts, want %d: it did not run at its event", d.id, after.attempts, got.attempts+1)
	}
}

// keepsTheOutboxBackoff runs the row n times and fails the test unless each run left it on the
// outbox's own backoff (record.RetryBackoff, by its attempts), waiting for nothing an event of the
// daemon's makes due: a wait whose end the daemon does not record. A plain failure of the row would
// store the same backoff, so it also fails the test unless logs, the runner's JSON log, holds no
// `outbox row failed` line: each run was a wait.
func (d *eventWaitRow) keepsTheOutboxBackoff(n int, logs *syncBuffer) {
	d.t.Helper()
	for range n {
		attempts := d.state().attempts
		after := d.run()
		at := d.runs[len(d.runs)-1]
		if !after.queued {
			d.t.Fatalf("row %d left the outbox at its run after %d attempts, want it waiting", d.id, attempts)
		}
		if after.key != "" || after.step != 0 || !after.due.Equal(at.Add(record.RetryBackoff(attempts))) {
			d.t.Fatalf("after its run at %d attempts row %d waits for %q at step %d, due %s after the run; want the outbox's backoff, %s, waiting for nothing",
				attempts, d.id, after.key, after.step, after.due.Sub(at), record.RetryBackoff(attempts))
		}
	}
	if failed := loggedMessages(d.t, logs.String(), "outbox row failed"); len(failed) != 0 {
		d.t.Fatalf("row %d logged %d of its runs as failed (%v), want every run a wait", d.id, len(failed), failed)
	}
}

// eventWaitHarness is one isolated database with the trees roots open, the daemon's own claim store
// (store.Store) under a supervisor on the fake runtime, the outbox on a held clock, and the
// workflow runtime whose hook every machine the supervisor makes tells of each change of its claim.
type eventWaitHarness struct {
	t       *testing.T
	ctx     context.Context
	clock   *heldClock
	pool    *store.Pool
	records record.Store
	trees   *store.Store
	sup     *supervisor
	rt      *fake.Runtime
	runner  *outbox
	w       *workflowRuntime
	logs    *syncBuffer
}

func newEventWaitHarness(t *testing.T, roots ...string) *eventWaitHarness {
	t.Helper()
	clock := microClock()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	trees := outboxTreeStore(t, pool, roots...)
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = trees
	logs := &syncBuffer{}
	runner := &outbox{pool: pool, records: records, supervisor: sup, trees: trees, tokens: outboxTokens{}, project: "legion",
		dispatchProject: "LEGION", stateDir: t.TempDir(), notices: &outboxPublisher{}, now: clock.Now, log: debugLogTo(logs),
		provision: func(_ context.Context, req workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + req.Issue}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	w := &workflowRuntime{outbox: runner, log: quietLogger(), readied: newWakeSet[claim.Token](), changed: newWakeSet[string]()}
	sup.deps.ClaimChanged = w.claimChanged
	return &eventWaitHarness{t: t, ctx: context.Background(), clock: clock, pool: pool, records: records, trees: trees, sup: sup,
		rt: rt, runner: runner, w: w, logs: logs}
}

// issue records key as the root of its own tree, at generation 1, standing in at with status.
func (h *eventWaitHarness) issue(key string, at phase.Phase, status string, tune func(*record.Issue)) record.Issue {
	h.t.Helper()
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: key, Title: key, Phase: at, Generation: 1, Status: status, Rank: "U", LastDispatchSeq: 1}
	if tune != nil {
		tune(&issue)
	}
	putOutboxIssue(h.t, h.pool, h.records, issue)
	return issue
}

// lingering records key as a root whose tree lingers after its close, at generation 1.
func (h *eventWaitHarness) lingering(key string) record.Issue {
	until := h.clock.Now().Add(time.Hour)
	return h.issue(key, phase.Done, "done", func(issue *record.Issue) { issue.LingerUntil = &until })
}

// queue writes payload's row for issue, due at, and answers the row's driver.
func (h *eventWaitHarness) queue(issue string, payload record.OutboxPayload, at time.Time) *eventWaitRow {
	h.t.Helper()
	id := queueRow(h.t, h.pool, h.records, mustOutboxRow(h.t, issue, payload, at))
	return &eventWaitRow{t: h.t, runner: h.runner, pool: h.pool, clock: h.clock, w: h.w, id: id}
}

// machine is the supervisor's machine of token.
func (h *eventWaitHarness) machine(token claim.Token) *supervise.Machine {
	h.t.Helper()
	machine, found := h.sup.Machine(token)
	if !found {
		h.t.Fatalf("no machine for %s", token)
	}
	return machine
}

// working is role's claim on issue, in tree, launched, ready and in a turn.
func (h *eventWaitHarness) working(tree, issue string, role claim.Role) (claim.Token, *supervise.Machine) {
	h.t.Helper()
	token := launchedClaim(h.t, h.sup, tree, issue, role)
	machine := h.machine(token)
	if err := machine.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		h.t.Fatalf("the turn of %s: %v", token, err)
	}
	if got := machine.Claim().State; got != supervise.StateWorking {
		h.t.Fatalf("%s after its turn started is %s, want working", token, got)
	}
	return token, machine
}

// suspended is role's claim on key, in key's own tree, suspended by by with its session kept.
func (h *eventWaitHarness) suspended(key string, role claim.Role, by supervise.SuspendSource) (claim.Token, *supervise.Machine) {
	h.t.Helper()
	token := mustClaimToken(h.t, key, role)
	machine, _, err := h.sup.Create(h.ctx, supervise.Claim{Token: token, Project: "legion", Tree: key, Issue: key, Role: role,
		Generation: 1, ServingGeneration: 1, State: supervise.StateSuspended, SuspendedBy: by,
		Session: "ses-" + string(role), SessionFile: h.t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		h.t.Fatalf("create the suspended %s: %v", token, err)
	}
	return token, machine
}

// R6: each wait of an event of the daemon's own that the plan lists (E5 to E20) makes its row wait
// for that event's key on the capped backoff, and the event makes the row due at once, at its
// first outbox run after it; each wait whose end the daemon does not record (O1 to O5) keeps the
// outbox's backoff (record.RetryBackoff) or the moment it names
// (dispatch://INFRA-4131/spec#b-r-wait-event).
func TestEventWaitListed(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		test func(t *testing.T)
	}{
		{"E5 a start waits for its issue's claim still running in another tree", eventWaitStopElsewhere},
		{"E6 a stop of a tree the issue left waits for the claim still running there", eventWaitStopInLeftTree},
		{"E7 a stop waits for the machine of its stored claim", eventWaitStopWithoutMachine},
		{"E8 a workspace removal waits for the tree's close to retire a claim", eventWaitWorkspaceRemoval},
		{"E9 a reserved tree cleanup waits for the tree's claims to retire", eventWaitTreeCleanup},
		{"E10 a close of an issue's pod waits for its role stops", eventWaitRoleStops},
		{"E11 a close of an issue's pod waits for a stored claim that may run", eventWaitStoredClaim},
		{"E12 a release's teardown waits for its workspace check", eventWaitWorkspaceCheck},
		{"E13 a child architect's first start waits for its tree to run again", eventWaitTreeRuns},
		{"E14 a child architect's first start waits for the child's branch", eventWaitBranch},
		{"E15 a start waits for its tree's next epoch", eventWaitEpoch},
		{"E15b a start after its tree's cleanup confirmed waits for the next epoch", eventWaitEpochAfterTheCleanup},
		{"E16 a retirement waits for the task on its way to the agent", eventWaitTaskSending},
		{"E17 a start waits for the operator's hold to lift", eventWaitRuleHold},
		{"E17b a start whose claim the operator holds by the time it records the start waits for the hold to lift", eventWaitRuleHoldAtTheStart},
		{"E18 an update ask waits for its claim's machine lock", eventWaitMachineBusy},
		{"E19 a notice waits behind an earlier notice to its architect", eventWaitEarlierNotice},
		{"E19b three notices behind a held one drain in three runs once it ends", eventWaitEarlierNotices},
		{"E19c a notice behind an earlier notice the listener refuses for itself runs at that end", eventWaitEarlierNoticeRefused},
		{"E20 a suspend waits for the turn its suspension is held for", eventWaitSuspendHeld},
		{"O1 a write waits for a Dispatch that serves its route", eventWaitNoRoute},
		{"O2 a claim release waits for Dispatch to apply it", eventWaitClaimRelease},
		{"O3 a fresh session waits for the Envoy listener to free its role", eventWaitForgetsForRole},
		{"O4 a notice waits for the moment an earlier notice is due", eventWaitNamedMoment},
		{"O5 a row waits out another row's GitHub rate limit", eventWaitRateLimit},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.test(t)
		})
	}
}

// E5: a start of an issue carried into a tree of its own waits while one of its claims still runs
// in its old tree (stopElsewhere), for that claim's change; the turn's end lands the suspension the
// start asked for, which makes the start due at once.
func eventWaitStopElsewhere(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208", "LEGION-209")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	h.issue("LEGION-209", phase.Implementing, "in_progress", func(issue *record.Issue) { issue.Generation, issue.HandedOver = 2, true })
	implementer, machine := h.working("LEGION-208", "LEGION-209", claim.RoleImplementer)
	start := h.queue("LEGION-209", record.SuperviseRequest{Op: "start", Tree: "LEGION-209", Role: claim.RoleArchitect, Generation: 2}, h.clock.Now())

	start.waitsOnToTheCap(record.WaitForClaim(implementer))
	start.dueAtItsEvent(func() { endTurn(h.ctx, t, machine, implementer) })
}

// E6: a suspend of the tree the issue has left waits while its claim still runs there
// (stopInLeftTree), for the claim's change; the turn's end lands the suspension.
func eventWaitStopInLeftTree(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208", "LEGION-209")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	h.issue("LEGION-209", phase.Implementing, "in_progress", func(issue *record.Issue) { issue.Generation, issue.HandedOver = 2, true })
	implementer, machine := h.working("LEGION-208", "LEGION-209", claim.RoleImplementer)
	stop := h.queue("LEGION-209", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Reason: "the tree of LEGION-208 lingers"}, h.clock.Now())

	stop.waitsOnToTheCap(record.WaitForClaim(implementer))
	stop.dueAtItsEvent(func() { endTurn(h.ctx, t, machine, implementer) })
}

// E7: a suspend that finds no machine for a stored claim that may still run waits for the claim's
// machine (stopWithoutMachine); the supervisor publishing that machine (supervisor.add) is a change
// of the claim its hook is told.
func eventWaitStopWithoutMachine(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Testing, "testing", nil)
	tester := mustClaimToken(t, "LEGION-208", claim.RoleTester)
	locator, err := h.rt.Spawn(h.ctx, runtime.SpawnSpec{Claim: tester, Role: claim.RoleTester})
	if err != nil {
		t.Fatalf("mint a locator: %v", err)
	}
	stored := supervise.Claim{Token: tester, Project: "legion", Tree: "LEGION-208", TreeEpoch: 1, Issue: "LEGION-208", Role: claim.RoleTester,
		Generation: 1, ServingGeneration: 1, State: supervise.StateWorking, Locator: &locator, Session: "ses-tester", SessionFile: "/tmp/tester.jsonl"}
	if err := h.trees.PutClaim(h.ctx, stored); err != nil {
		t.Fatalf("store the tester's claim: %v", err)
	}
	stop := h.queue("LEGION-208", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1,
		Reason: "the tester's step was stopped"}, h.clock.Now())

	stop.waitsOnToTheCap(record.WaitForClaim(tester))
	stop.dueAtItsEvent(func() {
		if _, err := h.sup.restore(h.ctx, []supervise.Claim{stored}); err != nil {
			t.Fatalf("supervise the stored tester: %v", err)
		}
	})
}

// E8: a workspace removal at the linger's expiry waits while a claim of its issue is not retired
// (removeWorkspace), for that claim's change; the tree's close retires it.
func eventWaitWorkspaceRemoval(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.lingering("LEGION-208")
	architect, machine := h.suspended("LEGION-208", claim.RoleArchitect, supervise.SuspendedByDaemon)
	removal := h.queue("LEGION-208", record.WorkspaceRemove{Tree: "LEGION-208", Linger: 1}, h.clock.Now())

	removal.waitsOnToTheCap(record.WaitForClaim(architect))
	removal.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.RequestTreeClose{Claim: architect}); err != nil {
			t.Fatalf("close the tree of %s: %v", architect, err)
		}
	})
}

// E9: a tree close that reserved its tree's cleanup waits while a claim of the tree is not retired
// (store.CleanupReservedTree), for a change of any claim of the tree; the close of that claim
// retires it.
func eventWaitTreeCleanup(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.lingering("LEGION-208")
	implementer, machine := h.suspended("LEGION-208", claim.RoleImplementer, supervise.SuspendedByDaemon)
	closing := h.queue("LEGION-208", record.SuperviseRequest{Op: "tree_close", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1, Linger: 1},
		h.clock.Now())

	closing.waitsOnToTheCap(record.WaitForTreeClaims("LEGION-208"))
	closing.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.RequestTreeClose{Claim: implementer}); err != nil {
			t.Fatalf("close the tree of %s: %v", implementer, err)
		}
	})
}

// closesIssues makes the harness's runtime one that closes an issue's pod under the store's
// authorization (closingRuntime), as a Kubernetes runtime does.
func (h *eventWaitHarness) closesIssues() {
	h.sup.deps.Runtime = &closingRuntime{Runtime: h.rt}
}

// E10: a close of an issue's pod waits while a role stop of the issue is still queued
// (store.ErrRoleStopsUnfinished), for the issue's role stops; the stop's finish makes it due.
func eventWaitRoleStops(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.closesIssues()
	h.lingering("LEGION-208")
	stopAt := h.clock.Now().Add(time.Hour)
	h.queue("LEGION-208", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1,
		Reason: "the tree of LEGION-208 lingers"}, stopAt)
	closing := h.queue("LEGION-208", record.IssueSuspend{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1}, h.clock.Now())

	closing.waitsOnToTheCap(record.WaitForRoleStops("LEGION-208"))
	closing.dueAtItsEvent(func() {
		h.clock.advance(stopAt.Sub(h.clock.Now()))
		closing.drain()
	})
}

// E11: a close of an issue's pod waits while a stored claim of the issue may still hold a process
// (store.StoredClaimWait), for that claim's change; its suspension stops it.
func eventWaitStoredClaim(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.closesIssues()
	h.lingering("LEGION-208")
	tester := launchedClaim(t, h.sup, "LEGION-208", "LEGION-208", claim.RoleTester)
	machine := h.machine(tester)
	closing := h.queue("LEGION-208", record.IssueSuspend{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1}, h.clock.Now())

	closing.waitsOnToTheCap(record.WaitForClaim(tester))
	closing.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.RequestSuspend{Claim: tester, By: supervise.SuspendedByDaemon, Reason: "the tree lingers"}); err != nil {
			t.Fatalf("suspend %s: %v", tester, err)
		}
		if got := machine.Claim().State; got != supervise.StateSuspended {
			t.Fatalf("%s after its suspend is %s, want suspended", tester, got)
		}
	})
}

// E12: a release's teardown waits while the release's own workspace check is still queued
// (store.ErrWorkspaceCheckUnfinished), for the check; the check's finish makes it due. The check's
// first authorization fails, so it is still queued when the teardown first runs, and its retry is
// put an hour out, where the event is raised.
func eventWaitWorkspaceCheck(t *testing.T) {
	ctx := context.Background()
	rig := newIdleRig(t)
	clock := &heldClock{at: time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)}
	rig.runner.now = clock.Now
	rig.runner.trees = &releaseCheckFaults{outboxTrees: rig.st, checkErr: errors.New("the check authorization transaction is unavailable")}
	rig.w.releaseIdlePods(ctx)
	if err := rig.runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the release once: %v", err)
	}
	checkAt := clock.Now().Add(time.Hour)
	var teardown int64
	if err := rig.pool.QueryRow(ctx, `with check_row as (update outbox set next_at = $2 where issue = $1 and kind = 'issue_bundle' returning id)
		select id from outbox where issue = $1 and kind = 'issue_release' and exists (select 1 from check_row)`, rig.issue.Key, checkAt).Scan(&teardown); err != nil {
		t.Fatalf("hold the check an hour out and read the waiting teardown: %v", err)
	}
	row := &eventWaitRow{t: t, runner: rig.runner, pool: rig.pool, clock: clock, id: teardown}
	if got := row.state(); got.key != record.WaitForWorkspaceCheck(rig.issue.Key) || got.step != 1 {
		t.Fatalf("the teardown after its first run waits for %q at step %d, want %q at step 1", got.key, got.step, record.WaitForWorkspaceCheck(rig.issue.Key))
	}

	row.waitsOnToTheCap(record.WaitForWorkspaceCheck(rig.issue.Key))
	row.dueAtItsEvent(func() {
		clock.advance(checkAt.Sub(clock.Now()))
		row.drain()
	})
}

// E13: the first start of a child's own architect waits while its tree lingers or its root waits
// in the line (startRoot), for the tree to run again; the root's re-admission and promotion make it
// due (admit's treeRunsAgain).
func eventWaitTreeRuns(t *testing.T) {
	rig := newChildArchitectRig(t)
	parkTheRootBeforeTheChildsRowsRun(rig)
	var id int64
	if err := rig.pool.QueryRow(rig.ctx, `select id from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'architect'`, doorChild).Scan(&id); err != nil {
		t.Fatalf("read the waiting start of the child's architect: %v", err)
	}
	clock := microClock()
	rig.runner.now = clock.Now
	start := &eventWaitRow{t: t, runner: rig.runner, pool: rig.pool, clock: clock, id: id}

	start.waitsOnToTheCap(record.WaitForTreeRuns(doorRoot))
	start.dueAtItsEvent(func() { reopenTheRoot(rig) })
}

// E14: the first start of a child's own architect in a tree that runs waits while the child's
// branch row is still queued (startRoot), for that row; its finish makes the start due.
func eventWaitBranch(t *testing.T) {
	rig := newChildArchitectRig(t)
	clock := microClock()
	rig.runner.now = clock.Now
	rig.put(rig.member(doorChild, doorRoot, phase.Planning, "in_progress", "V"))
	id := queueRow(t, rig.pool, rig.records, mustOutboxRow(t, doorChild, record.SuperviseRequest{Op: "start", Tree: doorRoot,
		Role: claim.RoleArchitect, Generation: 1}, clock.Now()))
	branchAt := clock.Now().Add(time.Hour)
	queueRow(t, rig.pool, rig.records, mustOutboxRow(t, doorChild, record.IssueBranch{Generation: 1}, branchAt))
	start := &eventWaitRow{t: t, runner: rig.runner, pool: rig.pool, clock: clock, id: id}

	start.waitsOnToTheCap(record.WaitForBranch(doorChild))
	start.dueAtItsEvent(func() {
		clock.advance(branchAt.Sub(clock.Now()))
		start.drain()
	})
	if _, found := rig.sup.Machine(mustClaimToken(t, doorChild, claim.RoleArchitect)); !found {
		t.Fatal("the start that ran once the child's branch was made created no architect claim")
	}
}

// reservedTreeRig is a root whose tree's cleanup its close reserved while it lingered at
// generation 1, then re-admitted at generation 2 by a person's todo, applied through the Dispatch
// mirror, the workflow engine and admission (apply): the re-admission's promotion waits for the
// cleanup, so the tree's next epoch is not open. confirm confirms the cleanup, and promote applies
// a fact on which admission promotes again (Admission.Apply's release of a consumer position).
type reservedTreeRig struct {
	h       *eventWaitHarness
	root    record.Issue
	epoch   uint64
	apply   func(id string, fact intake.Fact)
	confirm func()
	promote func()
}

func newReservedTreeRig(t *testing.T) *reservedTreeRig {
	t.Helper()
	h := newEventWaitHarness(t, "LEGION-208")
	root := h.lingering("LEGION-208")
	lifecycle, reserved, err := h.trees.ReserveWorkflowTreeCleanup(h.ctx, "legion", root.Key, root.Generation)
	if err != nil || !reserved {
		t.Fatalf("reserve the cleanup of the lingering tree = %t, %v; want it reserved", reserved, err)
	}
	engine := workflow.New(h.records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour}, quietLogger())
	admission := admit.New(h.records, engine, 1, "LEGION", quietLogger())
	handlers := []intake.Handler{blockers.NewMirror(h.records, "LEGION"), engine, admission}
	r := &reservedTreeRig{h: h, root: root, epoch: lifecycle.Epoch}
	r.apply = func(id string, fact intake.Fact) {
		t.Helper()
		if result, err := intake.ApplyFact(h.ctx, h.pool, "test", id, fact, handlers...); err != nil || result.Refusal != nil {
			t.Fatalf("apply %s = %#v, %v", id, result, err)
		}
	}
	r.confirm = func() {
		t.Helper()
		if err := h.trees.CleanupReservedTree(h.ctx, "legion", root.Key, lifecycle.Epoch, h.rt); err != nil {
			t.Fatalf("confirm the tree's cleanup: %v", err)
		}
	}
	r.promote = func() { r.apply("position", intake.DispatchConsumerPosition{}) }
	r.apply("root-todo", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		HandedOver: true, Title: root.Title, Rank: "U"})
	if readmitted := readIssue(t, h.pool, root.Key); readmitted.Generation != 2 || readmitted.Lingers() {
		t.Fatalf("the root after its todo = generation %d lingering %t, want generation 2 re-admitted", readmitted.Generation, readmitted.Lingers())
	}
	return r
}

// start queues the start of the re-admitted root's architect, at generation 2, on the rig's clock
// moved a minute ahead of the wall clock first: the promotion that makes the start due runs on
// the wall clock, and moves only a row due after it (dueAtItsEvent), so the start's every moment
// stays after the wall clock however long the rig took to set up.
func (r *reservedTreeRig) start() *eventWaitRow {
	if ahead := time.Now().UTC().Add(time.Minute); ahead.After(r.h.clock.Now()) {
		r.h.clock.advance(ahead.Sub(r.h.clock.Now()).Truncate(time.Microsecond))
	}
	return r.h.queue(r.root.Key, record.SuperviseRequest{Op: "start", Tree: r.root.Key, Role: claim.RoleArchitect, Generation: 2}, r.h.clock.Now())
}

// opened fails the test unless the promotion opened the tree's next epoch.
func (r *reservedTreeRig) opened(t *testing.T) {
	t.Helper()
	var epoch int64
	if err := r.h.pool.QueryRow(r.h.ctx, `select epoch from tree_lifecycles where project = 'legion' and tree = $1 and not cleanup_started`,
		r.root.Key).Scan(&epoch); err != nil || uint64(epoch) != r.epoch+1 {
		t.Fatalf("the tree's lifecycle after the promotion = epoch %d, %v; want its next epoch, %d, open", epoch, err, r.epoch+1)
	}
}

// E15: a start whose claim's admission meets its tree's reserved cleanup
// (treelifecycle.ErrCleanupReserved) waits for the tree's next epoch; once the cleanup confirms,
// the promotion of the re-admitted root opens that epoch, which makes the start due.
func eventWaitEpoch(t *testing.T) {
	r := newReservedTreeRig(t)
	start := r.start()

	start.waitsOnToTheCap(record.WaitForEpoch(r.root.Key))
	start.dueAtItsEvent(func() {
		r.confirm()
		r.promote()
	})
	r.opened(t)
}

// E15b: a start whose claim's admission meets its tree's cleanup confirmed and the tree's next
// epoch not opened yet (treelifecycle.ErrLifecycleClosed) waits for the same event as E15, the
// next epoch, which the promotion of the re-admitted root opens: the confirmation alone opens
// nothing (treelifecycle.ConfirmCleanup), so between it and that promotion the start is no failure
// but a wait for the epoch, and the promotion makes it due.
func eventWaitEpochAfterTheCleanup(t *testing.T) {
	r := newReservedTreeRig(t)
	r.confirm()
	start := r.start()

	start.waitsOnToTheCap(record.WaitForEpoch(r.root.Key))
	start.dueAtItsEvent(r.promote)
	r.opened(t)
}

// E16: a retirement of a stopped phase's task waits while that task's prompt is on its way to the
// agent (supervise.ErrTaskSending), for the claim's change; the send settling ends it.
func eventWaitTaskSending(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.sup.deps.Timeouts.RPC = time.Minute
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	implementer := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	machine, _, err := h.sup.Create(h.ctx, supervise.Claim{Token: implementer, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create %s: %v", implementer, err)
	}
	conn := newGatedPrompt()
	h.sup.deps.Conns.(*fake.Conns).Register(implementer, conn)
	if err := machine.Handle(h.ctx, supervise.RequestSpawn{Claim: implementer}); err != nil {
		t.Fatalf("spawn %s: %v", implementer, err)
	}
	readyClaimAs(t, h.sup, implementer, claim.RoleImplementer, "ses-implementer")
	if err := machine.Handle(h.ctx, supervise.RequestDeliver{Claim: implementer, Task: "Implement the plan.", ID: "outbox:1",
		Phase: phase.Implementing, Generation: 1, Entry: 1}); err != nil {
		t.Fatalf("deliver the implementing task: %v", err)
	}
	testwait.Receive(t, "the implementing task's prompt to leave", conn.entered)
	retire := h.queue("LEGION-208", record.SuperviseRequest{Op: "retire_task", Tree: "LEGION-208", Role: claim.RoleImplementer,
		Generation: 1, Phase: phase.Implementing, Entry: 1}, h.clock.Now())

	retire.waitsOnToTheCap(record.WaitForClaim(implementer))
	retire.dueAtItsEvent(func() {
		close(conn.release)
		testwait.Eventually(t, "the send's end to reach the claim's hook", func() bool { return !machine.View().SendInFlight && h.w.changed.waiting() > 0 })
	})
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("the retirement that ran once the send settled left %+v pending, want the task retired", pending)
	}
}

// E17: a start of a claim the operator suspended waits for the operator's resume
// (supervise.ErrHeldByOperator), a change of the claim.
func eventWaitRuleHold(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	implementer, machine := h.suspended("LEGION-208", claim.RoleImplementer, supervise.SuspendedByOperator)
	start := h.queue("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Task: "Implement the plan."}, h.clock.Now())

	start.waitsOnToTheCap(record.WaitForClaim(implementer))
	start.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.RequestResume{Claim: implementer, By: supervise.SuspendedByOperator}); err != nil {
			t.Fatalf("the operator's resume of %s: %v", implementer, err)
		}
	})
}

// E17b: a start that read no hold of its claim and meets the operator's hold when the claim
// records the start (supervise.Machine.StartedBy answers the hold's supervise.ErrHeldByOperator)
// waits for the operator's resume as E17's does, a change of the claim. The hold lands between the
// start's read and StartedBy: the start found no machine for its claim and provisions the claim's
// workspace, the operator's suspension of a claim made meanwhile lands then, and the start's create
// of the claim answers that claim as it is (supervisor.Create).
func eventWaitRuleHoldAtTheStart(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	implementer := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	var machine *supervise.Machine
	provision := h.runner.provision
	h.runner.provision = func(ctx context.Context, req workspace.Request) (workspace.Workspace, error) {
		if machine == nil {
			_, machine = h.suspended("LEGION-208", claim.RoleImplementer, supervise.SuspendedByOperator)
		}
		return provision(ctx, req)
	}
	start := h.queue("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Task: "Implement the plan."}, h.clock.Now())

	start.waitsOn(record.WaitForClaim(implementer), 1)
	if machine == nil {
		t.Fatal("the start's first run provisioned no workspace, so no hold landed before it recorded the start")
	}
	start.waitsOnToTheCap(record.WaitForClaim(implementer))
	start.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.RequestResume{Claim: implementer, By: supervise.SuspendedByOperator}); err != nil {
			t.Fatalf("the operator's resume of %s: %v", implementer, err)
		}
	})
}

// E18: an update ask that finds its claim's machine lock held by a decision waits for that
// claim's change (supervise.ErrMachineBusy): the end of the decision tells the hook of the refusal
// (supervise.Deps.ClaimChanged) even where the decision changed nothing of the claim. The decision
// here is a turn's tool step whose write the store holds (heldStepStore), which changes nothing of
// the view, so what its end tells the hook is the refusal alone: the two views it hands match.
func eventWaitMachineBusy(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	held := newHeldStepStore(h.sup.deps.Store)
	h.sup.deps.Store = held
	t.Cleanup(held.open)
	var mu sync.Mutex
	var told [][2]supervise.ClaimView
	h.sup.deps.ClaimChanged = func(before, after supervise.ClaimView) {
		mu.Lock()
		told = append(told, [2]supervise.ClaimView{before, after})
		mu.Unlock()
		h.w.claimChanged(before, after)
	}
	implementer := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	ready := h.clock.Now().Add(-time.Minute)
	machine := roleWaitClaim(t, h.sup, supervise.Claim{Token: implementer, Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1, ServingGeneration: 1, State: supervise.StateWorking, Session: "ses-implementer",
		SessionFile: t.TempDir() + "/session.jsonl", LastReady: ready, LastTurnStart: ready.Add(30 * time.Second), UpdateRound: "pod:uid-1"})
	stepped := make(chan error, 1)
	go func() {
		stepped <- machine.Handle(h.ctx, supervise.StreamToolStep{Claim: implementer, Tool: "bash", CallID: "toolu_1"})
	}()
	testwait.Receive(t, "the tool step's write to reach the store", held.entered)
	mu.Lock()
	since := len(told)
	mu.Unlock()
	ask := h.queue("LEGION-208", record.UpdateAsk{Role: claim.RoleImplementer, Round: "pod:uid-1", Pod: "pod-1", Text: "Finish your turn for the pod update."},
		h.clock.Now())

	ask.waitsOnToTheCap(record.WaitForClaim(implementer))
	ask.dueAtItsEvent(func() {
		held.open()
		if err := testwait.Receive(t, "the tool step's decision to end", stepped); err != nil {
			t.Fatalf("the tool step of %s: %v", implementer, err)
		}
		change := testwait.Until(t, "the end of the tool step to tell the hook of the refused lock", func() ([2]supervise.ClaimView, bool) {
			mu.Lock()
			defer mu.Unlock()
			if len(told) == since {
				return [2]supervise.ClaimView{}, false
			}
			return told[since], true
		})
		if before, after := change[0], change[1]; before.State != after.State || before.Generation != after.Generation ||
			!before.LastReady.Equal(after.LastReady) || !before.LastTurnStart.Equal(after.LastTurnStart) {
			t.Fatalf("the end of the tool step told the hook of %s at launch %d becoming %s at launch %d, want views that match: the refusal alone told",
				before.State, before.Generation, after.State, after.Generation)
		}
	})
}

// heldEarlierNotice is a ready architect of LEGION-208 and a notice to it that another lane holds,
// as a lane that claimed it and is publishing it does: the harness, the notice's driver, and the
// row as that lane holds it, whose settle ends it.
func heldEarlierNotice(t *testing.T) (*eventWaitHarness, *eventWaitRow, record.OutboxRow) {
	t.Helper()
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	readyOutboxClaim(t, h.sup, "LEGION-208", claim.RoleArchitect)
	earlier := h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, h.clock.Now())
	var held record.OutboxRow
	if err := store.Transact(h.ctx, h.pool, func(ctx context.Context, tx pgx.Tx) error {
		var claimed bool
		var err error
		if held, claimed, err = h.records.ClaimRow(ctx, tx, earlier.id, h.clock.Now(), time.Hour); err == nil && !claimed {
			err = errors.New("the earlier notice could not be claimed")
		}
		return err
	}); err != nil {
		t.Fatalf("claim the earlier notice: %v", err)
	}
	return h, earlier, held
}

// E19: a notice to an architect waits behind an earlier notice to the same architect that another
// lane holds (errNoticeWaits), for that row's end; its finish makes the notice due.
func eventWaitEarlierNotice(t *testing.T) {
	h, earlier, held := heldEarlierNotice(t)
	later := h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, h.clock.Now())

	later.waitsOnToTheCap(record.WaitForRow(earlier.id))
	later.dueAtItsEvent(func() { h.runner.settle(h.ctx, held, nil) })
}

// E19b: notices queued to one architect behind one another lane holds each wait for the end of the
// newest notice ahead of it (earlierNoticeFor's latest), not of the oldest: once the held one ends,
// the end of each makes due the one right behind it and no other, so the three drain in three runs,
// one each and in order, each publishing its notice, rather than every waiting notice running
// again at each end.
func eventWaitEarlierNotices(t *testing.T) {
	h, first, held := heldEarlierNotice(t)
	var behind []*eventWaitRow
	for _, reason := range []string{"max_fix_attempts", "checks_failing", "merge_conflict"} {
		behind = append(behind, h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: reason}, h.clock.Now()))
	}
	// The three are due at the same moments, so each drain that runs the first runs them all.
	behind[0].waitsOnToTheCap(record.WaitForRow(first.id))
	waited := behind[0].state()
	for i, row := range behind[1:] {
		if got := row.state(); got.key != record.WaitForRow(behind[i].id) || got.step != waited.step || got.attempts != waited.attempts {
			t.Fatalf("notice row %d waits for %q at step %d after %d attempts, want the end of the notice right ahead of it, %q, at step %d after %d",
				row.id, got.key, got.step, got.attempts, record.WaitForRow(behind[i].id), waited.step, waited.attempts)
		}
	}

	publisher := h.runner.notices.(*outboxPublisher)
	for i, row := range behind {
		if i == 0 {
			row.dueAtItsEvent(func() { h.runner.settle(h.ctx, held, nil) })
		} else {
			if got := row.state(); !got.queued || got.due.After(h.clock.Now()) || got.step != 0 || got.attempts != waited.attempts {
				t.Fatalf("after the end of the notice ahead of it, notice row %d is due %s from now at step %d after %d attempts, want due at once at step 0 after its %d waiting runs",
					row.id, got.due.Sub(h.clock.Now()), got.step, got.attempts, waited.attempts)
			}
			row.drain()
		}
		if got := row.state(); got.queued {
			t.Fatalf("notice row %d still waits for %q after its run, want it published and ended", row.id, got.key)
		}
		if keys := publisher.keys(); len(keys) != i+1 || keys[i] != record.OutboxKey(row.id) {
			t.Fatalf("after the run of notice row %d the listener was handed %v, want the three published one at a run, in order", row.id, keys)
		}
		for j, later := range behind[i+1:] {
			got := later.state()
			if got.attempts != waited.attempts {
				t.Fatalf("notice row %d ran again at the end of row %d, want it run only at the end of the notice right ahead of it", later.id, row.id)
			}
			if j > 0 && !got.due.After(h.clock.Now()) {
				t.Fatalf("the end of notice row %d made row %d due too, want only the notice right behind it due", row.id, later.id)
			}
		}
	}
}

// E19c: a notice waits behind an earlier notice to its architect that another lane holds, as in
// E19, and the listener refuses that notice's publish for the message itself (a 400 with its own
// error, notify.ErrMessageRefused). That refusal ends the earlier row undelivered at its run
// (messageRefused, finishRefused), and the end makes the waiting notice due at once, as the earlier
// row's delivered finish does: a refused end is an end of the row it waits for (finishedWaits,
// dispatch://INFRA-4131/spec#b-r-wait-event).
func eventWaitEarlierNoticeRefused(t *testing.T) {
	h, earlier, held := heldEarlierNotice(t)
	later := h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, h.clock.Now())
	publisher := h.runner.notices.(*outboxPublisher)

	later.waitsOnToTheCap(record.WaitForRow(earlier.id))
	later.dueAtItsEvent(func() {
		// The lane that holds the earlier notice runs it and settles what it came to, as runRow
		// does, against a listener that refuses the message itself, then takes publishes again.
		publisher.refuseWith(messageRefusal(t, http.StatusBadRequest))
		h.runner.settle(h.ctx, held, h.runner.execute(h.ctx, held))
		publisher.refuseWith(nil)
		if got := earlier.state(); got.queued {
			t.Fatalf("the earlier notice row %d the listener refused for itself still waits for %q at step %d, want it ended at that run",
				earlier.id, got.key, got.step)
		}
	})
	if got := later.state(); got.queued {
		t.Fatalf("notice row %d still waits for %q after its run, want it published and ended", later.id, got.key)
	}
	if keys := publisher.keys(); len(keys) != 2 || keys[0] != record.OutboxKey(earlier.id) || keys[1] != record.OutboxKey(later.id) {
		t.Fatalf("the listener was handed %v, want the earlier notice's refused publish, then the later notice's", keys)
	}
}

// E20: a suspend of a claim in a turn waits while the suspension is held for that turn
// (supervise.ErrSuspendHeld), for the claim's change; the turn's end lands it.
func eventWaitSuspendHeld(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	implementer, machine := h.working("LEGION-208", "LEGION-208", claim.RoleImplementer)
	stop := h.queue("LEGION-208", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Reason: "the issue was parked"}, h.clock.Now())

	stop.waitsOnToTheCap(record.WaitForClaim(implementer))
	stop.dueAtItsEvent(func() { endTurn(h.ctx, t, machine, implementer) })
	if got := machine.Claim().State; got != supervise.StateSuspended {
		t.Fatalf("%s after the suspend ran again is %s, want suspended", implementer, got)
	}
}

// eventWaitBackoffRuns is how many runs an out-of-scope row is held to the outbox's backoff for:
// past the run whose wait the capped backoff would make longer (RetryBackoffMost, 64 s, against
// 128 s), so a row on the wrong one fails.
const eventWaitBackoffRuns = 9

// O1: a write of the record lane meeting a Dispatch with no route for it waits for a Dispatch
// that serves it, a deploy rather than a fact of the daemon's, on the outbox's backoff.
func eventWaitNoRoute(t *testing.T) {
	notFound := &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route for PUT /api/v1/issues/LEGION-1/…"}
	answers := make([]error, eventWaitBackoffRuns)
	for i := range answers {
		answers[i] = notFound
	}
	rig := newStepRecordRigOf(t, planRecordOf(), answersInTurn(answers...))
	row := &eventWaitRow{t: t, runner: rig.runner, pool: rig.pool, clock: rig.clock, id: onlyRowOf(t, rig.pool, stepIssue)}
	row.keepsTheOutboxBackoff(eventWaitBackoffRuns, rig.logs)
}

// O2: a claim release Dispatch answers contended waits for Dispatch to apply it, Dispatch's state
// rather than the daemon's, on the outbox's backoff.
func eventWaitClaimRelease(t *testing.T) {
	rig := newClaimReleaseRig(t, dispatch.Issue{Status: "backlog", Claim: sessionClaim(claimTreeSession)}, false,
		func(int) error { return claimConflict(dispatch.ClaimContended) })
	rig.records(parkedIssue())
	rig.due(0)
	row := &eventWaitRow{t: t, runner: rig.runner, pool: rig.pool, clock: rig.clock, id: rig.row}
	row.keepsTheOutboxBackoff(eventWaitBackoffRuns, rig.logs)
}

// O3: a start that asks for a fresh session while the Envoy listener names the recorded session
// its role's holder waits for the listener to free the role (supervise.ErrForgetWaitsForRole), the
// listener's state rather than the daemon's, on the outbox's backoff.
func eventWaitForgetsForRole(t *testing.T) {
	const taken = "ses-taken"
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	if _, err := sup.deps.Store.(*outboxClaimStore).HoldSessions(context.Background(), "legion",
		mustClaimToken(t, "LEGION-943", claim.RoleImplementer), supervise.RecordedHolds(taken, "")); err != nil {
		t.Fatalf("hold %s for the claim that recorded it first: %v", taken, err)
	}
	sup.deps.RoleHolder = func(context.Context, string) (string, string, error) { return taken, "live", nil }
	tester := mustClaimToken(t, "LEGION-208", claim.RoleTester)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: tester, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleTester, Generation: 1, State: supervise.StateFailed, Session: taken, SessionFile: "/tmp/tester.jsonl"}, ""); err != nil {
		t.Fatalf("create the failed tester: %v", err)
	}
	clock := microClock()
	logs := &syncBuffer{}
	runner := &outbox{log: debugLogTo(logs), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: clock.Now}
	id := queueRow(t, pool, records, mustOutboxRow(t, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Task: "Test it.", FreshSession: true}, clock.Now()))
	row := &eventWaitRow{t: t, runner: runner, pool: pool, clock: clock, id: id}
	row.keepsTheOutboxBackoff(eventWaitBackoffRuns, logs)
}

// O4: a notice behind an earlier notice to its architect that is due at a moment still ahead runs
// at that moment (wait.Until), once, rather than on any backoff.
func eventWaitNamedMoment(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	readyOutboxClaim(t, h.sup, "LEGION-208", claim.RoleArchitect)
	moment := h.clock.Now().Add(10 * time.Minute)
	h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, moment)
	later := h.queue("LEGION-208", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, h.clock.Now())

	got := later.run()
	if !got.queued || got.key != "" || !got.due.Equal(moment) {
		t.Fatalf("the notice behind one due in 10 minutes waits for %q, due %s from its run; want it due at that moment, %s, waiting for nothing",
			got.key, got.due.Sub(later.runs[0]), moment.Sub(later.runs[0]))
	}
}

// O5: a row of a kind that spends a GitHub budget another row met the rate limit of is deferred to
// the moment that limit ends, GitHub's rather than the daemon's.
func eventWaitRateLimit(t *testing.T) {
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	until := h.clock.Now().Add(10 * time.Minute)
	h.runner.limits = map[githubrest.Budget]rateLimit{githubrest.BudgetCore: {row: 1, err: errors.New("GitHub's rate limit"), until: until}}
	branch := h.queue("LEGION-208", record.IssueBranch{Generation: 1}, h.clock.Now())

	got := branch.run()
	if !got.queued || got.key != "" || got.step != 0 || !got.due.Equal(until) {
		t.Fatalf("the branch row deferred behind a rate limit waits for %q at step %d, due %s from its run; want it due when the limit ends, %s, waiting for nothing",
			got.key, got.step, got.due.Sub(branch.runs[0]), until.Sub(branch.runs[0]))
	}
}

// onlyRowOf is the id of the one row the outbox holds for issue.
func onlyRowOf(t *testing.T, pool *store.Pool, issue string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), "select id from outbox where issue = $1", issue).Scan(&id); err != nil {
		t.Fatalf("read the row of %s: %v", issue, err)
	}
	return id
}

// pendingDeliveryRig is a claim of LEGION-208's role, ready, holding pending, a task no turn has
// confirmed, and a start row of payload whose task the claim's machine refuses behind it
// (supervise.ErrDeliveryPending). Where inTurn is set, the claim is in a turn that started before
// the task came, so the task is held unsent behind that turn; otherwise it is sent.
func pendingDeliveryRig(t *testing.T, role claim.Role, inTurn bool, pending supervise.RequestDeliver, payload record.SuperviseRequest) (*eventWaitHarness, *supervise.Machine, *eventWaitRow) {
	t.Helper()
	h := newEventWaitHarness(t, "LEGION-208")
	h.issue("LEGION-208", phase.Implementing, "in_progress", nil)
	token := launchedClaim(t, h.sup, "LEGION-208", "LEGION-208", role)
	machine := h.machine(token)
	if inTurn {
		if err := machine.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("the turn of %s: %v", token, err)
		}
	}
	pending.Claim = token
	if err := machine.Handle(h.ctx, pending); err != nil {
		t.Fatalf("deliver the pending task: %v", err)
	}
	machine.Wait()
	if held := machine.Claim().Pending; held == nil || held.ID != pending.ID || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the claim holds %+v pending, want %s and not confirmed", held, pending.ID)
	}
	return h, machine, h.queue("LEGION-208", payload, h.clock.Now())
}

// R6 (E1): a start whose task the claim's machine refuses behind the claim's pending task, which
// no turn has confirmed (refuseBehindPending, supervise.ErrDeliveryPending), waits for the
// claim's change on the capped backoff, at most 12 runs an hour once at the cap; the turn that
// confirms the pending task makes the start due at once, and the start's run then queues its task
// in the confirmed one's place.
func TestEventWaitPendingDeliveryRunsWhenTheTurnTakesIt(t *testing.T) {
	t.Parallel()
	const task = "Take the review into account."
	h, machine, start := pendingDeliveryRig(t, claim.RoleImplementer, false,
		supervise.RequestDeliver{Task: "Implement the plan.", ID: "outbox:900", Phase: phase.Implementing, Generation: 1, Entry: 1},
		record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Task: task})
	token := machine.Claim().Token

	start.waitsOnToTheCap(record.WaitForClaim(token))
	start.dueAtItsEvent(func() {
		if err := machine.Handle(h.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
			t.Fatalf("the turn that takes the pending task: %v", err)
		}
	})
	if got := start.state(); got.queued {
		t.Fatalf("the start still waits at step %d for %q after the turn took the pending task, want it run and finished", got.step, got.key)
	}
	if held := machine.Claim().Pending; held == nil || held.Task != task {
		t.Fatalf("the claim holds %+v pending, want the start's task in the confirmed one's place", held)
	}
}

// R6 (E1): a start refused behind the claim's pending task waits for the claim's change, and the
// end of that task's hold — its retirement (supervise.Machine.RetireStoppedTask), or its drop for
// a newer task that supersedes it (supervise's dropPending) — makes the start due at once.
func TestEventWaitPendingDeliveryRunsWhenItIsRetiredOrDropped(t *testing.T) {
	t.Parallel()
	t.Run("retired", func(t *testing.T) {
		t.Parallel()
		h, machine, start := pendingDeliveryRig(t, claim.RoleImplementer, false,
			supervise.RequestDeliver{Task: "Implement the plan.", ID: "outbox:900", Phase: phase.Implementing, Generation: 1, Entry: 1},
			record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing,
				Task: "Take the review into account."})
		token := machine.Claim().Token

		start.waitsOnToTheCap(record.WaitForClaim(token))
		start.dueAtItsEvent(func() {
			if retired, err := machine.RetireStoppedTask(h.ctx, phase.Implementing, 1, 1); err != nil || !retired {
				t.Fatalf("retire the pending task = %t, %v; want it retired", retired, err)
			}
		})
		if got := start.state(); got.queued {
			t.Fatalf("the start still waits at step %d for %q after the pending task was retired, want it run and finished", got.step, got.key)
		}
	})
	t.Run("dropped", func(t *testing.T) {
		t.Parallel()
		// A root architect's task of an entry that no turn has taken and that is not on its way,
		// held unsent behind the turn the architect is in, is dropped for the task of a later
		// entry of the same run (supervise's supersedes). The start that waits behind it carries
		// no entry, so it supersedes nothing and is refused.
		h, machine, start := pendingDeliveryRig(t, claim.RoleArchitect, true,
			supervise.RequestDeliver{Task: "Plan the tree.", ID: "outbox:900", Generation: 1, Entry: 1},
			record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 1, Task: "Read the board."})
		token := machine.Claim().Token

		start.waitsOnToTheCap(record.WaitForClaim(token))
		start.dueAtItsEvent(func() {
			if err := machine.Handle(h.ctx, supervise.RequestDeliver{Claim: token, Task: "Plan the next entry.", ID: "outbox:901",
				Generation: 1, Entry: 2}); err != nil {
				t.Fatalf("deliver the later entry's task: %v", err)
			}
			if held := machine.Claim().Pending; held == nil || held.ID != "outbox:901" {
				t.Fatalf("the claim holds %+v pending, want the later entry's task in the dropped one's place", held)
			}
		})
	})
}

// designGateOpening records gate, closed, as the design gate of its issue, gated on the spec step
// (config.DesignGateRootIssues), and answers the opening that approves its latest version: the
// approval as Dispatch reports it, applied through the workflow engine as the daemon applies every
// Dispatch event (workflow's putGateTold, tellGateMoved).
func designGateOpening(t *testing.T, pool *store.Pool, records record.Store, gate record.DesignGate) func() {
	t.Helper()
	putOutboxGate(t, pool, records, gate)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	return func() {
		t.Helper()
		applyFact(t, pool, "dispatch", "approved-"+gate.ArtifactID, intake.DispatchArtifact{Key: gate.Issue, ArtifactID: gate.ArtifactID,
			Kind: intake.DispatchArtifactApproved, Version: gate.LatestVersion, Actor: dispatch.Actor{Kind: "user", ID: "alice"},
			Mark: record.EventMark{Seq: 90}}, engine)
	}
}

// R6 (E2, E3): a merger's start that its tree's closed design gate holds
// (mergerWaitsForTheDesignGate) waits for the gate to open on the capped backoff, at most 12 runs
// an hour once at the cap, and the gate's opening makes it due at once (tellGateMoved). The
// opening approves a version the merger has not read, so the start then waits for what changed to
// be recorded, and the reconcile's record of it makes the start due at once (designChanges), which
// then starts the merger.
func TestEventWaitDesignGateRunsTheMergerStartWhenItOpens(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	clock := microClock()
	r.runner.now = clock.Now
	r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	open := designGateOpening(t, r.pool, r.records, record.DesignGate{Issue: reconcileRoot, Step: record.DesignStepSpec, ArtifactID: reconcileDoc,
		LatestVersion: 6, ApprovedVersion: new(5), ApprovedMark: record.EventMark{Seq: 30}, EverOpened: true})
	id := queueRow(t, r.pool, r.records, mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
		Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging, Entry: 1, Task: "Merge it."}, clock.Now()))
	start := &eventWaitRow{t: t, runner: r.runner, pool: r.pool, clock: clock, id: id}

	start.waitsOnToTheCap(record.WaitForGate(reconcileRoot))
	if spawns := len(r.rt.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the gate was closed, want none", spawns)
	}
	reconcileAt := clock.Now().Add(time.Hour)
	start.dueAtItsEvent(func() {
		open()
		// The reconcile the opening queued runs an hour out, so the start's run at the opening
		// meets the news not recorded yet.
		if _, err := r.pool.Exec(r.ctx, "update outbox set next_at = $2 where kind = 'design_changes' and issue = $1", reconcileRoot, reconcileAt); err != nil {
			t.Fatalf("hold the reconcile an hour out: %v", err)
		}
	})
	if got := start.state(); !got.queued || got.key != record.WaitForNews(reconcileRoot) {
		t.Fatalf("the start after its run at the gate's opening waits for %q (queued %t), want it waiting for the news, %q",
			got.key, got.queued, record.WaitForNews(reconcileRoot))
	}

	start.waitsOn(record.WaitForNews(reconcileRoot), 2)
	start.dueAtItsEvent(func() {
		clock.advance(reconcileAt.Sub(clock.Now()))
		start.drain()
	})
	if got := start.state(); got.queued {
		t.Fatalf("the merger's start still waits for %q once the gate opened and the news was recorded, want it run", got.key)
	}
	if spawns := len(r.rt.CallsOf("Spawn")); spawns != 1 {
		t.Fatalf("the runtime spawned %d sessions once the gate opened and the news was recorded, want the merger's one", spawns)
	}
}

// R6 (E2, E4): a held message (deliverHeld) to a released role while its tree's design gate is
// closed waits for the gate to open on the capped backoff, at most 12 runs an hour once at the
// cap, and the gate's opening makes it due at once: to a released merger, by the rule that holds
// the merger's start (mergerWaitsForTheDesignGate), and to a released role of another kind, by the
// gate alone (designGateClosed).
func TestEventWaitDesignGateRunsAHeldMessageWhenItOpens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		at      phase.Phase
		status  string
		role    claim.Role
		payload record.OutboxPayload
	}{
		{"a merge-queue removal to a released merger", phase.Merging, "retro", claim.RoleMerger, record.MergeQueueRemoval{Number: 86, Head: "sha-1"}},
		{"a merge forward's ask to a released implementer", phase.Implementing, "in_progress", claim.RoleImplementer,
			record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: "sha-1", Base: "main",
				Phase: phase.Implementing, PullRequest: 86, Why: record.ConflictMergeNext}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := microClock()
			runner, sup, pool := runnerIn(t, tc.at, tc.status, &outboxPublisher{}, quietLogger())
			runner.now, runner.gatedSteps = clock.Now, config.DesignGateRootIssues.Steps()
			if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
				return runner.records.PutPullRequest(ctx, tx, record.PullRequest{
					State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "acme/widgets", Number: 86,
					Branch: "legion/LEGION-208", HeadSHA: "sha-1", HeadUpdatedAt: clock.Now(),
					Failing: []string{}, CheckRuns: []record.AttemptRun{}, Base: "main",
					MergeForward: &record.MergeForward{Head: "sha-1", Base: "main", Role: tc.role,
						Phase: tc.at, Why: record.ConflictMergeNext, AskedAt: clock.Now()},
				})
			}); err != nil {
				t.Fatalf("seed LEGION-208's pull request: %v", err)
			}
			releasedClaim(t, sup, tc.role)
			open := designGateOpening(t, pool, runner.records, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec,
				ArtifactID: "artifact-208", LatestVersion: 2, ApprovedVersion: new(1)})
			id := queueRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", tc.payload, clock.Now()))
			held := &eventWaitRow{t: t, runner: runner, pool: pool, clock: clock, id: id}

			held.waitsOnToTheCap(record.WaitForGate("LEGION-208"))
			held.dueAtItsEvent(open)
		})
	}
}

// R4 (R6): a row that waits for an event of the daemon's own logs its wait once at Info when it
// starts, once at WARN when it has waited 30 minutes, naming the event it waits for, and once at
// Info when it ends, and every other run at Debug alone, over more than 20 runs on the capped
// backoff (dispatch://INFRA-4131/spec#b-r-logged-once). The row is a close of an issue's pod behind
// a role stop that runs an hour out (E10).
func TestEventWaitLoggedOnceAtWarn(t *testing.T) {
	t.Parallel()
	h := newEventWaitHarness(t, "LEGION-208")
	h.closesIssues()
	h.lingering("LEGION-208")
	stopAt := h.clock.Now().Add(time.Hour)
	h.queue("LEGION-208", record.SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1,
		Reason: "the tree of LEGION-208 lingers"}, stopAt)
	closing := h.queue("LEGION-208", record.IssueSuspend{Tree: "LEGION-208", Generation: 1, TreeGeneration: 1}, h.clock.Now())
	key := record.WaitForRoleStops("LEGION-208")

	closing.waitsOn(key, roleWaitRuns)
	if last := closing.runs[len(closing.runs)-1]; !last.Before(stopAt) {
		t.Fatalf("the close's %d runs reached %s past its start, want them all before the role stop an hour out", roleWaitRuns, last.Sub(closing.runs[0]))
	}
	closing.dueAtItsEvent(func() {
		h.clock.advance(stopAt.Sub(h.clock.Now()))
		closing.drain()
	})
	if got := closing.state(); got.queued {
		t.Fatalf("the close still waits for %q once its role stop finished, want it ended", got.key)
	}
	wantWaitLogged(t, h.logs.String(), closing.id, "LEGION-208", record.OutboxKindIssueSuspend, roleWaitRuns+1,
		waitLog{level: slog.LevelWarn, attempts: roleWaitLongAttempts, waitsFor: []any{"event", key}, end: "run"})
}
