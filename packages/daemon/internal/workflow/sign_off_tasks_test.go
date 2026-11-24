package workflow

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The owner names the task its sign-off and its move back are of: tasks a and b each stand at
// their owner's acceptance, `implementer acceptance:owner tester`, c implements, and d, which
// needs all three, waits. A sign-off that names no task is refused TASK_REQUIRED, naming a and b,
// and one that names c, which implements, TASK_NOT_AT_PHASE; neither moves anything. The sign-off
// of a moves a to its tester and leaves b at its acceptance. A move back of a, now testing, is
// refused TASK_NOT_AT_PHASE, and a move back that names no task is of b, the one task left at
// its acceptance, which returns to its implementer while a tests.
//
// The fault it catches is an owner's call that acts on whichever task the issue's own phase reads
// (its control: signOff of the read tree, on the issue's own phase, implementing while c
// implements, so the call that names no task is refused SIGNOFF_OUTSIDE_ACCEPTING, not
// TASK_REQUIRED).
func TestASignOffNamesItsTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	accepting := []string{"implementer", "acceptance:owner", "tester"}
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: accepting, agents: []claim.Agent{"implementer", "", "tester"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: accepting, agents: []claim.Agent{"implementer-2", "", "tester-2"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-3"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "d", steps: []string{"reviewer"}, agents: []claim.Agent{"reviewer"}, needs: []string{"a", "b", "c"}, state: record.TaskWaiting})
	seedTaskPRs(t, pool, root.Key, "a", "b")
	apply := applyFacts(t, pool, engine)

	signOff := rootsSignOff(t, pool, engine, root.Key)
	mustRefuse(t, "a sign-off that names no task", apply("sign-off-none", signOff),
		"TASK_REQUIRED", "tasks a, b of LEGION-208 each stand at accepting: name the task with task.")
	signOff.Task = "c"
	mustRefuse(t, "a sign-off of c", apply("sign-off-c", signOff),
		"TASK_NOT_AT_PHASE", "task c of LEGION-208 stands at implementing, not at accepting.")
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(2))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(2))

	signOff = rootsSignOff(t, pool, engine, root.Key)
	signOff.Task = "a"
	mustSpend(t, "the sign-off of a", apply("sign-off-a", signOff))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "a", record.TaskRunning, atStep(3))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(2))

	back := rootsMoveBack(t, pool, engine, root.Key, phase.Implementing, "the evidence does not cover the migration")
	back.Task = "a"
	mustRefuse(t, "a move back of a", apply("back-a", back),
		"TASK_NOT_AT_PHASE", "task a of LEGION-208 stands at testing, not at accepting.")
	back.Task = ""
	mustSpend(t, "a move back that names no task", apply("back-none", back))
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "a", record.TaskRunning, atStep(3))
}
