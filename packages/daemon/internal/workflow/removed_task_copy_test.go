package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// copyRemovals is each row of the outbox that removes the working copy of a removed task
// (record.TaskCopyRemove), in the order it was queued.
func copyRemovals(t *testing.T, pool *pgxpool.Pool) []record.TaskCopyRemove {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select payload from outbox where kind = $1 order by id`, string(record.OutboxKindTaskCopyRemove))
	var payloads [][]byte
	if err == nil {
		payloads, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
	}
	if err != nil {
		t.Fatalf("read the rows that remove a task's working copy: %v", err)
	}
	removals := make([]record.TaskCopyRemove, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &removals[i]); err != nil {
			t.Fatalf("decode a row that removes a task's working copy: %v", err)
		}
	}
	return removals
}

// A task that a later version removes has its working copy removed: one row, queued in the
// version's transaction, names the branch it held. Task k runs its tester's step on
// legion/LEGION-208.k, and z, the task that merges, waits on it; a, an implementer's task, stopped
// on the issue's own branch, b, a reviewer's, stopped on legion/LEGION-208.b, and w waits, never
// started, with no branch. Version 2 removes a, b and w: one row removes b's working copy, and
// none names a's or w's. a held the issue's own branch, whose working copy is the issue's own and
// goes only with the issue's removal; w never started, so it has none. Version 3 adds b back: b
// starts on legion/LEGION-208.b.2, a branch of its own, and its start queues no removal.
//
// The fault it catches is the working copy of a removed task left on the daemon's host for ever
// (its control: writeVersion without the row, so the count of rows fails), and the removal of the
// issue's own working copy (its control: the row queued for a task on the issue's branch too,
// which the row's own check refuses, record.TaskCopyRemove, so the version fails).
func TestARemovedTaskQueuesTheRemovalOfItsWorkingCopy(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskStopped, place: atStep(1)},
		planTask{id: "b", steps: []string{"reviewer:b"}, agents: []claim.Agent{"rev-b"}, state: record.TaskStopped, place: atStep(1)},
		planTask{id: "k", steps: []string{"tester:k"}, agents: []claim.Agent{"test-k"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "w", steps: []string{"implementer:w"}, agents: []claim.Agent{"impl-w"}, state: record.TaskWaiting},
		planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"k"}, state: record.TaskWaiting})
	// Each task holds its branch, and the generation records each name it gave (plan_branches), as
	// the starts of a, b and k left them (record.Store.AssignTaskBranch).
	for _, task := range []struct{ id, branch string }{{"a", "legion/LEGION-208"}, {"b", "legion/LEGION-208.b"}, {"k", "legion/LEGION-208.k"}} {
		seedTaskBranch(t, pool, root.Key, task.id, task.branch)
		if _, err := pool.Exec(t.Context(), `insert into plan_branches (issue, generation, branch) values ($1, $2, $3)`,
			root.Key, int64(root.Generation), task.branch); err != nil {
			t.Fatalf("record that the generation gave %s: %v", task.branch, err)
		}
	}
	apply := applyFacts(t, pool, engine)
	kBlock, zBlock := keyed(t, "k", "", []string{"tester:k"}), keyed(t, "z", "", []string{"reviewer:z"}, "k")

	mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, kBlock, zBlock)))
	want := []record.TaskCopyRemove{{Generation: root.Generation, Task: "b", Branch: "legion/LEGION-208.b"}}
	if removals := copyRemovals(t, pool); !reflect.DeepEqual(removals, want) {
		t.Fatalf("the rows that remove a task's working copy after version 2 removed a, b and w = %+v, want the one %+v", removals, want)
	}
	if tasks := tasksOf(t, pool, engine, root.Key); len(tasks) != 2 {
		t.Fatalf("the tasks of %s after version 2 = %+v, want k and z", root.Key, tasks)
	}

	mustTake(t, "version 3", apply("version-3", laterVersion(t, root.Key, root.Generation, 3,
		keyed(t, "b", "", []string{"reviewer:b"}), kBlock, zBlock)))
	if again := tasksOf(t, pool, engine, root.Key)["b"]; again.State != record.TaskRunning || again.Branch != "legion/LEGION-208.b.2" {
		t.Fatalf("task b added back = %+v, want a new task running on legion/LEGION-208.b.2", again)
	}
	if removals := copyRemovals(t, pool); !reflect.DeepEqual(removals, want) {
		t.Fatalf("the rows that remove a task's working copy after version 3 added b back = %+v, want version 2's one %+v alone", removals, want)
	}
}

// A plan of one task queues no removal of a working copy when a later version replaces that task:
// the one task of a plan holds the issue's own branch, whose working copy is the issue's own. Task
// x, an implementer's, runs on legion/LEGION-208 and its owner stops it; version 2 gives task y in
// its place.
func TestAPlanOfOneTaskQueuesNoRemovalOfAWorkingCopy(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "x", steps: []string{"implementer:x"}, agents: []claim.Agent{"impl-x"}, state: record.TaskRunning, place: atStep(1)})
	seedTaskBranch(t, pool, root.Key, "x", "legion/LEGION-208")
	apply := applyFacts(t, pool, engine)
	if result := apply("stop-x", ownersStop(t, root, "x")); result.Refusal != nil {
		t.Fatalf("the owner's stop of x was refused: %+v", result.Refusal)
	}

	mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, keyed(t, "y", "", []string{"implementer:y"}))))
	if _, ok := tasksOf(t, pool, engine, root.Key)["x"]; ok {
		t.Fatalf("the plan of %s holds x after version 2 replaced it", root.Key)
	}
	if removals := copyRemovals(t, pool); len(removals) != 0 {
		t.Fatalf("the rows that remove a task's working copy after version 2 replaced the one task x = %+v, want none", removals)
	}
}
