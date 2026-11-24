package workflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// nodeTask is the task of a plan node: id, its steps as the plan writes them and the ids of the
// tasks it needs, [] for none.
func nodeTask(id string, steps []string, needs ...string) record.PlanRecordTask {
	if needs == nil {
		needs = []string{}
	}
	return record.PlanRecordTask{ID: id, Steps: steps, Needs: &needs}
}

// The plan node of a run of several tasks holds each task of the plan, in task id order, with its
// steps and the tasks it needs: Dispatch refuses a need that names a task its node does not hold,
// and the daemon takes that refusal as final for the node's row. At the first step of a plan of
// two tasks, b needing a, the one node written holds a and b, b needing a. A later version taken
// after the work started, which adds c needing b, writes the node again, whole, holding a, b and c.
// At the first step of the plan of tasks a to e, the node holds each of the five with its needs.
// The node a hand-over's carry writes (WritePlan) of a run of two tasks holds both.
//
// The fault it catches is a node that names one task of a plan of several, or none, so a need of
// another task names a task the node does not hold (its control: planNode writing the first task
// alone, which loses b and its need of a).
func TestThePlanNodeHoldsEachTask(t *testing.T) {
	node := func(key string, generation uint64, version int, tasks ...record.PlanRecordTask) record.PlanRecord {
		return record.PlanRecord{ID: PlanID(key, generation), Generation: generation, Source: record.PlanSourceOwner,
			PlanDocument: &record.PlanDocument{ArtifactID: "plan-artifact", Version: version}, Tasks: tasks}
	}
	t.Run("two tasks, b needing a, and a later version", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskWaiting},
			planTask{id: "b", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)

		first := node(root.Key, root.Generation, 1, nodeTask("a", []string{"implementer"}), nodeTask("b", []string{"tester"}, "a"))
		if got := planWrites(t, pool, root.Key); !reflect.DeepEqual(got, []record.PlanRecord{first}) {
			t.Fatalf("the plan nodes of %s at its first step = %+v, want the one node holding a and b, b needing a: %+v", root.Key, got, first)
		}

		given := laterVersion(t, root.Key, root.Generation, 2,
			plan.Task{ID: "a", Text: "Do a.", Steps: []plan.Step{{Role: "implementer"}}},
			plan.Task{ID: "b", Text: "Do b.", Steps: []plan.Step{{Role: "tester"}}, Needs: []string{"a"}},
			plan.Task{ID: "c", Text: "Do c.", Steps: []plan.Step{{Role: "reviewer"}}, Needs: []string{"b"}})
		mustTake(t, "version 2", applyFacts(t, pool, engine)("version-2", given))

		second := node(root.Key, root.Generation, 2, nodeTask("a", []string{"implementer"}), nodeTask("b", []string{"tester"}, "a"),
			nodeTask("c", []string{"reviewer"}, "b"))
		if got := planWrites(t, pool, root.Key); !reflect.DeepEqual(got, []record.PlanRecord{first, second}) {
			t.Fatalf("the plan nodes of %s after version 2 = %+v, want the first and then %+v, whole", root.Key, got, second)
		}
	})

	t.Run("the plan of tasks a to e", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Admitted)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskWaiting},
			planTask{id: "b", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, state: record.TaskWaiting},
			planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-3"}, needs: []string{"b"}, state: record.TaskWaiting},
			planTask{id: "d", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-4"}, needs: []string{"a", "c"}, state: record.TaskWaiting},
			planTask{id: "e", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"d"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)

		want := node(root.Key, root.Generation, 1, nodeTask("a", []string{"implementer"}), nodeTask("b", []string{"implementer"}),
			nodeTask("c", []string{"implementer"}, "b"), nodeTask("d", []string{"implementer"}, "a", "c"), nodeTask("e", []string{"tester"}, "d"))
		if got := planWrites(t, pool, root.Key); !reflect.DeepEqual(got, []record.PlanRecord{want}) {
			t.Fatalf("the plan nodes of %s at its first step = %+v, want the one node holding a to e with their needs: %+v", root.Key, got, want)
		}
	})

	// A hand-over's carry re-admits an orphan at a new generation in the phase it ran in, so its
	// run never leaves admitted and its node is written at the carry (WritePlan).
	t.Run("a carried run of two tasks", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a"}, state: record.TaskWaiting})

		onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			return engine.WritePlan(ctx, tx, issue)
		})

		want := node(root.Key, root.Generation, 1, nodeTask("a", []string{"implementer"}), nodeTask("b", []string{"tester"}, "a"))
		if got := planWrites(t, pool, root.Key); !reflect.DeepEqual(got, []record.PlanRecord{want}) {
			t.Fatalf("the plan nodes of the carried %s = %+v, want the one node holding a and b, b needing a: %+v", root.Key, got, want)
		}
	})
}
