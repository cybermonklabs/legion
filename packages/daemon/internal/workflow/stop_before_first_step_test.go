package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A recorded issue of no experiment's group that stands before its first step has no plan rows
// at its generation yet: the first step settles the plan it walks (planFirstStep), the plan its
// owner gave, which may wait at generation 0, or the project's default steps. stop_task stops every
// task of such an issue that is not done, so the stop settles that plan as the first step would,
// under the fact lock, and then stops its task, which stands stopped before its first step: held
// from admitted, at no act, with plan rows at the issue's generation. A stop of a task that plan
// does not hold names the plan's tasks and writes nothing. The control is a task that is done,
// refused TASK_DONE. A person's todo on such a root then takes the stopped task back to its first
// step's gate and line (waitForFirstStep).
func TestAStopBeforeTheFirstStepSettlesThePlanAndStopsItsTask(t *testing.T) {
	t.Run("a child whose owner's plan waits at generation 0", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		recordedAt(t, pool, "LEGION-208", "LEGION-208")
		mustTake(t, "version 1 at generation 0", apply(planID("LEGION-209", 0, 1),
			ownersPlan(t, "LEGION-209", 0, 1, givenTask(t, "Build the widget.", "implementer", "reviewer"))))
		recordedAt(t, pool, "LEGION-209", "LEGION-208")
		child := issueOf(t, pool, engine, "LEGION-209")

		result := apply("stop", ownersStop(t, child, "a"))

		if result.Refusal != nil {
			t.Fatalf("the stop of task a = %+v, want it taken", result.Refusal)
		}
		assertStoppedBeforeItsFirstStep(t, pool, engine, child.Key)
		if got := stepsOfTask(planStandingOf(t, pool, engine, child.Key, child.Generation).plan, "a"); !reflect.DeepEqual(got, []string{"implementer/implementation", "reviewer/review"}) {
			t.Fatalf("the steps of task a at generation %d = %v, want the owner's plan carried from generation 0", child.Generation, got)
		}
	})

	t.Run("a root whose owner gave no plan: default, and a task the default steps do not hold", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		apply := applyFacts(t, pool, engine)
		root := stepRoot(t, pool, phase.Admitted)

		before := standingOf(t, pool, engine, root.Key, false)
		missing := apply("stop-a", ownersStop(t, root, "a"))
		if want := intake.TaskNotFound(root.Key, "a", []string{plan.DefaultTask}); !reflect.DeepEqual(missing.Refusal, want) {
			t.Fatalf("the stop of task a = %+v, want %+v", missing.Refusal, want)
		}
		if after := standingOf(t, pool, engine, root.Key, false); !reflect.DeepEqual(after, before) || planRowsOf(t, pool, root.Key) != [3]int{} {
			t.Fatalf("the refused stop changed %s or wrote plan rows %v:\n before %+v\n after  %+v", root.Key, planRowsOf(t, pool, root.Key), before, after)
		}

		if result := apply("stop-default", ownersStop(t, root, plan.DefaultTask)); result.Refusal != nil {
			t.Fatalf("the stop of task default = %+v, want it taken", result.Refusal)
		}
		assertStoppedBeforeItsFirstStep(t, pool, engine, root.Key)

		// A person's todo on the root ends the stop: the task waits at no act again, and the
		// root stands at admitted, where its gate and the line start its first step.
		if result := apply("todo", personsTodo(root.Key)); result.Refusal != nil {
			t.Fatalf("the person's todo = %+v, want it taken", result.Refusal)
		}
		after := issueOf(t, pool, engine, root.Key)
		if task := taskOf(t, pool, engine, root.Key); after.Phase != phase.Admitted || after.Hold != nil || task.State != record.TaskWaiting || task.Place != (record.TaskPlace{}) {
			t.Fatalf("after the todo %s stands in %s (hold %+v), its task %s at %+v; want admitted, unheld, the task waiting at no act",
				root.Key, after.Phase, after.Hold, task.State, task.Place)
		}
	})

	t.Run("control: a task that is done is refused TASK_DONE", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer")
		move(t, pool, engine, root.Key, TriggerGateOpened, record.TaskPlace{}, record.PhaseRow{})
		leaveAs(t, pool, engine, root.Key, "done")
		result := applyFacts(t, pool, engine)("stop", ownersStop(t, issueOf(t, pool, engine, root.Key), "a"))
		if want := intake.FinishedTask(root.Key, "a", record.TaskDone, intake.StopOfTask); !reflect.DeepEqual(result.Refusal, want) {
			t.Fatalf("the stop of a done task = %+v, want %+v", result.Refusal, want)
		}
	})
}

// assertStoppedBeforeItsFirstStep fails unless key is held from admitted for its stopped task,
// which stands stopped at no act in plan rows of key's generation.
func assertStoppedBeforeItsFirstStep(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	if issue.Phase != phase.Held || issue.Hold == nil || *issue.Hold != (record.Hold{From: phase.Admitted, Reason: record.HoldTaskStopped}) {
		t.Fatalf("%s stands in %s with hold %+v, want held from admitted for its stopped task", key, issue.Phase, issue.Hold)
	}
	if now := planStandingOf(t, pool, engine, key, issue.Generation); now.plan == nil {
		t.Fatalf("%s has no plan rows at generation %d after the stop, want the plan its first step would walk", key, issue.Generation)
	}
	if task := taskOf(t, pool, engine, key); task.State != record.TaskStopped || task.Place != (record.TaskPlace{}) {
		t.Fatalf("the task of %s is %s at %+v, want stopped at no act", key, task.State, task.Place)
	}
}
