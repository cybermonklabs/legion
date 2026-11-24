package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A start queued for a role at a phase is the work of a place the issue stands at
// (Issue.HoldsQueued): the issue's own phase, or the phase of a running step of it that is not
// held, and at the phase step the step of that role. Two tasks of a plan run at one time, each at
// its own phase, and the issue's phase is one of them: the start of the other task's step is held
// all the same, which the four judges of queued work then send. Away from step the phase alone is
// compared, as it was: a re-admission brings back each other role of a member that has run on a
// start that names the member's phase and carries no task (admit's startMidPhaseMembers), and the
// issue stands in that phase at every reading of it, held, awaiting a person or admitted, a step of
// it running or not. A held step's phase, and a phase no step stands at and the issue is not in,
// hold nothing. An issue of one task and one with no plan rows read as they did when the rule
// compared the issue's phase alone: a running step of it that is not held stands at that phase.
//
// The faults: a start held to the issue's one phase is dropped for each task that stands at
// another (the outbox's "serves a phase or a step the issue has left"), so the second task's step
// never starts; and a start held to the role of a running step is dropped for every other role a
// re-admission brings back, and for every start of an issue that stands in a phase no running step
// stands at, so those sessions never come back. The controls are the rule that compares the
// issue's phase alone, under which the first case fails, and the rule that compares the running
// steps' roles and never the issue's phase, under which each case of a role brought back and each
// case of a phase no step runs at fails.
func TestAQueuedStartIsHeldToTheRunningStepOfItsRole(t *testing.T) {
	step := func(task string, position int, role claim.Role, at phase.Phase, held bool) RunningStep {
		return RunningStep{Task: task, Place: TaskPlace{Kind: PlaceStep, Position: position}, Role: role, Phase: at, Agent: claim.Agent(role), Held: held}
	}
	twoTasks := Issue{Key: "LEGION-208", Phase: phase.Implementing, Running: []RunningStep{
		step("a", 2, claim.RoleReviewer, phase.Reviewing, false), step("b", 2, claim.RoleImplementer, phase.Implementing, false)}}
	oneHeld := Issue{Key: "LEGION-208", Phase: phase.Implementing, Running: []RunningStep{
		step("a", 2, claim.RoleReviewer, phase.Reviewing, true), step("b", 2, claim.RoleImplementer, phase.Implementing, false)}}
	oneTask := Issue{Key: "LEGION-208", Phase: phase.Reviewing, Running: []RunningStep{step("a", 2, claim.RoleReviewer, phase.Reviewing, false)}}
	// A task that runs, whose step waits for its agent or its join, stands at that step's phase and
	// has no running step yet (RunningStep).
	oneWaiting := Issue{Key: "LEGION-208", Phase: phase.Implementing}
	unplanned := Issue{Key: "LEGION-208", Phase: phase.Testing}
	unplanned.Running = RunningOf(unplanned, false, nil)
	readmitted := Issue{Key: "LEGION-209", Phase: phase.Implementing}
	readmitted.Running = RunningOf(readmitted, false, nil)
	heldUnplanned := Issue{Key: "LEGION-208", Phase: phase.Held, Hold: &Hold{From: phase.Testing}}
	heldUnplanned.Running = RunningOf(heldUnplanned, false, nil)
	awaiting := Issue{Key: "LEGION-208", Phase: phase.AwaitingPerson}
	awaiting.Running = RunningOf(awaiting, false, nil)
	admitted := Issue{Key: "LEGION-208", Phase: phase.Admitted}
	admitted.Running = RunningOf(admitted, false, nil)
	steps := Issue{Key: "LEGION-208", Phase: phase.Step, Running: []RunningStep{step("a", 1, claim.Role("security"), phase.Step, false)}}
	heldStep := Issue{Key: "LEGION-208", Phase: phase.Step, Running: []RunningStep{
		step("a", 1, claim.Role("security"), phase.Step, true), step("b", 1, claim.Role("audit"), phase.Step, false)}}
	for _, c := range []struct {
		name   string
		issue  Issue
		role   claim.Role
		queued phase.Phase
		want   bool
	}{
		{"the step of a task at another phase than the issue's", twoTasks, claim.RoleReviewer, phase.Reviewing, true},
		{"the step of the task at the issue's phase", twoTasks, claim.RoleImplementer, phase.Implementing, true},
		{"another role brought back at the phase of a task's step", twoTasks, claim.RolePlanner, phase.Reviewing, true},
		{"another role brought back at the issue's phase", twoTasks, claim.RolePlanner, phase.Implementing, true},
		{"a role at no running step", twoTasks, claim.RoleTester, phase.Testing, false},
		{"a held step of the role", oneHeld, claim.RoleReviewer, phase.Reviewing, false},
		{"another role at the held step's phase", oneHeld, claim.RolePlanner, phase.Reviewing, false},
		{"the other task beside the held step", oneHeld, claim.RoleImplementer, phase.Implementing, true},
		{"the one task's step", oneTask, claim.RoleReviewer, phase.Reviewing, true},
		{"a phase the one task has left", oneTask, claim.RoleImplementer, phase.Implementing, false},
		{"the one task's step that does not run yet", oneWaiting, claim.RoleImplementer, phase.Implementing, true},
		{"an issue with no plan rows at its phase", unplanned, claim.RoleTester, phase.Testing, true},
		{"an issue with no plan rows past a phase", unplanned, claim.RoleImplementer, phase.Implementing, false},
		{"another role of an issue with no plan rows brought back at its phase", readmitted, claim.RolePlanner, phase.Implementing, true},
		{"an issue with no plan rows held", heldUnplanned, claim.RoleTester, phase.Testing, false},
		{"a role of a held issue with no plan rows brought back at held", heldUnplanned, claim.RolePlanner, phase.Held, true},
		{"a role of an issue awaiting a person brought back there", awaiting, claim.RolePlanner, phase.AwaitingPerson, true},
		{"an issue at admitted, which no step runs at", admitted, claim.RoleArchitect, phase.Admitted, true},
		{"the step of a role of the deployment", steps, claim.Role("security"), phase.Step, true},
		{"the step of another role of the deployment", steps, claim.Role("audit"), phase.Step, false},
		{"a held step of a role of the deployment", heldStep, claim.Role("security"), phase.Step, false},
		{"the step of a role of the deployment beside a held one", heldStep, claim.Role("audit"), phase.Step, true},
	} {
		if got := c.issue.HoldsQueued(c.role, c.queued); got != c.want {
			t.Errorf("%s: HoldsQueued(%s, %s) of an issue at %s running %+v = %v, want %v", c.name, c.role, c.queued, c.issue.Phase, c.issue.Running, got, c.want)
		}
	}
}
