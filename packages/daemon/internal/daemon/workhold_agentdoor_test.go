package daemon

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Decision 16 of dispatch://INFRA-4528's plan at the daemon (I3): under `gates.hold: work`, while a
// tree's standing design gate is closed, the project's controller starts no phase worker of that
// tree. Its resume, its retry and its fresh session come through the real agent door the daemon's
// API serves, each with a controller grant (askTheControllerDoor), over the work-hold rig's
// supervisor, store, workflow and admission (newWorkHoldRig). The door refuses each 409
// HELD_BY_WORK_HOLD before anything of the claim changes (api's agentDoor,
// supervise.Machine.WorkHoldRefusal), and claimstart's phase restart of a worker the controller's
// stop retired writes no start row (restartWorkedPhase). Once the gate opens, the claims the
// opening does not start are the controller's to start, and its resume and its retry are taken.
//
// The worker is the root's implementer, which holds the root's implementing phase: released with
// its phase's task kept unconfirmed in a session of its own, as
// TestWorkHoldKeepsAReleasedWorkerStoppedUntilTheOpening seeds it, so each refusal has a session
// and a phase task to keep.

// heldSession and heldSessionFile are the session the implementer's claim records through every
// case here (workHoldRig.releasedWithATask).
const (
	heldSession     = "ses-implementer"
	heldSessionFile = "/tmp/implementer.jsonl"
)

// implementerWithItsTask is the root's implementer holding its phase at entry 1, released while it
// held that entry's task, unconfirmed, in a session of its own.
func implementerWithItsTask(r *workHoldRig) {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.tree.key, Role: claim.RoleImplementer,
			Claim: r.token(claim.RoleImplementer), Entry: 1, EntryTask: keptTask})
	})
	r.releasedWithATask(claim.RoleImplementer, keptTask, 1)
}

// stoppedByTheClose is case (a): implementerWithItsTask, and the close at version 4, whose stop
// the hold makes of the released implementer at once.
func stoppedByTheClose(r *workHoldRig) {
	r.t.Helper()
	implementerWithItsTask(r)
	launched := r.launches(claim.RoleImplementer)
	r.close(4)
	r.drain()
	r.wantHeld(claim.RoleImplementer, launched, "after the close")
}

// suspendedBeforeTheClose is case (b): implementerWithItsTask suspended by the controller through
// its door, which takes the release over, and then the close at version 4, whose stop leaves the
// controller's suspension as it is, since the controller's suspension outranks the hold's stop.
func suspendedBeforeTheClose(r *workHoldRig) {
	r.t.Helper()
	implementerWithItsTask(r)
	if recorder := askTheControllerDoor(r, agentSuspend, claim.RoleImplementer); recorder.Code != http.StatusOK {
		r.t.Fatalf("the controller's suspend of the released implementer = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	r.close(4)
	r.drain()
	if c := r.machine(claim.RoleImplementer).Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByController || c.WorkHeld() {
		r.t.Fatalf("after the close the implementer is %s by %q, WorkHeld %v; want the controller's suspension left as it was",
			c.State, c.SuspendedBy, c.WorkHeld())
	}
	if state, by := r.stored(claim.RoleImplementer); state != supervise.StateSuspended || by != supervise.SuspendedByController {
		r.t.Fatalf("after the close the implementer is stored %s by %q, want the controller's suspension", state, by)
	}
}

// takenOverAfterTheClose is case (c): stoppedByTheClose, and the controller's suspension through
// its door, which takes the hold's stop over.
func takenOverAfterTheClose(r *workHoldRig) {
	r.t.Helper()
	stoppedByTheClose(r)
	theControllerTakesOver(r, claim.RoleImplementer)
	if state, by := r.stored(claim.RoleImplementer); state != supervise.StateSuspended || by != supervise.SuspendedByController {
		r.t.Fatalf("the implementer the controller took over is stored %s by %q, want the controller's suspension", state, by)
	}
}

// agentDoorCases are the claims of a held tree whose start the controller asks while the gate is
// closed (decision 16): (a) one the hold stopped, (b) one the controller suspended before the
// close, and (c) one the hold stopped that the controller's suspension took over. Each walk leaves
// the gate closed and the implementer holding its session and its phase task.
var agentDoorCases = []struct {
	name string
	walk func(r *workHoldRig)
}{
	{"(a) a claim the hold stopped", stoppedByTheClose},
	{"(b) a claim the controller suspended before the close", suspendedBeforeTheClose},
	{"(c) a held claim the controller's suspension took over", takenOverAfterTheClose},
}

// agentDoorStarts are the controller's requests that start an agent, as its agent door takes them:
// its resume, its retry (which reaches a retired phase worker through claimstart's phase restart)
// and its fresh session.
var agentDoorStarts = []struct{ name, route string }{
	{"resume_agent", agentResume},
	{"retry", agentRetry},
	{"fresh_session", agentFreshSession},
}

// storedClaimOf is role's claim of the root as the store holds it, its pending delivery with it.
func (r *workHoldRig) storedClaimOf(role claim.Role) supervise.Claim {
	r.t.Helper()
	claims, err := r.trees.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	token := r.token(role)
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	r.t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}

// claimAsItStands is what a refused start changes nothing of: role's claim as its machine holds it
// and as the store holds it, each with a copy of its pending delivery, the newest outbox row, and
// how often the runtime launched the claim's agent.
type claimAsItStands struct {
	machine, stored supervise.Claim
	newestRow       int64
	launches        int
}

// standing reads role's claim as it stands now (claimAsItStands).
func standing(r *workHoldRig, role claim.Role) claimAsItStands {
	r.t.Helper()
	return claimAsItStands{machine: withOwnPending(r.machine(role).Claim()), stored: withOwnPending(r.storedClaimOf(role)),
		newestRow: r.newestRow(), launches: r.launches(role)}
}

// withOwnPending is c holding a copy of its pending delivery, so no later change of the machine's
// own can reach the copy a comparison reads.
func withOwnPending(c supervise.Claim) supervise.Claim {
	if c.Pending != nil {
		pending := *c.Pending
		c.Pending = &pending
	}
	return c
}

// sameClaim fails the test where after, the claim as where holds it after what, differs from
// before in its state, its suspension's source, its session, its run or the task it holds.
func sameClaim(r *workHoldRig, what, where string, before, after supervise.Claim) {
	r.t.Helper()
	if after.State != before.State || after.SuspendedBy != before.SuspendedBy || after.Session != before.Session ||
		after.SessionFile != before.SessionFile || after.Generation != before.Generation || !reflect.DeepEqual(after.Pending, before.Pending) {
		r.t.Fatalf("%s left the claim in %s %s by %q on session %q (%s) at generation %d holding %+v; want it as before: %s by %q on session %q (%s) at generation %d holding %+v",
			what, where, after.State, after.SuspendedBy, after.Session, after.SessionFile, after.Generation, after.Pending,
			before.State, before.SuspendedBy, before.Session, before.SessionFile, before.Generation, before.Pending)
	}
}

// refusedWithNothingChanged is the controller's request on route for role's agent refused 409
// HELD_BY_WORK_HOLD at the agent door (refusedByTheHold), with nothing of the claim changed: its
// state, its suspension's source, its session and the task it holds stay as its machine and the
// store held them, the refusal writes no outbox row, so no start row of the role, and the runtime
// launches nothing. It answers the claim as it stood.
func refusedWithNothingChanged(r *workHoldRig, route string, role claim.Role) claimAsItStands {
	r.t.Helper()
	before := standing(r, role)
	refusedByTheHold(r, route, role)
	after := standing(r, role)
	what := "the controller's refused " + route
	sameClaim(r, what, "its machine", before.machine, after.machine)
	sameClaim(r, what, "the store", before.stored, after.stored)
	if got := r.startsSince(role, before.newestRow); got != 0 {
		r.t.Fatalf("%s wrote %d start rows of the %s, want none", what, got, role)
	}
	if after.newestRow != before.newestRow {
		r.t.Fatalf("%s wrote outbox rows up to %d past %d, want none", what, after.newestRow, before.newestRow)
	}
	if after.launches != before.launches {
		r.t.Fatalf("%s launched the %s %d times, want none", what, role, after.launches-before.launches)
	}
	return before
}

// wantSessionAndTaskKept is the implementer's claim, in its machine and in the store, recording
// the session it was released in and holding its phase's task, unconfirmed.
func wantSessionAndTaskKept(r *workHoldRig, when string) {
	r.t.Helper()
	for _, held := range []struct {
		where string
		claim supervise.Claim
	}{
		{"its machine", r.machine(claim.RoleImplementer).Claim()},
		{"the store", r.storedClaimOf(claim.RoleImplementer)},
	} {
		c := held.claim
		if c.Session != heldSession || c.SessionFile != heldSessionFile {
			r.t.Fatalf("%s the implementer's claim in %s records the session %q (%s), want its own, %q (%s)",
				when, held.where, c.Session, c.SessionFile, heldSession, heldSessionFile)
		}
		if c.Pending == nil || c.Pending.Task != keptTask || c.Pending.Phase != phase.Implementing || !c.Pending.ConfirmedAt.IsZero() {
			r.t.Fatalf("%s the implementer's claim in %s holds %+v, want its phase's task %q of %s, unconfirmed",
				when, held.where, c.Pending, keptTask, phase.Implementing)
		}
	}
}

// Item 1, decision 16: while the gate is closed, the controller's resume_agent, retry and
// fresh_session of the implementer, each through the agent door with a controller grant, are
// refused 409 HELD_BY_WORK_HOLD, whatever suspended it: (a) the hold, (b) the controller before
// the close, (c) the controller over the hold's stop. Each refusal changes nothing: the claim's
// state, its source, its session and its pending phase task stay as its machine and the store
// held them, and no start row of the implementer is written, then or by the outbox's passes after.
func TestWorkHoldAgentDoorRefusesTheControllersStartsWhileTheGateIsClosed(t *testing.T) {
	for _, tc := range agentDoorCases {
		for _, start := range agentDoorStarts {
			t.Run(tc.name+"/"+start.name, func(t *testing.T) {
				r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
				tc.walk(r)
				wantSessionAndTaskKept(r, "before the controller's "+start.name)

				before := refusedWithNothingChanged(r, start.route, claim.RoleImplementer)

				r.drain()
				after := standing(r, claim.RoleImplementer)
				sameClaim(r, "the outbox's passes after the refused "+start.name, "its machine", before.machine, after.machine)
				sameClaim(r, "the outbox's passes after the refused "+start.name, "the store", before.stored, after.stored)
				if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 0 {
					t.Fatalf("the outbox holds %d starts of the implementer after the refused %s, want none", got, start.name)
				}
				if after.launches != before.launches {
					t.Fatalf("the runtime launched the implementer %d times after the refused %s, want none", after.launches-before.launches, start.name)
				}
			})
		}
	}
}

// Item 2, decision 16: the controller's fresh session of a phase worker of a held tree is refused
// before it forgets anything, so the claim still records its session and still holds its phase
// task, in its machine and in the store, and the outbox's passes after the refusal change neither.
func TestWorkHoldAgentDoorKeepsTheSessionAndPhaseTaskOfARefusedFreshSession(t *testing.T) {
	for _, tc := range agentDoorCases {
		t.Run(tc.name, func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			tc.walk(r)

			refusedWithNothingChanged(r, agentFreshSession, claim.RoleImplementer)
			wantSessionAndTaskKept(r, "after the refused fresh_session")

			r.drain()
			wantSessionAndTaskKept(r, "after the outbox's passes that followed the refused fresh_session")
		})
	}
}

// retiredByTheController is stoppedByTheClose, and the controller's stop of the held implementer,
// which retires it. The opening skips a retired claim, so the controller's retry is what starts it.
func retiredByTheController(r *workHoldRig) {
	r.t.Helper()
	stoppedByTheClose(r)
	theControllerStops(r, claim.RoleImplementer)
	if state, _ := r.stored(claim.RoleImplementer); state != supervise.StateRetired {
		r.t.Fatalf("the implementer the controller stopped is stored %s, want it retired", state)
	}
}

// Item 3, decision 16: the controller's retry of a held phase worker its stop retired reaches
// claimstart's restart of the worker's phase (restartWorkedPhase). While the gate is closed it is
// refused 409 HELD_BY_WORK_HOLD, and no phase restart is applied: no start row of the implementer
// is written, the root stands in its phase at its run, and the claim stays retired.
func TestWorkHoldAgentDoorRefusesTheControllersRetryOfAWorkerItsStopRetired(t *testing.T) {
	r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
	retiredByTheController(r)
	issue := r.issue()

	before := refusedWithNothingChanged(r, agentRetry, claim.RoleImplementer)

	if got := r.issue(); got.Phase != issue.Phase || got.Generation != issue.Generation {
		t.Fatalf("after the refused retry the root is in %s at generation %d, want it in %s at generation %d: no phase restart",
			got.Phase, got.Generation, issue.Phase, issue.Generation)
	}
	r.drain()
	if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 0 {
		t.Fatalf("the outbox holds %d starts of the implementer after the refused retry, want none", got)
	}
	if state, _ := r.stored(claim.RoleImplementer); state != supervise.StateRetired {
		t.Fatalf("after the refused retry the implementer is stored %s, want it retired", state)
	}
	if got := r.launches(claim.RoleImplementer); got != before.launches {
		t.Fatalf("the runtime launched the implementer %d times after the refused retry, want none", got-before.launches)
	}
}

// openedSendingNothing is the person's approval of version 4, which opens the gate, run through
// the outbox: the opening queues no start, no task and no message for the implementer, launches it
// not, publishes nothing to its topic, and leaves its claim as it stood.
func openedSendingNothing(r *workHoldRig) {
	r.t.Helper()
	before := standing(r, claim.RoleImplementer)
	told := len(r.published(claim.RoleImplementer))

	r.approve(4, 12)
	if got := r.sentToClaim(r.tree.key, claim.RoleImplementer, before.newestRow); got != 0 {
		r.t.Fatalf("the opening queued %d starts or messages for the implementer, want none", got)
	}
	r.drain()

	after := standing(r, claim.RoleImplementer)
	sameClaim(r, "the opening", "its machine", before.machine, after.machine)
	sameClaim(r, "the opening", "the store", before.stored, after.stored)
	if after.launches != before.launches {
		r.t.Fatalf("the opening launched the implementer %d times, want none", after.launches-before.launches)
	}
	if got := len(r.published(claim.RoleImplementer)); got != told {
		r.t.Fatalf("the opening told the implementer %d messages, want none", got-told)
	}
}

// Item 4, decision 16: once the gate opens, a claim the opening does not start is the controller's
// to start. The opening sends nothing to (b) the claim the controller suspended before the close
// or (c) the one it took over from the hold; the controller's resume_agent of either is then taken
// (200) and resumes it in the session it kept, with its phase's task. The opening skips the worker
// the controller's stop retired (item 3) too; the controller's retry of it is then taken (202),
// writes the start of its phase, and that start launches it with a task of that phase.
func TestWorkHoldAgentDoorTakesTheControllersStartsOnceTheGateOpens(t *testing.T) {
	for _, tc := range agentDoorCases[1:] {
		t.Run(tc.name+"/resume_agent", func(t *testing.T) {
			r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
			tc.walk(r)
			refusedWithNothingChanged(r, agentResume, claim.RoleImplementer)

			openedSendingNothing(r)
			wantSessionAndTaskKept(r, "after the opening")

			launched, resumed := r.launches(claim.RoleImplementer), len(resumesOf(r.rt, r.token(claim.RoleImplementer)))
			if recorder := askTheControllerDoor(r, agentResume, claim.RoleImplementer); recorder.Code != http.StatusOK {
				t.Fatalf("the controller's resume_agent once the gate opened = %d, want 200; body %s", recorder.Code, recorder.Body)
			}
			r.drain()
			if got := r.launches(claim.RoleImplementer); got != launched+1 {
				t.Fatalf("the controller's resume_agent once the gate opened launched the implementer %d times, want once", got-launched)
			}
			if got := len(resumesOf(r.rt, r.token(claim.RoleImplementer))); got != resumed+1 {
				t.Fatalf("the controller's resume_agent resumed the implementer's session %d times, want once", got-resumed)
			}
			c := r.machine(claim.RoleImplementer).Claim()
			if c.State == supervise.StateSuspended || c.State == supervise.StateRetired || c.WorkHeld() {
				t.Fatalf("after the controller's resume_agent the implementer is %s by %q, want it started", c.State, c.SuspendedBy)
			}
			if c.Session != heldSession || c.Pending == nil || c.Pending.Task != keptTask {
				t.Fatalf("after the controller's resume_agent the implementer is on session %q holding %+v, want its own session and its phase's task %q",
					c.Session, c.Pending, keptTask)
			}
		})
	}
	t.Run("the held worker the controller's stop retired/retry", func(t *testing.T) {
		r := newWorkHoldRig(t, legionTree(phase.Implementing, config.HoldWork))
		retiredByTheController(r)
		refusedWithNothingChanged(r, agentRetry, claim.RoleImplementer)

		openedSendingNothing(r)
		if state, _ := r.stored(claim.RoleImplementer); state != supervise.StateRetired {
			t.Fatalf("after the opening the implementer is stored %s, want it left retired", state)
		}

		launched, since := r.launches(claim.RoleImplementer), r.newestRow()
		if recorder := askTheControllerDoor(r, agentRetry, claim.RoleImplementer); recorder.Code != http.StatusAccepted {
			t.Fatalf("the controller's retry once the gate opened = %d, want 202, the phase's restart; body %s", recorder.Code, recorder.Body)
		}
		if got := r.startsSince(claim.RoleImplementer, since); got != 1 {
			t.Fatalf("the controller's retry once the gate opened wrote %d starts of the implementer, want its phase's one", got)
		}
		r.drain()
		if got := r.launches(claim.RoleImplementer); got != launched+1 {
			t.Fatalf("the phase's start the controller's retry wrote launched the implementer %d times, want once", got-launched)
		}
		c := r.machine(claim.RoleImplementer).Claim()
		if c.State == supervise.StateSuspended || c.State == supervise.StateRetired || c.WorkHeld() {
			t.Fatalf("after the controller's retry the implementer is %s by %q, want it started", c.State, c.SuspendedBy)
		}
		if c.Pending == nil || c.Pending.Phase != phase.Implementing {
			t.Fatalf("after the controller's retry the implementer holds %+v, want a task of its phase, %s", c.Pending, phase.Implementing)
		}
		if got := r.rows(record.OutboxKindSupervise, "start", claim.RoleImplementer); got != 0 {
			t.Fatalf("the outbox still holds %d starts of the implementer after the retry's start ran, want none", got)
		}
	})
}
