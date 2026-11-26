package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullRequestOfTask is the pull request of task of key as the store reads it back, which fails
// the test where it holds none.
func pullRequestOfTask(t *testing.T, pool *pgxpool.Pool, key, task string) record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequestOfTask(t.Context(), tx, key, task)
		return err
	})
	if pr == nil {
		t.Fatalf("the record holds no pull request of task %s of %s", task, key)
	}
	return *pr
}

// seedTaskBranch writes branch on the row of task of key's plan at the issue's generation, as the
// task's first start gives it one (record.Store.AssignTaskBranch), and fails the test where the
// plan holds no such task.
func seedTaskBranch(t *testing.T, pool *pgxpool.Pool, key, task, branch string) {
	t.Helper()
	tag, err := pool.Exec(t.Context(), `update plan_tasks t set branch = $3 from issues i
		where i.key = t.issue and t.generation = i.generation and t.issue = $1 and t.task = $2`, key, task, branch)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("give task %s of %s the branch %s: %d rows, %v", task, key, branch, tag.RowsAffected(), err)
	}
}

// The facts of the pull request on the issue's branch reach the task that holds that branch,
// whatever task its row was recorded under: #42 is recorded on legion/LEGION-208 with no task, as
// the release before recorded every row, and task a holds legion/LEGION-208 (its row names it, as
// the first task to start takes it; or its row names none, as a task started at the release, while
// z, which waits, names none either). a stands at its reviewer's step, b at its tester's on
// legion/LEGION-208.b. A CI settlement of #42 is told to a's reviewer, the implementer and the
// architect, and never to b's tester; a person's request for changes on #42 returns a to its
// implementer's step and leaves b testing.
//
// The fault it catches is a pull request's task read from what its row recorded rather than from
// the branch it is on (its control: scopeOfPullRequest on the row's own task, which names none, so
// nobody is told and nothing moves).
func TestAFactOfThePullRequestOnTheIssuesBranchReachesTheTaskThatHoldsIt(t *testing.T) {
	for _, tc := range []struct {
		name, branch string
	}{
		{"a's row names the issue's branch", "legion/LEGION-208"},
		{"a's row names no branch, as a task started at the release", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := returnEngine()
			root := stepRoot(t, pool, phase.Testing)
			seedTasks(t, pool, root.Key, root.Generation,
				planTask{id: "a", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
				planTask{id: "b", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, state: record.TaskRunning, place: atStep(1)},
				planTask{id: "z", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-2"}, needs: []string{"a", "b"}, state: record.TaskWaiting})
			if tc.branch != "" {
				seedTaskBranch(t, pool, root.Key, "a", tc.branch)
			}
			seedTaskBranch(t, pool, root.Key, "b", "legion/LEGION-208.b")
			apply := applyFacts(t, pool, engine)

			mustSpend(t, "the settlement of #42", apply("settled", settledAt("head", 1, "unit")))
			body := toldBody("CI settled at head: \"build\" succeeded, \"unit\" failed")
			assertRoleMessages(t, pool, root.Key, []record.RoleMessage{
				{Role: roleToken(t, claim.RoleArchitect), Body: body},
				{Role: roleToken(t, claim.RoleReviewer), Body: body},
				{Role: roleToken(t, claim.RoleImplementer), Body: body},
			})

			mustSpend(t, "the request for changes on #42", apply("changes", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 1,
				State: "changes_requested", CommitID: "head", Author: "a-writer", AuthorCanWrite: true, Body: "rename it",
				SubmittedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}))
			tasks := tasksOf(t, pool, engine, root.Key)
			assertTask(t, tasks, "a", record.TaskRunning, atStep(1))
			assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
			assertTask(t, tasks, "z", record.TaskWaiting, record.TaskPlace{})
		})
	}
}

// Each fact of a task's pull request reaches that task, and a fact of a done task's pull request
// moves no task. Task a stands at its tester's step and b at its reviewer's, each with its own
// pull request (a's #44, b's #45); c is done and keeps its pull request #46 open; z needs all three
// and waits. The issue stands in testing, the first of a's and b's phases. A CI settlement of a's
// head is told to a's tester, the implementer whose code it ran on and the architect, and never to
// b's reviewer. A person's request for changes on b's pull request returns b to its implementer's
// step and leaves a testing. A request for changes on c's pull request is recorded on c's row and
// moves no task.
//
// Each of a and b runs a step of its own role: two tasks whose steps share a role share that
// role's phases row, which is keyed by the issue and the role.
//
// The fault it catches is a fact of one task's pull request read as the issue's, or as another
// task's (its control: scopeOfPullRequest answering the issue's first running task, a, so the
// request for changes on b's pull request is read at a's testing and returns nothing).
func TestAReviewOfOneTasksPullRequestMovesThatTask(t *testing.T) {
	pool := migratedPool(t)
	engine := returnEngine()
	root := stepRoot(t, pool, phase.Testing)
	seedTasks(t, pool, root.Key, root.Generation,
		planTask{id: "a", steps: []string{"implementer", "tester"}, agents: []claim.Agent{"implementer", "tester"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "b", steps: []string{"implementer", "reviewer"}, agents: []claim.Agent{"implementer-2", "reviewer"}, state: record.TaskRunning, place: atStep(2)},
		planTask{id: "c", steps: []string{"implementer"}, agents: []claim.Agent{"implementer-3"}, state: record.TaskDone, place: atStep(1)},
		planTask{id: "z", steps: []string{"tester"}, agents: []claim.Agent{"tester-2"}, needs: []string{"a", "b", "c"}, state: record.TaskWaiting})
	seedTaskPRs(t, pool, root.Key, "a", "b", "c")
	apply := applyFacts(t, pool, engine)

	mustSpend(t, "the settlement of a's head", apply("a-settled", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 44, HeadSHA: "head-a",
		CheckRuns: []record.AttemptRun{{Name: "build", ID: 1}, {Name: "unit", ID: 1}}, Generation: 1, Snapshot: "head-a-1", Failing: []string{"unit"}}))
	told := "Pull request #44: CI settled at head-a: \"build\" succeeded, \"unit\" failed. Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code."
	var want []record.RoleMessage
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer} {
		want = append(want, record.RoleMessage{Role: roleToken(t, role), Body: told})
	}
	assertRoleMessages(t, pool, root.Key, want)

	changes := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 45, ID: 1, State: "changes_requested", CommitID: "head-b",
		Author: "a-writer", AuthorCanWrite: true, Body: "rename it", SubmittedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)}
	mustSpend(t, "the request for changes on b's pull request", apply("b-changes", changes))
	tasks := tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "a", record.TaskRunning, atStep(2))

	done := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 46, ID: 2, State: "changes_requested", CommitID: "head-c",
		Author: "a-writer", AuthorCanWrite: true, Body: "rename it too", SubmittedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	mustSpend(t, "the request for changes on c's pull request", apply("c-changes", done))
	if got := pullRequestOfTask(t, pool, root.Key, "c").ReviewSeen; got.ID != done.ID || !got.SubmittedAt.Equal(done.SubmittedAt) {
		t.Fatalf("c's pull request has seen the review %+v, want review %d of %s: a fact of a done task's pull request is recorded", got, done.ID, done.SubmittedAt)
	}
	tasks = tasksOf(t, pool, engine, root.Key)
	assertTask(t, tasks, "c", record.TaskDone, atStep(1))
	assertTask(t, tasks, "b", record.TaskRunning, atStep(1))
	assertTask(t, tasks, "a", record.TaskRunning, atStep(2))
}
