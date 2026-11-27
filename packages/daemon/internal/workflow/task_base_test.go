package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// dNeedsAC is LEGION-208 with a and c done, not merging, on branches of their own, and d
// (`implementer reviewer`), needing both, started by startReady with its join ended, and with its
// pull request #45 open; withE adds e (`tester`), which needs d, so d does not merge. It answers the
// blocks of a later version of the plan as it stands, extra added.
func dNeedsAC(t *testing.T, withE bool) (*pgxpool.Pool, *Engine, record.Issue, func(extra ...plan.Task) []plan.Task) {
	t.Helper()
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Implementing)
	tasks := []planTask{
		{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskDone, place: atStep(1)},
		{id: "c", steps: []string{"implementer:c"}, agents: []claim.Agent{"impl-c"}, state: record.TaskDone, place: atStep(1)},
		{id: "d", steps: []string{"implementer:d", "reviewer:dr"}, agents: []claim.Agent{"impl-d", "rev-d"}, needs: []string{"a", "c"}, state: record.TaskWaiting},
	}
	if withE {
		tasks = append(tasks, planTask{id: "e", steps: []string{"tester:e"}, agents: []claim.Agent{"test-e"}, needs: []string{"d"}, state: record.TaskWaiting})
	}
	seedTasks(t, pool, root.Key, root.Generation, tasks...)
	seedTaskBranch(t, pool, root.Key, "a", "legion/"+root.Key+".a")
	seedTaskBranch(t, pool, root.Key, "c", "legion/"+root.Key+".c")
	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.startReadyNow(ctx, tx, issue.Key)
	})
	joinsEnd(t, pool, engine, root.Key)
	// stepRoot's #42 stands on the issue's own branch, which d holds. Under the key of the issue and
	// the branch d's #45 took its place; each pull request is a row of its own now, so the rig
	// deletes #42, and d's branch holds #45 alone.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().DeletePullRequest(t.Context(), tx, "sjawhar/legion", 42)
	})
	seedTaskPR(t, pool, engine, root.Key, "d", 45)
	blocks := func(extra ...plan.Task) []plan.Task {
		return append([]plan.Task{keyed(t, "a", "", []string{"implementer:a"}), keyed(t, "c", "", []string{"implementer:c"}),
			keyed(t, "d", "", []string{"implementer:d", "reviewer:dr"}, "a", "c")}, extra...)
	}
	return pool, engine, root, blocks
}

// baseOfD is plan_tasks.base of d as the store holds it now.
func baseOfD(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string) string {
	t.Helper()
	return tasksOf(t, pool, engine, key)["d"].Base
}

// A version that changes whether a running task merges keeps the base its start fixed. (1) d needs
// a and c, which are done and do not merge, and e needs d: d's start fixes its join branch
// legion/LEGION-208.join (d, the first task of the generation to start, holds the issue's own
// branch). Version 2 removes e while d reviews, so d now merges and the plan alone
// would base it on main; d's review fails, and its implementer's start text still names the join
// branch. (2) The same d with no e: its start fixes main. Version 2 adds f, which needs d, so the
// plan alone would base it on its join branch; d's review fails, and its implementer's start text
// holds no base sentence, and its base is still main: a move back to a first step writes no base.
// (3) d is stopped, and version 3 changes it: its start again fixes the base the plan gives now,
// its join branch.
//
// The fault it catches is a version that moves a running task's base by its agent's hand, to a
// branch no start of it made (its control: BaseOf as PlannedBase alone, the revision before, under
// which case (1)'s text names no base and case (2)'s names legion/LEGION-208.join).
func TestAVersionThatChangesARunningTasksMergeKeepsItsBase(t *testing.T) {
	const joined = "Open the pull request of this task against legion/LEGION-208.join"
	t.Run("a version makes d merge", func(t *testing.T) {
		pool, engine, root, blocks := dNeedsAC(t, true)
		if base := baseOfD(t, pool, engine, root.Key); base != "legion/LEGION-208.join" {
			t.Fatalf("the base d's start fixed = %q, want its join branch", base)
		}
		taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
		mustTake(t, "version 2, which removes e while d reviews", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2, blocks()...)))
		taskMoves(t, pool, engine, root.Key, "d", TriggerReviewRejected, changesBody)
		if start := newestStart(t, pool, root.Key, claim.RoleImplementer); !strings.Contains(start.Task, joined) {
			t.Fatalf("d's implementer's start after the review failed = %q, want it to name %q", start.Task, joined)
		}
	})

	t.Run("a version makes d no longer merge, and d starts again", func(t *testing.T) {
		pool, engine, root, blocks := dNeedsAC(t, false)
		if base := baseOfD(t, pool, engine, root.Key); base != "main" {
			t.Fatalf("the base d's start fixed = %q, want main", base)
		}
		taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
		f := keyed(t, "f", "", []string{"tester:f"}, "d")
		apply := applyFacts(t, pool, engine)
		mustTake(t, "version 2, which adds f needing d while d reviews", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, blocks(f)...)))
		taskMoves(t, pool, engine, root.Key, "d", TriggerReviewRejected, changesBody)
		if start := newestStart(t, pool, root.Key, claim.RoleImplementer); strings.Contains(start.Task, toldABase) {
			t.Fatalf("d's implementer's start after the review failed = %q, want no base sentence: d's base is still main", start.Task)
		}
		if base := baseOfD(t, pool, engine, root.Key); base != "main" {
			t.Fatalf("d's base after the move back = %q, want main: a move back writes no base", base)
		}

		if result := apply("stop-d", ownersStop(t, root, "d")); result.Refusal != nil {
			t.Fatalf("the owner's stop of d was refused: %+v", result.Refusal)
		}
		dAgain := keyed(t, "d", " Again.", []string{"implementer:d", "reviewer:dr"}, "a", "c")
		mustTake(t, "version 3, which changes the stopped d", apply("version-3", laterVersion(t, root.Key, root.Generation, 3,
			keyed(t, "a", "", []string{"implementer:a"}), keyed(t, "c", "", []string{"implementer:c"}), dAgain, f)))
		if base := baseOfD(t, pool, engine, root.Key); base != "legion/LEGION-208.join" {
			t.Fatalf("d's base after its start again = %q, want the join branch the plan gives now", base)
		}
	})
}
