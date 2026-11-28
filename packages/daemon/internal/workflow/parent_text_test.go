package workflow

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// requirementOne is the block of the spec task a stands under in these tests, as its block's
// `parent` names it.
const requirementOne = "dispatch://ACME-1/spec#b-r1"

// A task whose block names a parent keeps that parent where the version is stored
// (record.PlanTask.ParentRef), and a later version is compared with it (heldPlan): version 2 gives
// a the same parent and a new text while its first step runs, and it is taken. Task a goes on at
// its step on its steps with its counts, holds version 2's text, and keeps its parent. The control
// is heldPlan without the stored parent: the held task then names none, so version 2 reads as a
// parent added to a running task and is refused.
func TestARunningTaskWithAStoredParentTakesALaterText(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	engine := returnEngine()
	apply := applyFacts(t, pool, engine)
	root := stepRoot(t, pool, phase.Admitted)
	key, gen := root.Key, root.Generation
	first := givenTask(t, "Build the widget.", "implementer", "tester", "reviewer")
	first.ParentRef = requirementOne
	mustTake(t, "version 1", apply(planID(key, gen, 1), ownersPlan(t, key, gen, 1, first)))
	move(t, pool, engine, key, TriggerGateOpened, "", record.PhaseRow{})

	before := planStandingOf(t, pool, engine, key, gen)
	if task := before.task(t); task.State != record.TaskRunning || task.ParentRef != requirementOne {
		t.Fatalf("task a after its gate opened = %+v, want it running with the parent %s its block names", task, requirementOne)
	}
	second := givenTask(t, "Build the widget, and its manual.", "implementer", "tester", "reviewer")
	second.ParentRef = requirementOne
	mustTake(t, "version 2, the same parent and a new text", apply(planID(key, gen, 2), ownersPlan(t, key, gen, 2, second)))

	after := planStandingOf(t, pool, engine, key, gen)
	task, was := after.task(t), before.task(t)
	if task.State != record.TaskRunning || task.Place != was.Place || !sameReturnCounts(task.Counts, was.Counts) {
		t.Fatalf("task a after version 2 = %+v, want it running at %+v with its counts as they were", task, was.Place)
	}
	if task.BlockText != second.Text || task.ParentRef != requirementOne {
		t.Fatalf("task a after version 2 holds the text %q and the parent %q, want version 2's text and the parent %s", task.BlockText, task.ParentRef, requirementOne)
	}
	if !reflect.DeepEqual(after.plan.Steps, before.plan.Steps) || !reflect.DeepEqual(after.versions, []string{"1 owner", "2 owner"}) {
		t.Fatalf("after version 2 the steps are %+v and the versions %v, want the steps as they were and versions 1 and 2", after.plan.Steps, after.versions)
	}
}
