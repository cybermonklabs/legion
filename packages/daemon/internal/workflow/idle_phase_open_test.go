package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// A phase reminder is armed in the claim of the worker of the issue's open phase that serves the
// issue's current generation (idlePhaseOpen): the role of the claim that works the task's place,
// which the idle view reads with the issue (pods.IdleView.PlaceRole), none at a place the owner
// does, and at step the role of the running step (pods.IdleView.StepRole), which no phase names.
// The generation is held for both: a checker serving an earlier run is not the worker of this one.
// Where the view reads no place's role, as in these views, a phase of Legion's names the worker,
// as before.
func TestAPhaseReminderIsArmedForTheDeploymentRoleThatRuns(t *testing.T) {
	const checker claim.Role = "checker"
	const checkerToken, implementerToken, testerToken claim.Token = "legion-legion-legion-209-checker",
		"legion-legion-legion-209-implementer", "legion-legion-legion-209-tester"
	atStep := pods.IdleView{Issue: "LEGION-209", Project: "legion", Phase: phase.Step, StepRole: checker, Generation: 3}
	atTesting := pods.IdleView{Issue: "LEGION-209", Project: "legion", Phase: phase.Testing, StepRole: claim.RoleTester, Generation: 3}
	for _, c := range []struct {
		name  string
		view  pods.IdleView
		claim pods.IdleClaim
		want  bool
	}{
		{"the checker at step, serving the current run", atStep, pods.IdleClaim{Token: checkerToken, Role: checker, ServingGeneration: 3}, true},
		{"the checker at step, serving an earlier run", atStep, pods.IdleClaim{Token: checkerToken, Role: checker, ServingGeneration: 2}, false},
		{"the implementer at step", atStep, pods.IdleClaim{Token: implementerToken, Role: claim.RoleImplementer, ServingGeneration: 3}, false},
		{"the tester at testing, serving the current run", atTesting, pods.IdleClaim{Token: testerToken, Role: claim.RoleTester, ServingGeneration: 3}, true},
	} {
		if got := idlePhaseOpen(c.view, c.claim); got != c.want {
			t.Errorf("%s: idlePhaseOpen = %t, want %t", c.name, got, c.want)
		}
	}
}

// Two claims of one issue can hold the tester's role: key a's agent, which the tester's phases
// row names on its step (pods.IdleView.Phases), and the tester's own claim. Only the claim the
// record gives the step works the phase (record.WorkerClaim), so a reminder due on the tester's
// own claim holds no pod (IdleHoldReminder). A role whose row runs no step, or that has no row,
// works its phase in its own claim alone, the role's own token, so key a's agent, which holds the
// role, works no phase there.
func TestAPhaseReminderIsArmedOnlyForTheClaimTheStepNames(t *testing.T) {
	const keyed, own = claim.Token("legion-legion-legion-209-implementer"), claim.Token("legion-legion-legion-209-tester")
	named := pods.IdleView{Issue: "LEGION-209", Project: "legion", Phase: phase.Testing, Generation: 3,
		Phases: map[claim.Role]pods.IdlePhase{claim.RoleTester: {Claim: keyed, StepTask: "a"}}}
	stepless := pods.IdleView{Issue: "LEGION-209", Project: "legion", Phase: phase.Testing, Generation: 3,
		Phases: map[claim.Role]pods.IdlePhase{claim.RoleTester: {Claim: keyed}}}
	unnamed := pods.IdleView{Issue: "LEGION-209", Project: "legion", Phase: phase.Testing, Generation: 3}
	for _, c := range []struct {
		name  string
		view  pods.IdleView
		claim pods.IdleClaim
		want  bool
	}{
		{"key a's agent, which the row names", named, pods.IdleClaim{Token: keyed, Role: claim.RoleTester, ServingGeneration: 3}, true},
		{"the tester's own claim, which works no step", named, pods.IdleClaim{Token: own, Role: claim.RoleTester, ServingGeneration: 3}, false},
		{"the tester's own claim where the row names none", unnamed, pods.IdleClaim{Token: own, Role: claim.RoleTester, ServingGeneration: 3}, true},
		{"key a's agent where the row names none", unnamed, pods.IdleClaim{Token: keyed, Role: claim.RoleTester, ServingGeneration: 3}, false},
		{"key a's agent where its row runs no step", stepless, pods.IdleClaim{Token: keyed, Role: claim.RoleTester, ServingGeneration: 3}, false},
		{"the tester's own claim where its row runs no step", stepless, pods.IdleClaim{Token: own, Role: claim.RoleTester, ServingGeneration: 3}, true},
	} {
		if got := idlePhaseOpen(c.view, c.claim); got != c.want {
			t.Errorf("%s: idlePhaseOpen = %t, want %t", c.name, got, c.want)
		}
	}
}
