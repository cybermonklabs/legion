package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// atReport is the place of a task with a sub-owner once its sub-owner reported it: its report.
var atReport = record.TaskPlace{Kind: record.PlaceReported}

// changedPart is the block of part id of task of, with one implementer's step keyed id, as
// putOwnedVersion stores part(id, of), its text changed: a version that gives it changes it.
func changedPart(id, of string) plan.Task {
	return plan.Task{ID: id, Text: "Do " + id + ". Again.", PartOf: of, Steps: []plan.Step{{Role: "implementer", Key: id}}}
}

// partChanged applies the version of the sub-owner of f that changes the stopped part f1 and keeps
// each other task of tasks as it stands, which must be taken.
func partChanged(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, generation uint64, tasks ...plan.Task) {
	t.Helper()
	given := subOwnersVersion(t, key, generation, "f", 2, append(tasks, changedPart("f1", "f"))...)
	mustTake(t, "the sub-owner's version 2", applyFacts(t, pool, engine)("sub-owner-version-2", given))
}

// A stopped part that a later version changes starts again only where it may start, by the one
// rule of a task's start (mayStart): each task it needs is done, and its task runs at its parts and
// waits for nothing. While its task f is stopped and while f waits for the join of its needs, the
// sub-owner's version that changes the stopped part f1 leaves f1 waiting at no act, its stop ended.
// Once f's join ends, f1 starts (taskJoined, startReady). Where f runs at its parts and waits for
// nothing, the same version starts f1 again at its first step, and its worker is told that the
// sub-owner of task f gave the version. While f stands at its report the version is refused
// before (TestASubOwnersVersionDependsOnWhereItsTaskStands).
//
// The fault it catches is a part started again while its task does not run at its parts (its
// control: startAtFirstStep that asks needsDone alone, as before, starts f1 at step 1 in the first
// two cases, and each fails at "f1 waiting").
func TestAStoppedPartStartsAgainOnlyWhereItsTaskStandsAtItsParts(t *testing.T) {
	h := stepped("h", "implementer", "f")
	for _, tc := range []struct {
		name   string
		f      ownedTask
		stops  []string
		fState record.TaskState
	}{
		{name: "its task is stopped", f: subOwned("f").at(record.TaskRunning, atParts), stops: []string{"f", "f1"}, fState: record.TaskStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Implementing)
			seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
			a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
			seedOwnedVersion(t, pool, root.Key, root.Generation, a, tc.f, part("f1", "f").at(record.TaskRunning, atStep(1)), h)
			stopTasks(t, pool, root.Key, root.Generation, tc.stops...)

			partChanged(t, pool, engine, root.Key, root.Generation, block("a", ""), ownedBlock("f"), block("h", "", "f"))

			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
			assertTask(t, tasks, "f", tc.fState, tc.f.place)
		})
	}
	t.Run("its task waits for the join of its needs, and the end of the join starts it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		a := stepped("a", "implementer").at(record.TaskDone, atStep(1))
		seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f", "a"), part("f1", "f").at(record.TaskRunning, atStep(1)))
		seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key)
		onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
			_, err := engine.startReady(ctx, tx, issue)
			return err
		})
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "f", atParts, record.StepWaitJoin)
		stopTasks(t, pool, root.Key, root.Generation, "f1")

		partChanged(t, pool, engine, root.Key, root.Generation, block("a", ""), ownedBlock("f", "a"))

		assertTask(t, tasksOf(t, pool, engine, root.Key), "f1", record.TaskWaiting, record.TaskPlace{})
		joinEnds(t, pool, engine, root.Key, "f")
		tasks := tasksOf(t, pool, engine, root.Key)
		assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
		assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
	})
	t.Run("its task runs at its parts, so it starts again, told by whom", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Implementing)
		seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
		seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f").at(record.TaskRunning, atParts), part("f1", "f").at(record.TaskRunning, atStep(1)), h)
		stopTasks(t, pool, root.Key, root.Generation, "f1")

		partChanged(t, pool, engine, root.Key, root.Generation, ownedBlock("f"), block("h", "", "f"))

		tasks := tasksOf(t, pool, engine, root.Key)
		assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
		if want := "the sub-owner of task f gave version 2 of the plan while task f1 was stopped"; tasks["f1"].StartReason != want {
			t.Fatalf("task f1 started again for %q, want %q", tasks["f1"].StartReason, want)
		}
	})
}

// A stopped task with a sub-owner starts again at its parts, where its sub-owner acts, the place
// its start puts it (Walk.startPlace), and its parts that wait start once it waits for nothing: with
// a running, f (owner=architect) stopped at its parts, its part f1 waiting and h needing f, the
// owner's version that changes f starts f again at its parts, waiting for the join that makes its
// own branch (startTaskCopy), with f1 waiting; the end of that join starts f1 at its first step.
//
// The fault it catches is a task with a sub-owner started again at a step it does not have (its
// control: startAtFirstStep that moves the task to step 1, as before, which the move refuses, so
// the version is an error).
func TestAStoppedTaskWithASubOwnerStartsAgainAtItsParts(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	a := stepped("a", "implementer").at(record.TaskRunning, atStep(1))
	seedOwnedVersion(t, pool, root.Key, root.Generation, a, subOwned("f").at(record.TaskRunning, atParts), part("f1", "f"), stepped("h", "implementer", "f"))
	stopTasks(t, pool, root.Key, root.Generation, "f")

	f := ownedBlock("f")
	f.Text = "Do f. Again."
	mustTake(t, "the owner's version 2", applyFacts(t, pool, engine)("owner-version-2",
		laterVersion(t, root.Key, root.Generation, 2, block("a", ""), f, partBlock("f1", "f", "implementer"), block("h", "", "f"))))

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitJoin)
	assertTask(t, tasks, "f1", record.TaskWaiting, record.TaskPlace{})
	assertTask(t, tasks, "h", record.TaskWaiting, record.TaskPlace{})

	joinEnds(t, pool, engine, root.Key, "f")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "f1", record.TaskRunning, atStep(1))
}

// A person's todo on a root whose task f (owner=architect) and its part e1 are both stopped starts
// each again by the one rule of a task's start, in task id order: e1 first, which waits, since f
// is stopped; then f, again at its parts, whose start starts e1, which now may start.
//
// The fault it catches is a part left waiting for good by a door that starts its task again at its
// parts and starts none of its parts (its control: startAtFirstStep with no start of the waiting
// parts, and the assertion "e1 running" fails).
func TestATodoStartsAStoppedTaskAndItsStoppedPartAgain(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedOwnedVersion(t, pool, root.Key, root.Generation, subOwned("f").at(record.TaskRunning, atParts), part("e1", "f").at(record.TaskRunning, atStep(1)))
	for _, task := range []string{"e1", "f"} {
		if result := applyFacts(t, pool, engine)("stop-"+task, ownersStop(t, root, task)); result.Refusal != nil {
			t.Fatalf("the owner's stop of %s was refused: %+v", task, result.Refusal)
		}
	}
	if held := issueOf(t, pool, engine, root.Key); held.Phase != phase.Held {
		t.Fatalf("%s after both stops stands in %s with hold %+v, want held for the stopped tasks", root.Key, held.Phase, held.Hold)
	}

	if result := applyFacts(t, pool, engine)("todo", personsTodo(root.Key)); result.Refusal != nil {
		t.Fatalf("the todo was refused: %+v", result.Refusal)
	}

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "f", atParts, record.StepWaitNone)
	assertTask(t, tasks, "e1", record.TaskRunning, atStep(1))
}

// A plan given at generation 0, for an issue the daemon recorded since, waits there only while its
// generation holds a plan an earlier owner gave (waitsAtZero), by the rule the first step passes
// such plans over by (record.Store's PlansOfEarlierOwners): a version a sub-owner gave is its
// owner's plan. With LEGION-208 admitted and its generation's plan in force given by the sub-owner
// of f over a version of the tree's architect, the plan is its owner's, which the first step keeps,
// so the plan given at generation 0 is refused PLAN_STALE_GENERATION and nothing is written.
//
// The fault it catches is a plan at generation 0 taken to wait for a first step that will take the
// generation's plan instead (its control: waitsAtZero judging the version by its own giver, as
// before, takes it).
func TestAPlanAtZeroWaitsOnlyBeforeAnEarlierOwnersPlan(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Admitted)
	architect, subOwner := architectOf(t), agentClaim(t, root.Key, plan.SubOwnerAgent("f"))
	seedRecord(t, pool, func(tx pgx.Tx) error {
		if err := putOwnedVersion(t.Context(), tx, root.Key, root.Generation, 1, architect, subOwned("f")); err != nil {
			return err
		}
		return putOwnedVersion(t.Context(), tx, root.Key, root.Generation, 2, subOwner, subOwned("f"), part("f1", "f"))
	})

	given := laterVersion(t, root.Key, 0, 1, block("a", ""))
	before := planStandingOf(t, pool, engine, root.Key, 0)
	assertPlanRefusal(t, pool, engine, root.Key, 0, planID(root.Key, 0, 1), applyFacts(t, pool, engine)(planID(root.Key, 0, 1), given),
		intake.PlanStaleGeneration(root.Key, 0, root.Generation), before)
}
