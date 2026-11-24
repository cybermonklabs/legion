package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// rowsOfTask is how many task_branch rows of key name task.
func rowsOfTask(t *testing.T, pool *pgxpool.Pool, key, task string) int {
	t.Helper()
	count := 0
	for _, named := range taskBranchRowsOf(t, pool, key) {
		if named == task {
			count++
		}
	}
	return count
}

// mergeBaseSetBy applies the end of the row of task's merge with the default branch main as its base
// (intake.TaskJoined.Base), on the branch and with the needs task's row holds now, as the outbox's
// executor applies it once it set the base of the task's pull request.
func mergeBaseSetBy(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string) {
	t.Helper()
	issue := issueOf(t, pool, engine, key)
	row := tasksOf(t, pool, engine, key)[task]
	mustSpend(t, "the end of the row of the merge of "+task, applyFacts(t, pool, engine)(fmt.Sprintf("merge-base:%s:%s", key, task),
		intake.TaskJoined{Issue: key, Generation: issue.Generation, Task: task, Branch: row.BranchOf(key), Needs: row.Needs, Base: "main"}))
}

// assertMergeWaitsForBase fails unless task of key stands at its merge waiting for the move of its
// pull request's base, with rows task_branch rows of it, and no start of the merger.
func assertMergeWaitsForBase(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, rows int) {
	t.Helper()
	assertWaits(t, tasksOf(t, pool, engine, key), task, atMerge, record.StepWaitJoin)
	if got := rowsOfTask(t, pool, key, task); got != rows {
		t.Fatalf("the task_branch rows of task %s of %s = %d, want %d", task, key, got, rows)
	}
	if starts := roleStartsOf(t, pool, key, claim.RoleMerger); len(starts) != 0 {
		t.Fatalf("the merger's starts of %s = %+v, want none: the merge waits for its base", key, starts)
	}
}

// assertMergerStarts fails unless task of key stands at its merge waiting for nothing, its pull
// request number and its row read the base main, and the merger is started once.
func assertMergerStarts(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, task string, number int) {
	t.Helper()
	tasks := tasksOf(t, pool, engine, key)
	assertWaits(t, tasks, task, atMerge, record.StepWaitNone)
	if tasks[task].Base != "main" {
		t.Fatalf("the base of task %s of %s = %q, want main", task, key, tasks[task].Base)
	}
	if pr := pullRequestOfTask(t, pool, key, task); pr.Number != number || pr.Base != "main" {
		t.Fatalf("the pull request of task %s of %s = #%d against %q, want #%d against main", task, key, pr.Number, pr.Base, number)
	}
	if starts := roleStartsOf(t, pool, key, claim.RoleMerger); len(starts) != 1 {
		t.Fatalf("the merger's starts of %s = %+v, want one", key, starts)
	}
}

// A version that makes a running task merge moves its pull request's base once, at its merge's
// start. d needs a and c, which are done and do not merge, and e needs d: d's start fixed its join
// branch, and its pull request #45 is against it. Version 2 removes e while d reviews, so d now
// merges. d's review fails: d is back at its implementer, and no task_branch row of d is queued
// beyond its start's. Its implementer and its reviewer pass, and its retro starts at once. Its retro
// passes: d stands at its merge waiting for its base, with one more task_branch row and no start of
// the merger. The row's end, with the base main, records main on #45 and on d, ends the wait and
// starts the merger.
//
// The faults it catches are the merger merging d's pull request into its join branch, so d's work
// never reaches main (its control: the merge with no wait for its base, under which the merger
// starts at d's move to its merge), and a base moved within the run, at a move back (its control:
// the base check at every act's start, under which d's move back to its implementer queues a row).
func TestAVersionThatMakesARunningTaskMergeMovesItsBaseOnceAtItsMerge(t *testing.T) {
	pool, engine, root, blocks := dNeedsAC(t, true)
	pr := pullRequestOfTask(t, pool, root.Key, "d")
	pr.Base = "legion/LEGION-208.join"
	seedPR(t, pool, pr)
	started := rowsOfTask(t, pool, root.Key, "d")

	taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
	mustTake(t, "version 2, which removes e while d reviews", applyFacts(t, pool, engine)("version-2", laterVersion(t, root.Key, root.Generation, 2, blocks()...)))
	taskMoves(t, pool, engine, root.Key, "d", TriggerReviewRejected, changesBody)
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atStep(1), record.StepWaitNone)
	if got := rowsOfTask(t, pool, root.Key, "d"); got != started {
		t.Fatalf("the task_branch rows of d after its move back = %d, want %d: a move back moves no base", got, started)
	}

	taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
	taskMoves(t, pool, engine, root.Key, "d", TriggerReviewApproved, "")
	assertWaits(t, tasksOf(t, pool, engine, root.Key), "d", atRetro, record.StepWaitNone)
	if starts := actStarts(t, pool, root.Key, "d", record.PlaceRetro); starts != 1 {
		t.Fatalf("the retro of d counts %d starts, want 1: it starts at once", starts)
	}

	taskMoves(t, pool, engine, root.Key, "d", TriggerRetroCompleted, "")
	assertMergeWaitsForBase(t, pool, engine, root.Key, "d", started+1)

	mergeBaseSetBy(t, pool, engine, root.Key, "d")
	assertMergerStarts(t, pool, engine, root.Key, "d", 45)
}

// A version that makes a running task no longer merge moves no base: d needs a and c and no task
// needs it, so its start fixed main, and its pull request #45 is against main. Version 2 adds f,
// which needs d, while d reviews. d's reviewer passes: d is done at its last step, with no retro, no
// merge and no task_branch row of d beyond its start's, and f starts.
func TestAVersionThatMakesARunningTaskNonMergingMovesNoBase(t *testing.T) {
	pool, engine, root, blocks := dNeedsAC(t, false)
	pr := pullRequestOfTask(t, pool, root.Key, "d")
	pr.Base = "main"
	seedPR(t, pool, pr)
	started := rowsOfTask(t, pool, root.Key, "d")

	taskMoves(t, pool, engine, root.Key, "d", TriggerImplementationReady, "")
	mustTake(t, "version 2, which adds f needing d while d reviews", applyFacts(t, pool, engine)("version-2",
		laterVersion(t, root.Key, root.Generation, 2, blocks(keyed(t, "f", "", []string{"tester:f"}, "d"))...)))
	taskMoves(t, pool, engine, root.Key, "d", TriggerReviewApproved, "")

	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "d", record.TaskDone, atStep(2))
	if retro, merge := actStarts(t, pool, root.Key, "d", record.PlaceRetro), actStarts(t, pool, root.Key, "d", record.PlaceMerge); retro != 0 || merge != 0 {
		t.Fatalf("task d counts %d starts of its retro and %d of its merge, want none", retro, merge)
	}
	if got := rowsOfTask(t, pool, root.Key, "d"); got != started {
		t.Fatalf("the task_branch rows of d = %d, want %d: no base moved", got, started)
	}
	if tasks["f"].State != record.TaskRunning {
		t.Fatalf("task f once d is done = %+v, want it started", tasks["f"])
	}
}

// The base of the pull request of an issue with plan rows moves at its merge's start whatever the
// plan's count of tasks, and an issue with no plan rows keeps the base its implementer gave it. (1)
// The one task a of a plan of one, `implementer reviewer`, at its retro, its pull request #42 against
// release: its retro passes, its merge waits for its base with one task_branch row and no start of
// the merger, and the row's end starts the merger with #42 against main. (2) An issue with no plan
// rows at its retro, its pull request #42 stacked on legion/LEGION-208-base: its retro passes and the
// merger starts at once, with no wait, no task_branch row and #42 still against its stack.
//
// The faults it catches are a pull request of a plan of one task merged into a branch that is not
// main (its control: the wait of a plan of several tasks alone, the count of tasks back, which the
// base's gate of a count of tasks also refuses), and a base check that undoes a stack an implementer
// made on purpose (its control: the check with no read of plan rows, under which (2) waits).
func TestTheBaseOfAnIssueWithPlanRowsMovesAtItsMerge(t *testing.T) {
	t.Run("a plan of one task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Retro)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atRetro})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", Base: "release", HeadSHA: "head", Failing: []string{}})

		taskMoves(t, pool, engine, root.Key, "a", TriggerRetroCompleted, "")
		assertMergeWaitsForBase(t, pool, engine, root.Key, "a", 1)
		mergeBaseSetBy(t, pool, engine, root.Key, "a")
		assertMergerStarts(t, pool, engine, root.Key, "a", 42)
	})

	t.Run("an issue with no plan rows keeps its stacked base", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := stepRoot(t, pool, phase.Retro)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", Base: "legion/LEGION-208-base", HeadSHA: "head", Failing: []string{}})

		move(t, pool, engine, root.Key, TriggerRetroCompleted, record.TaskPlace{}, record.PhaseRow{})
		if at := issueOf(t, pool, engine, root.Key); at.Phase != phase.Merging {
			t.Fatalf("%s after its retro stands in %s, want merging", root.Key, at.Phase)
		}
		if rows := taskBranchRowsOf(t, pool, root.Key); len(rows) != 0 {
			t.Fatalf("the task_branch rows of %s = %v, want none: an issue with no plan rows moves no base", root.Key, rows)
		}
		if starts := roleStartsOf(t, pool, root.Key, claim.RoleMerger); len(starts) != 1 {
			t.Fatalf("the merger's starts of %s = %+v, want one at once", root.Key, starts)
		}
		var pr *record.PullRequest
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			pr, err = engine.store.MergePullRequest(ctx, tx, root.Key)
			return err
		})
		if pr == nil || pr.Base != "legion/LEGION-208-base" {
			t.Fatalf("the pull request of %s after its merge started = %+v, want it still against legion/LEGION-208-base", root.Key, pr)
		}
	})
}
