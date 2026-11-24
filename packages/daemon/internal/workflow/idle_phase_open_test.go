package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// A phase reminder is armed in the claim of the worker of one of the issue's open steps that serves
// the issue's current generation (idlePhaseOpen), and at step that worker is the role of the
// running step, which the idle view reads with the issue (pods.IdleView.Running) and no phase
// names. The generation is held for both: a checker serving an earlier run is not the worker of
// this one. On an issue with no plan rows the phase names the worker, as before.
func TestAPhaseReminderIsArmedForTheDeploymentRoleThatRuns(t *testing.T) {
	const checker claim.Role = "checker"
	atStep := pods.IdleView{Phase: phase.Step, Planned: true, Generation: 3,
		Running: []pods.RunningRow{{Task: "a", Kind: "step", Position: 2, Role: string(checker), At: 2}}}
	atTesting := pods.IdleView{Phase: phase.Testing, Generation: 3}
	for _, c := range []struct {
		name  string
		view  pods.IdleView
		claim pods.IdleClaim
		want  bool
	}{
		{"the checker at step, serving the current run", atStep, pods.IdleClaim{Role: checker, ServingGeneration: 3}, true},
		{"the checker at step, serving an earlier run", atStep, pods.IdleClaim{Role: checker, ServingGeneration: 2}, false},
		{"the implementer at step", atStep, pods.IdleClaim{Role: claim.RoleImplementer, ServingGeneration: 3}, false},
		{"the tester at testing, serving the current run", atTesting, pods.IdleClaim{Role: claim.RoleTester, ServingGeneration: 3}, true},
	} {
		if got := idlePhaseOpen(c.view, c.claim); got != c.want {
			t.Errorf("%s: idlePhaseOpen = %t, want %t", c.name, got, c.want)
		}
	}
}
