package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The first step of a task is told each join conflict with the branch the task starts from that
// holds the need's work (record.StoredPlan.StartsFrom). a is done at its last step, b is done at
// its merge, and d needs both: the merge of main, which holds b's work, into d's join branch
// conflicted, so d is told to join main@origin there, not b's branch, which GitHub can have deleted
// at b's merge. A conflict the row holds of a task that is no need now, which no join of the task's
// needs made, is not told: f needs a alone, and its row holds a conflict of c.
//
// The faults it catches are a conflict told with a merged need's branch, and a conflict told that
// no join of the task's needs made (its control: each conflict's branch read from the need's row, as
// planToldOf read it, under which d is told legion/LEGION-208.b@origin and f is told to join c).
func TestAJoinConflictIsToldWithTheBranchThatHoldsTheNeedsWork(t *testing.T) {
	pool := migratedPool(t)
	engine := textEngine()
	root := stepRoot(t, pool, phase.Implementing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, state: record.TaskDone, place: record.TaskPlace{Kind: record.PlaceMerge}},
		planTask{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "d", steps: []string{"implementer:d"}, agents: []claim.Agent{"impl-d"}, needs: []string{"a", "b"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "f", steps: []string{"implementer:f"}, agents: []claim.Agent{"impl-f"}, needs: []string{"a"}, state: record.TaskRunning, place: atStep(1)},
		planTask{id: "z", steps: []string{"implementer:z"}, agents: []claim.Agent{"impl-z"}, needs: []string{"c", "d", "f"}, state: record.TaskWaiting})
	seedBranches(t, pool, root.Key, "a", "b", "c", "d", "f")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.SetJoinConflicts(t.Context(), tx, root.Key, root.Generation, "d", []string{"b"}); err != nil {
			return err
		}
		return store.SetJoinConflicts(t.Context(), tx, root.Key, root.Generation, "f", []string{"c"})
	})

	d := stepLines(t, pool, engine, root.Key, "d", atStep(1))
	const repair = " GitHub could not join the work of task b into the join branch legion/LEGION-208.d.join, the base of this task's pull request:" +
		" their changes conflict."
	const join = "`jj -R /work/LEGION-208.d new legion/LEGION-208.d.join@origin main@origin -m \"LEGION-208: join the work of task b\"`"
	if !strings.Contains(d, repair) || !strings.Contains(d, join) {
		t.Fatalf("the first step of d's task = %q,\nwant it to hold %q and %q", d, repair, join)
	}
	lacks(t, "first step of d", d, "legion/LEGION-208.b@origin")
	lacks(t, "first step of f", stepLines(t, pool, engine, root.Key, "f", atStep(1)), "GitHub could not join")
}
