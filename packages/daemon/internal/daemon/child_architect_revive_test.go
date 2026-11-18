package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// holdsNotice says whether notices holds one of kind about role's claim.
func holdsNotice(notices []record.Notice, kind record.NoticeKind, role claim.Role) bool {
	return slices.ContainsFunc(notices, func(notice record.Notice) bool { return notice.Kind == kind && notice.Role == role })
}

// A child's own architect whose claim fails is started again at the child's next phase start,
// with fresh budgets and the session it had, or a new one once supervise's Machine.forgetSession
// has dropped that (this start asks for no new session). The child's architect is started by the
// child's first
// phase start and boots on its session; its process then dies and the runtime refuses its
// relaunch, which spends its launch budget and fails the claim, and the machine reports the
// failure through the daemon's terminal hook (workflowRuntime.terminal). The worker-died that
// failure writes is about the child's architect, so it is published to the root's architect, and
// a notice of the child written while its architect is down — the planner's lost session — goes
// there too, since no architect between the child and the root holds its role. The planner's
// completion then moves the child to implementing, a phase start, and that start queues the
// child's architect's start: the outbox retries the failed claim, which resumes the session file
// it recorded rather than spawning a fresh agent, on the same claim with its budgets zeroed.
func TestAFailedSubArchitectIsRetriedWithFreshBudgetsAtTheChildsNextPhaseStart(t *testing.T) {
	rig := newChildArchitectRig(t)
	token := mustClaimToken(t, doorChild, claim.RoleArchitect)
	rig.written("release", childAt("todo", 2))
	rig.drain()
	machine := rig.boot(doorChild, claim.RoleArchitect, "ses-child-architect")
	rig.boot(doorChild, claim.RolePlanner, "ses-child-planner")
	rig.drain()
	booted := machine.Claim()
	if booted.State != supervise.StateReady || booted.Session != "ses-child-architect" || booted.Locator == nil {
		t.Fatalf("the child's architect after its boot = %+v, want it ready on the session it registered", booted)
	}

	// The architect's process dies, which charges a launch failure, and the runtime refuses the
	// relaunch, which charges the second and spends the rig's budget of two (newOutboxSupervisor).
	rig.rt.ScriptResume(fake.SpawnResult{Err: errors.New("the pod did not start")})
	if err := machine.Handle(rig.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *booted.Locator, Kind: runtime.Gone, At: time.Now()}}); err == nil {
		t.Fatal("the death whose relaunch the runtime refused answered nil, want the refusal")
	}
	failed := machine.Claim()
	if failed.State != supervise.StateFailed || failed.Budgets.LaunchFailures == 0 || failed.Session != booted.Session || failed.SessionFile != booted.SessionFile {
		t.Fatalf("the child's architect after its relaunches ran out = %+v, want it failed with launch failures counted and its session kept", failed)
	}
	rig.drain()
	if root := rig.noticesTo(doorRoot); !holdsNotice(root, "worker-died", claim.RoleArchitect) {
		t.Fatalf("the root's architect was published %+v, want the worker-died of the child's architect", root)
	}
	if child := rig.noticesTo(doorChild); holdsNotice(child, "worker-died", claim.RoleArchitect) {
		t.Fatalf("the child's own failed architect was published %+v, want its worker-died to go to the architect above it", child)
	}

	rig.apply("planner-session-lost", intake.SessionLost{Issue: doorChild, Role: claim.RolePlanner, Detail: "the session file is gone"})
	if starts := rig.architectStarts(doorChild); starts != 0 {
		t.Fatalf("the child holds %d architect starts before its next phase start, want none: no notice of the child revives its architect; the workflow does at the child's next phase start, and an operator's retry can", starts)
	}
	rig.drain()
	if root := rig.noticesTo(doorRoot); !holdsNotice(root, "session-lost", claim.RolePlanner) {
		t.Fatalf("the root's architect was published %+v, want the child's later notice while its architect is down", root)
	}
	if child := rig.noticesTo(doorChild); holdsNotice(child, "session-lost", claim.RolePlanner) {
		t.Fatalf("the child's own failed architect was published %+v, want the child's later notice to go above it", child)
	}
	if again := machine.Claim(); again.State != supervise.StateFailed {
		t.Fatalf("the child's architect before its child's next phase start = %s, want still failed", again.State)
	}

	called := len(rig.rt.Calls())
	child := readIssue(t, rig.pool, doorChild)
	rig.apply("planned", intake.HandoffComplete{Issue: doorChild, Role: claim.RolePlanner, Claim: mustClaimToken(t, doorChild, claim.RolePlanner),
		Generation: child.Generation, Summary: "the plan", HandoffRecord: "plan-1", HandoffVersion: 1})
	if got := readIssue(t, rig.pool, doorChild); got.Phase != phase.Implementing {
		t.Fatalf("the child = %s after its planner's completion, want implementing", got.Phase)
	}
	if starts := rig.architectStarts(doorChild); starts != 1 {
		t.Fatalf("the child's move into implementing queued %d architect starts, want one", starts)
	}
	rig.drain()
	if again := rig.machine(doorChild, claim.RoleArchitect); again != machine {
		t.Fatal("the child's next phase start made its architect a new claim, want the claim that failed")
	}
	launches := launchesSince(rig.rt, token, called)
	if len(launches) != 1 || launches[0].Method != "Resume" || launches[0].Spec.ResumeSessionFile != booted.SessionFile {
		t.Fatalf("the child's next phase start launched its architect %+v, want one resume of the session file %s", launches, booted.SessionFile)
	}
	retried := machine.Claim()
	if retried.State == supervise.StateFailed || retried.Budgets != (supervise.Budgets{}) || retried.Session != booted.Session ||
		retried.SessionFile != booted.SessionFile || retried.Failure != "" {
		t.Fatalf("the child's architect after its child's next phase start = %+v, want it relaunched on its session %s with fresh budgets and no failure",
			retried, booted.Session)
	}

	rig.boot(doorChild, claim.RoleArchitect, booted.Session)
	rig.drain()
	if ready := machine.Claim(); ready.State != supervise.StateReady || ready.Budgets != (supervise.Budgets{}) {
		t.Fatalf("the child's architect after its relaunch's boot = %s with budgets %+v, want ready with fresh budgets", ready.State, ready.Budgets)
	}
}
