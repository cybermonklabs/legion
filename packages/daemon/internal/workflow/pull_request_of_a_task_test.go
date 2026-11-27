package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// tasksRoot is the root LEGION-208 standing in testing at generation 1, its tree open, with no
// pull request recorded.
func tasksRoot(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedTreeLifecycle(t, pool, root.Key)
	return root
}

// openedOn is GitHub's open of pull request number on branch of sjawhar/legion at at, or its
// reopen, its head a branch of sjawhar/legion itself.
func openedOn(number int, branch string, at time.Time, reopened bool) intake.PullRequestOpened {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: number, Branch: branch, Base: "main", HeadSHA: "head-" + branch, HeadRepo: "sjawhar/legion",
		UpdatedAt: at, Reopened: reopened}
}

// A pull request is the issue's only on a branch a task of its plan was given and holds
// (record.StoredPlan.HoldsBranch): the workflow records no other, and the merge of the pull request
// of the task that merges closes no other. A person's or a fork's pull request whose head is named
// legion/LEGION-208.foo, of a task's shape, which no task holds, is not recorded. One on
// legion/LEGION-208, while no task was given that branch, is not recorded either: the task b was
// stopped before its first step, so it was given no branch, and a took legion/LEGION-208.a. And a
// row on a branch no task holds, which the record holds all the same, is not closed by the merge
// of the task that merges, at its merge, which ends the issue: only the other task's pull request
// is.
//
// The faults: the pull request is recorded as the issue's, so a fact of it moves the issue, and the
// merge closes it as the implement App with `Issue LEGION-208 is done; its last merge is #2.`. The
// controls: the recorder without the rule (each pull request is recorded); a task stopped before
// its first step read as holding the issue's branch (b's pull request); the merge's close without
// the rule (#9 is closed).
func TestAPullRequestOnABranchNoTaskWasGivenIsNotTheIssues(t *testing.T) {
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

	t.Run("a pull request on a branch of a task's shape that no task holds", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := tasksRoot(t, pool)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "design", steps: []string{"tester:design"}, agents: []claim.Agent{"test-design"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "impl", steps: []string{"implementer:impl"}, agents: []claim.Agent{"impl-impl"}, needs: []string{"design"},
				state: record.TaskRunning, place: atStep(1)})
		seedTaskBranch(t, pool, root.Key, "design", "legion/LEGION-208")
		seedTaskBranch(t, pool, root.Key, "impl", "legion/LEGION-208.impl")

		mustSpend(t, "the open of #9", applyFacts(t, pool, engine)("opened-9", openedOn(9, "legion/LEGION-208.foo", at, false)))
		if rows := pullRequestRows(t, pool, 9); rows != 0 {
			t.Fatalf("the record holds %d rows of #9 on legion/LEGION-208.foo, want none: no task of %s holds that branch", rows, root.Key)
		}
	})

	t.Run("a pull request on the issue's branch while no task was given it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := tasksRoot(t, pool)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"tester:a"}, agents: []claim.Agent{"test-a"}, state: record.TaskRunning, place: atStep(1)},
			planTask{id: "b", steps: []string{"implementer:b"}, agents: []claim.Agent{"impl-b"}, needs: []string{"a"}, state: record.TaskStopped})
		gaveBranches(t, pool, root.Key, root.Generation, []string{"a"}, map[string]string{"a": "legion/LEGION-208.a"})

		mustSpend(t, "the open of #9", applyFacts(t, pool, engine)("opened-9", openedOn(9, "legion/LEGION-208", at, false)))
		if rows := pullRequestRows(t, pool, 9); rows != 0 {
			t.Fatalf("the record holds %d rows of #9 on legion/LEGION-208, want none: b, stopped before its first step, was given no branch", rows)
		}
	})

	t.Run("the merge closes no pull request on a branch no task holds", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := tasksRoot(t, pool)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "design", steps: []string{"tester:design"}, agents: []claim.Agent{"test-design"}, state: record.TaskDone, place: atStep(1)},
			planTask{id: "impl", steps: []string{"implementer:impl"}, agents: []claim.Agent{"impl-impl"}, needs: []string{"design"},
				state: record.TaskRunning, place: atMerge})
		seedTaskBranch(t, pool, root.Key, "design", "legion/LEGION-208")
		seedTaskBranch(t, pool, root.Key, "impl", "legion/LEGION-208.impl")
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")
		openPR(t, pool, root.Key, 2, "legion/LEGION-208.impl")
		// The recorder writes no row on legion/LEGION-208.foo; this one is written to the record
		// directly, so the merge's own rule is what this case reads.
		openPR(t, pool, root.Key, 9, "legion/LEGION-208.foo")

		mustSpend(t, "the merge of #2", applyFacts(t, pool, engine)("merged-2",
			intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 2, MergeSHA: "merge-2", HeadSHA: "head-legion/LEGION-208.impl"}))
		want := []record.TaskPullRequestsClose{{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: "Issue LEGION-208 is done; its last merge is #2."}}
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, want) {
			t.Fatalf("the closes of pull requests after the merge of #2 = %+v, want %+v: #9 is on a branch no task holds", got, want)
		}
	})
}

// A pull request whose row the record deleted when it let the pull request go is not recorded
// again by its open delivered again or by its reopen, since no task of the issue holds its branch,
// legion/LEGION-208. A run started again closed #1, the earlier run's pull request there: design
// took legion/LEGION-208.design, and impl, which waits, was given no branch. A version removed a,
// which held legion/LEGION-208 with #42, and left z alone, whose needs are then done: it starts in
// the version's transaction (startReady), and takes legion/LEGION-208.z, since its generation gave
// the issue's branch to a.
//
// The faults: #1 is recorded as the pull request of impl, the one task with no branch, and #42 as
// z's and as the issue's merge, so a fact of either moves a task of the run. The controls: the
// recorder without the rule (both are recorded); a waiting task of a plan of several read as
// holding the issue's branch (#1 is recorded); the recorder without its read of the names the
// generation gave (#42 is recorded).
func TestAPullRequestTheRecordLetGoIsNotRecordedAgain(t *testing.T) {
	t.Run("an earlier run's pull request that a run started again closed", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "design", steps: []string{"planner"}, agents: []claim.Agent{"planner"}, state: record.TaskWaiting},
			planTask{id: "impl", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"design"}, state: record.TaskWaiting})
		firstStepOf(t, pool, engine, root.Key)
		if design := tasksOf(t, pool, engine, root.Key)["design"]; design.Branch != "legion/LEGION-208.design" {
			t.Fatalf("design took %q, want legion/LEGION-208.design", design.Branch)
		}
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s after the first step, want none", got, root.Key)
		}

		apply := applyFacts(t, pool, engine)
		at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
		mustSpend(t, "the open of #1 delivered again", apply("opened-1-again", openedOn(1, "legion/LEGION-208", at, false)))
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s after the open of #1 delivered again, want none", got, root.Key)
		}
		mustSpend(t, "the reopen of #1", apply("reopened-1", openedOn(1, "legion/LEGION-208", at.Add(time.Hour), true)))
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s after the reopen of #1, want none", got, root.Key)
		}
	})

	t.Run("a removed task's pull request on the issue's branch, the plan left with one task", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := tasksRoot(t, pool)
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "a", steps: []string{"implementer:a"}, agents: []claim.Agent{"impl-a"}, state: record.TaskStopped, place: atStep(1)},
			planTask{id: "z", steps: []string{"reviewer:z"}, agents: []claim.Agent{"rev-z"}, needs: []string{"a"}, state: record.TaskWaiting})
		gaveBranches(t, pool, root.Key, root.Generation, []string{"a"}, map[string]string{"a": "legion/LEGION-208"})
		openPR(t, pool, root.Key, 42, "legion/LEGION-208")
		apply := applyFacts(t, pool, engine)

		mustTake(t, "version 2", apply("version-2", laterVersion(t, root.Key, root.Generation, 2, keyed(t, "z", "", []string{"reviewer:z"}))))
		if rows := pullRequestRows(t, pool, 42); rows != 0 {
			t.Fatalf("the record holds %d rows of a's pull request #42 after version 2 removed a, want none", rows)
		}
		if z := tasksOf(t, pool, engine, root.Key)["z"]; z.State != record.TaskRunning || z.Branch != "legion/LEGION-208.z" {
			t.Fatalf("z after version 2 = %s on %q, want running on legion/LEGION-208.z: its needs are done, and the issue's branch was given to a", z.State, z.Branch)
		}

		mustSpend(t, "the reopen of #42", apply("reopened-42", openedOn(42, "legion/LEGION-208", time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC), true)))
		if rows := pullRequestRows(t, pool, 42); rows != 0 {
			t.Fatalf("the record holds %d rows of #42 after its reopen, want none: z holds legion/LEGION-208.z", rows)
		}
		gaveBranches(t, pool, root.Key, root.Generation, []string{"z"}, map[string]string{"z": "legion/LEGION-208.z"})
	})
}

// A task that started before tasks had branches of their own holds the issue's branch whatever it
// does since, a stop among them, beside tasks a later version added: x, stopped at its
// implementer's step with no branch on its row, keeps #42 on legion/LEGION-208, as the record
// reads a pull request's task (record.PullRequest.Task) and as the workflow records a reopen of it.
// It holds behaviour the rule keeps: a stop of a task that started is told from a stop before its
// first step by the place it stands at, which each read of a pull request's task carries.
//
// The fault: x, stopped, read as a task that has not started, so its pull request is no task's.
// The control: the read of a pull request's task without the task's place (#42 is no task's).
func TestATaskThatStartedOnTheIssuesBranchKeepsItsPullRequestThroughAStop(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := tasksRoot(t, pool)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "x", steps: []string{"implementer:x"}, agents: []claim.Agent{"impl-x"}, state: record.TaskStopped, place: atStep(1)},
		planTask{id: "y", steps: []string{"tester:y"}, agents: []claim.Agent{"test-y"}, needs: []string{"x"}, state: record.TaskWaiting})
	openPR(t, pool, root.Key, 42, "legion/LEGION-208")
	if pr := pullRequestOfTask(t, pool, root.Key, "x"); pr.Number != 42 {
		t.Fatalf("the pull request of x = #%d, want #42 on the issue's branch", pr.Number)
	}

	mustSpend(t, "the reopen of #42", applyFacts(t, pool, engine)("reopened-42",
		openedOn(42, "legion/LEGION-208", time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC), true)))
	if pr := pullRequestOfTask(t, pool, root.Key, "x"); pr.Number != 42 || pr.State != record.PullRequestOpen {
		t.Fatalf("the pull request of x after the reopen of #42 = #%d %s, want #42 open", pr.Number, pr.State)
	}
}
