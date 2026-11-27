package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// returnHeldTask is the task LEGION-208's wait on its children holds for task a's implementer, as a
// transition composes one, and returnMerged the sentence with which the return names the child that
// merged during the wait.
const (
	returnHeldTask = "Implement LEGION-208. Task: a. Step 1 of 2: implementer. Do the held work of task a."
	returnMerged   = "These Dispatch children of LEGION-208 merged during the wait: LEGION-209 (pull request #43)"
)

// returnStart is the start of task a's first step, `implementer:k`, after LEGION-208 went back
// from awaiting_children (ReturnFromChildren), and the implementer's phases row after that start.
// LEGION-208 waits on its open child LEGION-209 holding the start of a's implementer, on the task
// returnHeldTask; LEGION-209 then merges and the return runs. Task m, `implementer:k`, holds k at
// its step where busy, and a's step waits for k until m passes; else m is done and a's step starts
// at once. z, which needs a and m, is of a role neither runs.
func returnStart(t *testing.T, busy bool) (record.SuperviseRequest, record.PhaseRow) {
	t.Helper()
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	m := planTask{id: "m", steps: []string{"implementer:k"}, agents: []claim.Agent{"k"}, state: record.TaskDone, place: atStep(1)}
	if busy {
		m.state = record.TaskRunning
	}
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:k", "reviewer"}, agents: []claim.Agent{"k", "reviewer"}, state: record.TaskRunning, place: atStep(1)},
		m,
		planTask{id: "z", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"a", "m"}, state: record.TaskWaiting})
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedTaskPR(t, pool, engine, root.Key, "a", 45)

	onTask(t, pool, engine, root.Key, "a", func(ctx context.Context, tx pgx.Tx, s TaskScope) error {
		return engine.awaitChildren(ctx, tx, s, childrenHold{Start: phase.Implementing, Task: returnHeldTask,
			Cause: "the implementer's start of task a waits on the child", Open: []record.OpenChild{{Key: "LEGION-209", Status: "todo"}}})
	})
	if waiting := issueOf(t, pool, engine, root.Key); waiting.Phase != phase.AwaitingChildren || waiting.ChildrenWait == nil {
		t.Fatalf("%s stands in %s holding %+v, want awaiting_children holding a's implementer", root.Key, waiting.Phase, waiting.ChildrenWait)
	}
	stepChild(t, pool, "LEGION-209", phase.Done, nil)
	seedChildPR(t, pool, record.PullRequestMerged)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		return engine.ReturnFromChildren(ctx, tx, *issue)
	})
	if busy {
		assertWaits(t, tasksOf(t, pool, engine, root.Key), "a", atStep(1), record.StepWaitAgent)
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 0 {
			t.Fatalf("the implementer's starts of %s after the return = %+v, want none: agent k does m's step", root.Key, starts)
		}
		taskMoves(t, pool, engine, root.Key, "m", TriggerImplementationReady, "")
		joinsEnd(t, pool, engine, root.Key)
	}
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "a", atStep(1), record.StepWaitNone)
	start := newestStart(t, pool, root.Key, claim.RoleImplementer)
	assertStartOf(t, start, root.Key, "a", "k")
	return start, storedPhaseRow(t, pool, root.Key, claim.RoleImplementer)
}

// A return out of awaiting_children whose step waits for its agent keeps what the return hands its
// start until the agent is free: LEGION-208 waits on its child holding the implementer's start of
// task a, whose agent k does task m's step when the child merges, so the return's start waits for
// k; m passes, and a's step starts for k. That start is told word for word what the return's start
// is told where k was free, the task the wait held and then the child that merged during the wait
// (returnTask), and its entry counts as raised by a move back out of awaiting_children
// (record.PhaseRow.EntryMovedBack, record.PhaseRow.EntryFrom), so the completion route's entry fence
// refuses a completion of the entry from before the wait whatever head it names.
//
// The fault it catches is a return that, once it has waited for its agent, starts as a step the
// task reached going forward: told neither the held task nor the children that merged, and its
// entry fence taking the completion of the entry before the wait at the head the record holds (its
// control: the same return where k is free, whose start is told the return's task and raises its
// entry as a move back at once).
func TestAReturnFromChildrenWhoseStepWaitedForItsAgentKeepsItsTaskAndItsMoveBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		busy bool
	}{
		{"its agent free (the control)", false},
		{"its agent busy with another task's step", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, row := returnStart(t, tc.busy)
			told := strings.Contains(start.Task, returnHeldTask) && strings.Contains(start.Task, returnMerged)
			if !told || !row.EntryMovedBack || row.EntryFrom != phase.AwaitingChildren {
				t.Fatalf("the implementer's start after the return is told the held task and the merged child %t, and its entry is marked moved back %t from %q;"+
					" want the return's own task, %q then %q, and an entry raised by a move back out of awaiting_children. The start is told %q",
					told, row.EntryMovedBack, row.EntryFrom, returnHeldTask, returnMerged, start.Task)
			}
		})
	}
	at, _ := returnStart(t, false)
	waited, _ := returnStart(t, true)
	if waited.Task != at.Task {
		t.Fatalf("the start after the wait for k is told %q,\nwant what the start where k was free is told: %q", waited.Task, at.Task)
	}
}
