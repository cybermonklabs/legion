package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAnEarlierJoinCannotEndTheWaitForANewerJoinOfTheSameTask(t *testing.T) {
	pool := migratedPool(t)
	engine, _ := tickingEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "c", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"c"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "d", "legion/"+root.Key+".d")
	joinWaits(t, pool, root.Key, root.Generation, "d")
	queue := func() int64 {
		t.Helper()
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			return engine.enqueue(ctx, tx, root.Key, record.TaskBranch{Generation: root.Generation, Task: "d"})
		})
		var id int64
		if err := pool.QueryRow(t.Context(), `select max(id) from outbox where issue = $1 and kind = 'task_branch'`, root.Key).Scan(&id); err != nil || id <= 0 {
			t.Fatalf("read the join attempt: id %d, error %v", id, err)
		}
		return id
	}
	old := joinedFact(t, pool, root.Key, root.Generation)
	old.Row = queue()
	current := old
	current.Row = queue()
	if current.Row <= old.Row {
		t.Fatalf("the newer join's row %d is not above %d", current.Row, old.Row)
	}
	apply := applyFacts(t, pool, engine)
	mustSpend(t, "the old join", apply("joined:old", old))
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitJoin)
	assertNothingStarted(t, pool, root.Key, "d", atStep(1))
	mustSpend(t, "the current join", apply("joined:current", current))
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitNone)
	if starts := roleStartsOf(t, pool, root.Key, claim.RoleImplementer); len(starts) != 1 {
		t.Fatalf("the current join wrote %d starts, want exactly one", len(starts))
	}
}
