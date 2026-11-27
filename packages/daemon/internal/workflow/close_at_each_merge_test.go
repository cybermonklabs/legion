package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// mergingRoot is the root LEGION-208 standing in merging at generation 1, its tree open, with no
// pull request recorded: each pull request is one its test seeds for a task.
func mergingRoot(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedTreeLifecycle(t, pool, root.Key)
	return root
}

// closesAre fails unless the closes of pull requests key's outbox holds are want, oldest first.
func closesAre(t *testing.T, pool *pgxpool.Pool, key, after string, want ...record.TaskPullRequestsClose) {
	t.Helper()
	if got := closesOf(t, pool, key); !reflect.DeepEqual(got, want) {
		t.Fatalf("the closes of pull requests of %s after %s = %+v, want %+v", key, after, got, want)
	}
}

// Each merge closes the open pull request of each task whose work it holds, as soon as it lands,
// and the merge that ends the issue closes each other open pull request of the issue. Of a, which
// needs none and is done at its last step with #1 open, b, which needs a and merges by itself, and
// c, which needs none and merges by itself: b's merge #2 queues one close, of #1, with `Merged in
// #2.`, while c runs with #3 open, which stays open. A person then reopens #1, closed by that row,
// and c's merge #3, which ends the issue, queues one close, of #1, with `Issue LEGION-208 is done;
// its last merge is #3.`. Where a is needed by both b and c, b's merge closes #1, and c's merge,
// once #1 is closed, queues none.
//
// The fault it catches is a pull request of a task that does not merge by itself left open after
// its work is on the default branch, and closed later with a comment that names a merge that does
// not hold its work (its control: the close at the merge that ends the issue alone, under which b's
// merge queues none, and c's queues #1 with `Merged in #3.`).
func TestEachMergeClosesThePullRequestsWhoseWorkItHolds(t *testing.T) {
	closedAt, reopenedAt := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 24, 10, 5, 0, 0, time.UTC)
	closeOfA := intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 1, HeadSHA: "head-a", UpdatedAt: closedAt}

	t.Run("a task one merge holds, reopened before the last merge", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := mergingRoot(t, pool)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", state: record.TaskDone, place: atStep(1), number: 1},
			taskAt{id: "b", needs: []string{"a"}, state: record.TaskRunning, place: atMerge, number: 2},
			taskAt{id: "c", state: record.TaskRunning, place: atMerge, number: 3})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the merge of b's #2", apply("merged-b", mergedFact(2, "b")))
		byB := record.TaskPullRequestsClose{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: "Merged in #2."}
		// The steps after b's merge do not stand on its row, so a missing row is reported and the
		// test goes on to c's merge, whose row then shows what a merge that holds no work closes.
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, []record.TaskPullRequestsClose{byB}) {
			t.Errorf("the closes of pull requests of %s after b's merge = %+v, want %+v", root.Key, got, []record.TaskPullRequestsClose{byB})
		}
		if pr := pullRequestOfTask(t, pool, root.Key, "c"); pr.State != record.PullRequestOpen {
			t.Fatalf("c's #3 after b's merge is %s, want open: b's merge holds none of c's work", pr.State)
		}
		mustSpend(t, "the close of #1", apply("closed-1", closeOfA))
		mustSpend(t, "a person's reopen of #1", apply("reopened-1", openedOn(1, "legion/LEGION-208", reopenedAt, true)))
		if pr := pullRequestOfTask(t, pool, root.Key, "a"); pr.Number != 1 || pr.State != record.PullRequestOpen {
			t.Fatalf("premise: a's pull request after the reopen = #%d %s, want #1 open", pr.Number, pr.State)
		}
		mustSpend(t, "the merge of c's #3", apply("merged-c", mergedFact(3, "c")))
		closesAre(t, pool, root.Key, "c's merge, which ends the issue", byB,
			record.TaskPullRequestsClose{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: "Issue LEGION-208 is done; its last merge is #3."})
	})

	t.Run("a task two merges hold", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := mergingRoot(t, pool)
		seedTasksAt(t, pool, root.Key, root.Generation,
			taskAt{id: "a", state: record.TaskDone, place: atStep(1), number: 1},
			taskAt{id: "b", needs: []string{"a"}, state: record.TaskRunning, place: atMerge, number: 2},
			taskAt{id: "c", needs: []string{"a"}, state: record.TaskRunning, place: atMerge, number: 3})
		apply := applyFacts(t, pool, engine)
		mustSpend(t, "the merge of b's #2", apply("merged-b", mergedFact(2, "b")))
		byB := record.TaskPullRequestsClose{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: "Merged in #2."}
		closesAre(t, pool, root.Key, "b's merge", byB)
		mustSpend(t, "the close of #1", apply("closed-1", closeOfA))
		mustSpend(t, "the merge of c's #3", apply("merged-c", mergedFact(3, "c")))
		closesAre(t, pool, root.Key, "c's merge, once #1 is closed", byB)
	})
}

// A merge that lands while its task stands short of its merge queues its close when the task
// reaches its merge on that merged pull request. Of a, which needs none and is done at its last step
// with #1 open, c, which needs a and merges, and d, which needs none and merges, at its first step
// with #4 open: c's #3 merges while c stands at its retro, which queues no close, since c stands at
// no merge. c's retro passes and c reaches its merge on its merged pull request: one close, of #1,
// the pull request of the task c's merge holds, with `Merged in #3.`. With d merged and done first,
// c's merge ends the issue: one close, of #1, with `Issue LEGION-208 is done; its last merge is
// #3.`.
//
// The fault it catches is a merge before merging that closes nothing, so the pull request of a task
// whose work it holds stays open for good (its control: the close queued only where the task awaits
// its merge when the merge lands, under which c's arrival at its merge queues none).
func TestAMergeBeforeMergingQueuesItsCloseRowWhenItsTaskReachesMerging(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       taskAt
		comment string
	}{
		{"d still running", taskAt{id: "d", state: record.TaskRunning, place: atStep(1), number: 4}, "Merged in #3."},
		{"d merged and done first", taskAt{id: "d", state: record.TaskDone, place: atMerge, number: 4, merged: true}, "Issue LEGION-208 is done; its last merge is #3."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := mergingRoot(t, pool)
			seedTasksAt(t, pool, root.Key, root.Generation,
				taskAt{id: "a", state: record.TaskDone, place: atStep(1), number: 1},
				taskAt{id: "c", needs: []string{"a"}, state: record.TaskRunning, place: atRetro, number: 3},
				tc.d)
			mustSpend(t, "the merge of c's #3", applyFacts(t, pool, engine)("merged-c", mergedFact(3, "c")))
			closesAre(t, pool, root.Key, "c's merge while c stands at its retro")
			taskMoves(t, pool, engine, root.Key, "c", TriggerRetroCompleted, "")
			assertTask(t, tasksOf(t, pool, engine, root.Key), "c", record.TaskDone, atMerge)
			closesAre(t, pool, root.Key, "c's arrival at its merge on its merged pull request",
				record.TaskPullRequestsClose{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: tc.comment})
		})
	}
}
