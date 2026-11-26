package workflow

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// assertStoppedWaitingForNothing fails unless task of tasks is stopped at place and its row holds no
// wait and no moment a wait began.
func assertStoppedWaitingForNothing(t *testing.T, tasks map[string]record.PlanTask, task string, place record.TaskPlace) {
	t.Helper()
	assertTask(t, tasks, task, record.TaskStopped, place)
	if row := tasks[task]; row.StepWait != record.StepWaitNone || row.StepWaitSince != nil {
		t.Fatalf("stopped task %s's step waits for %q since %v, want no wait: a stop ends it", task, row.StepWait, row.StepWaitSince)
	}
}

// startsOfAgent is each start of the tester and of the implementer in key's outbox that names the
// claim of agent, oldest first.
func startsOfAgent(t *testing.T, key string, agent claim.Agent, starts ...[]record.SuperviseRequest) []record.SuperviseRequest {
	t.Helper()
	want := agentClaim(t, key, agent)
	var named []record.SuperviseRequest
	for _, rows := range starts {
		for _, start := range rows {
			if start.Claim == want {
				named = append(named, start)
			}
		}
	}
	return named
}

// A stop of a step that waits ends its wait and stops no other step. Of the plan c to z, c is done,
// d (`reviewer`) needs c, m (`tester:k`) and y (`implementer:k`) share agent k, and z needs d, m
// and y, so z is the task that merges. At the first step d's step waits for its join, m's runs for
// agent k, and y's waits for k. The owner stops y and d: both are stopped and their rows hold no
// wait, and no step_stop is queued, since neither step was delivered: k's process goes on with m's
// step. m's step passes, which frees k: the settle starts nothing of y. The end of d's join, which
// its stop left behind, changes nothing: d stays stopped and its conflicts are not written. A later
// version that changes y and d starts each again at its first step (startAtFirstStep), each
// waiting for its join again, and y's step starts under k's claim once its join ends.
//
// The fault it catches is a stopped task whose step keeps its wait, and a stop that ends the
// process of another task's step (its controls: StopTask that keeps the wait, under which y's row
// still waits for k; and stopTask that queues its step_stop for a step that waits, under which a
// step_stop names k's claim).
func TestAStopOfAStepThatWaitsStartsNothingAndStopsNothingElse(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Admitted)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"c"}, state: record.TaskWaiting},
		planTask{id: "m", steps: []string{"tester:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
		planTask{id: "y", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskWaiting},
		planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"d", "m", "y"}, state: record.TaskWaiting})

	firstStepOf(t, pool, engine, root.Key)
	joinEnds(t, pool, engine, root.Key, "m")
	joinEnds(t, pool, engine, root.Key, "y")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "d", atStep(1), record.StepWaitJoin)
	assertWaits(t, tasks, "m", atStep(1), record.StepWaitNone)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitAgent)
	kStarts := len(startsOfAgent(t, root.Key, "k", roleStartsOf(t, pool, root.Key, claim.RoleTester), roleStartsOf(t, pool, root.Key, claim.RoleImplementer)))
	if kStarts != 1 {
		t.Fatalf("the starts of agent k = %d, want one, of m's step", kStarts)
	}

	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the owner's stop of y", apply("stop-y", ownersStop(t, root, "y")))
	mustSpend(t, "the owner's stop of d", apply("stop-d", ownersStop(t, root, "d")))

	tasks = tasksOf(t, pool, engine, root.Key)
	assertStoppedWaitingForNothing(t, tasks, "y", atStep(1))
	assertStoppedWaitingForNothing(t, tasks, "d", atStep(1))
	assertWaits(t, tasks, "m", atStep(1), record.StepWaitNone)
	if stops := superviseRowsOf(t, pool, root.Key, "step_stop"); len(stops) != 0 {
		t.Fatalf("the step stops of %s = %+v, want none: neither stopped step was delivered, and k runs m's step", root.Key, stops)
	}

	taskMoves(t, pool, engine, root.Key, "m", TriggerTesterPassed, "")

	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "m", record.TaskDone, atStep(1))
	assertStoppedWaitingForNothing(t, tasks, "y", atStep(1))
	assertNothingStarted(t, pool, root.Key, "y", atStep(1))
	if got := len(startsOfAgent(t, root.Key, "k", roleStartsOf(t, pool, root.Key, claim.RoleTester), roleStartsOf(t, pool, root.Key, claim.RoleImplementer))); got != kStarts {
		t.Fatalf("the starts of agent k = %d after m passed, want %d: k is free, and y is stopped", got, kStarts)
	}

	d := tasks["d"]
	mustSpend(t, "the end of d's join after its stop", apply("joined-d", intake.TaskJoined{Issue: root.Key, Generation: root.Generation, Task: "d",
		Branch: d.BranchOf(root.Key), Needs: d.Needs, Conflicts: []string{"c"}}))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertStoppedWaitingForNothing(t, tasks, "d", atStep(1))
	if conflicts := tasks["d"].JoinConflicts; len(conflicts) != 0 {
		t.Fatalf("stopped task d's join conflicts = %v after the end of its join, want none written", conflicts)
	}
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleReviewer); len(starts) != 0 {
		t.Fatalf("the reviewer's starts of %s = %+v, want none: d is stopped", root.Key, starts)
	}

	given := laterVersion(t, root.Key, root.Generation, 2,
		keyed(t, "c", "", []string{"implementer:c"}), keyed(t, "d", " Again.", []string{"reviewer"}, "c"),
		keyed(t, "m", "", []string{"tester:k"}), keyed(t, "y", " Again.", []string{"implementer:k"}),
		keyed(t, "z", "", []string{"implementer:z"}, "d", "m", "y"))
	mustTake(t, "version 2", apply("version-2", given))

	tasks = tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "d", atStep(1), record.StepWaitJoin)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitJoin)
	if rows := taskBranchRowsOf(t, pool, root.Key); !slices.Contains(rows, "y") || !slices.Contains(rows, "d") {
		t.Fatalf("the task_branch rows of %s name %v, want a join of y and of d queued by their starts again", root.Key, rows)
	}

	joinEnds(t, pool, engine, root.Key, "y")
	tasks = tasksOf(t, pool, engine, root.Key)
	assertWaits(t, tasks, "y", atStep(1), record.StepWaitNone)
	assertStartOf(t, newestStart(t, pool, root.Key, claim.RoleImplementer), root.Key, "y", "k")
}
