package workflow

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// agentsOfTask is the name of the agent of each stored step of task, in place order.
func agentsOfTask(stored *record.StoredPlan, task string) []claim.Agent {
	var names []claim.Agent
	for _, step := range stored.Steps[task] {
		names = append(names, step.Agent)
	}
	return names
}

// A later version in which a step with no key would need the name its role gives while a key holds
// that name from an earlier version is a plan this release cannot carry out: it runs one agent of
// each role for an issue. The version is refused as a plan, 409 PLAN_REFUSED with R14's ending,
// before anything is written, and the fact stays unspent; version 1's rows, with their names,
// stand.
func TestALaterVersionThatWouldRenameAKeyIsRefusedAsAPlan(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer:a", "tester:a", "reviewer"))))
	move(t, pool, engine, key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
	if result := apply("stop", ownersStop(t, root, "a")); result.Refusal != nil {
		t.Fatalf("the owner's stop was refused: %+v", result.Refusal)
	}
	before := planStandingOf(t, pool, engine, key, gen)
	if got := agentsOfTask(before.plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "implementer", "reviewer"}) {
		t.Fatalf("the agents of version 1 = %q, want key a named implementer at steps 1 and 2, and reviewer", got)
	}

	id := planID(key, gen, 2)
	reason := "step 1 of task a needs an agent of its own, and another agent of this plan holds the name implementer of its role from an earlier version; " +
		"this release runs one agent of each role for an issue. This release of Legion does not carry it out yet."
	want := &intake.Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + reason,
		Reasons: []plan.Refusal{{Task: "a", Reason: reason}}}
	assertPlanRefusal(t, pool, engine, key, gen, id,
		apply(id, ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget.", "implementer", "tester:a", "reviewer"))), want, before)
}

// A version that gives a task again names its keys from the rows read before it replaces the
// task's steps, so a key keeps the name it had, even where its first step is now of another role.
func TestARegivenTaskKeepsTheNamesOfItsKeys(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, givenTask(t, "Build the widget.", "implementer", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"implementer", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 1 = %q, want implementer, key a named tester, and reviewer", got)
	}
	mustTake(t, "version 2", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, givenTask(t, "Build the widget, and its manual.", "implementer:a", "tester:a", "reviewer"))))
	if got := agentsOfTask(planStandingOf(t, pool, engine, key, gen).plan, "a"); !slices.Equal(got, []claim.Agent{"tester", "tester", "reviewer"}) {
		t.Fatalf("the agents of version 2 = %q, want key a still named tester at steps 1 and 2, and reviewer", got)
	}
}

// The project's default steps are named from their own steps alone when the first step of a run
// with no owner's plan writes them: each agent has a name, the steps of a key share it.
func TestTheDefaultStepsAreNamedWhenTheyAreWritten(t *testing.T) {
	engine := returnEngine()
	engine.cfg.DefaultSteps = []plan.Step{{Role: "implementer"}, {Role: "tester", Key: "r"}, {Role: "reviewer", Key: "r"}}
	_, _, steps := engine.defaultRows(record.Issue{Key: "LEGION-208", Generation: 1})
	var names []claim.Agent
	for _, step := range steps {
		names = append(names, step.Agent)
	}
	if want := []claim.Agent{"implementer", "tester", "tester"}; !slices.Equal(names, want) {
		t.Fatalf("the default steps are named %q, want %q", names, want)
	}
}
