package workflow

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// runAgainComment is what the first step of a run that is not carried writes on each pull request
// of an earlier run it closes.
const runAgainComment = "The issue's run started its plan again; the work of this pull request is not carried."

// rootRunAgain records LEGION-208 as a re-admission that carries no run leaves it: at admitted, at
// generation 2, its tree open and its slot held, so its first step starts at the waiting line's
// offer (firstStepOf), and with the phases row of the tester, a step its earlier run ran, which
// the re-admission kept (record.Store.ClearGeneration keeps each row and its entry).
func rootRunAgain(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedSlot(t, pool, record.Slot{Issue: root.Key, Index: 0})
	seedPhase(t, pool, record.PhaseRow{Issue: root.Key, Role: claim.RoleTester, Claim: agentClaim(t, root.Key, "tester"), Entry: 2})
	return root
}

// gaveBranches gives each of tasks of key's plan at generation its branch, in order, as each
// task's first start does (record.Store.AssignTaskBranch), and fails the test unless each takes the
// branch want names for it.
func gaveBranches(t *testing.T, pool *pgxpool.Pool, key string, generation uint64, tasks []string, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		for _, task := range tasks {
			branch, err := record.NewStore().AssignTaskBranch(t.Context(), tx, key, generation, task)
			if err != nil {
				return err
			}
			got[task] = branch
		}
		return nil
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the branches of %v at generation %d = %v, want %v", tasks, generation, got, want)
	}
}

// openPR records number open on branch of key, the pull request a task of an earlier run opened.
func openPR(t *testing.T, pool *pgxpool.Pool, key string, number int, branch string) {
	t.Helper()
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: number,
		Branch: branch, HeadSHA: "head-" + branch, Failing: []string{}})
}

// closesOf is each close of pull requests key's outbox holds, oldest first.
func closesOf(t *testing.T, pool *pgxpool.Pool, key string) []record.TaskPullRequestsClose {
	t.Helper()
	var closes []record.TaskPullRequestsClose
	for _, row := range issueOutboxOf(t, pool, key, string(record.OutboxKindTaskPullRequestsClose)) {
		var close record.TaskPullRequestsClose
		if err := json.Unmarshal(row.payload, &close); err != nil {
			t.Fatalf("decode a close of pull requests of %s: %v", key, err)
		}
		closes = append(closes, close)
	}
	return closes
}

// recordedNumbers is the number of each pull request the record holds of key, in task order.
func recordedNumbers(t *testing.T, pool *pgxpool.Pool, key string) []int {
	t.Helper()
	var numbers []int
	seedRecord(t, pool, func(tx pgx.Tx) error {
		prs, err := record.NewStore().TaskPullRequests(t.Context(), tx, key)
		for _, pr := range prs {
			numbers = append(numbers, pr.Number)
		}
		return err
	})
	return numbers
}

// A run that is not carried starts each task of a plan of several from its start, so it gives no
// task a name an earlier generation gave, the issue's own branch among them, and it closes each
// open pull request of the earlier run, whose work it does not go on with, deleting its row in the
// same transaction: no task of the new run is given another task's old commits or pull request.
// A plan of one task goes on as an issue always did, on the issue's branch and the pull request
// open there, and only the pull requests of an earlier run's other branches close.
//
// The faults: the first task of the new run takes legion/LEGION-208, with the commits of the task
// that held it, a task whose id an earlier generation gave its slug's branch takes it again, and
// #1, the open pull request on legion/LEGION-208, is read as a task's of the new run, so a later
// fact of it moves that task. The controls: AssignTaskBranch reading the names of its own
// generation alone (check takes legion/LEGION-208.check), the first step without the close (#1
// stays recorded and nothing closes it), the close without the record of the branch it closes
// (impl, after the run before plans, takes legion/LEGION-208), and AssignTaskBranch without the
// exception of a plan of one task (its task leaves the issue's branch and its pull request).
func TestARunNotCarriedTakesNoEarlierNameAndClosesTheEarlierPullRequests(t *testing.T) {
	t.Run("a plan of several taken from an earlier generation from its start", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		// Generation 1 ran impl on the issue's branch, with its open pull request #1, and check on
		// its own, whose pull request is closed and gone with the generation; the owner's version
		// that added design, which both need, came after; the issue was set back with no merge.
		seedTasks(t, pool, root.Key, 1,
			planTask{id: "design", steps: []string{"planner"}, agents: []claim.Agent{"planner"}, state: record.TaskWaiting},
			planTask{id: "check", steps: []string{"tester"}, agents: []claim.Agent{"tester"}, needs: []string{"design"}, state: record.TaskWaiting},
			planTask{id: "impl", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"design", "check"}, state: record.TaskWaiting})
		gaveBranches(t, pool, root.Key, 1, []string{"impl", "check"}, map[string]string{"impl": "legion/LEGION-208", "check": "legion/LEGION-208.check"})
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")

		firstStepOf(t, pool, engine, root.Key)

		before := tasksOf(t, pool, engine, root.Key)
		if design := before["design"]; design.State != record.TaskRunning || design.Branch != "legion/LEGION-208.design" {
			t.Fatalf("design at generation 2 is %s on %q, want running on legion/LEGION-208.design: generation 1 gave legion/LEGION-208 to impl",
				design.State, design.Branch)
		}
		want := []record.TaskPullRequestsClose{{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: runAgainComment}}
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, want) {
			t.Fatalf("the closes of pull requests of %s = %+v, want %+v", root.Key, got, want)
		}
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s, want none: #1 is of the run before", got, root.Key)
		}

		// A later fact of #1, its merge before the close reached GitHub, moves no task.
		standing := issueOf(t, pool, engine, root.Key).Phase
		mustSpend(t, "the merge of #1", applyFacts(t, pool, engine)("merged-1",
			intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 1, MergeSHA: "merge-1", HeadSHA: "head-legion/LEGION-208"}))
		if after := tasksOf(t, pool, engine, root.Key); !reflect.DeepEqual(after, before) {
			t.Fatalf("the tasks after the merge of #1 = %+v, want them as before it, %+v", after, before)
		}
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s after the merge of #1, want none", got, root.Key)
		}
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, want) {
			t.Fatalf("the closes of pull requests of %s after the merge of #1 = %+v, want %+v", root.Key, got, want)
		}
		if issue := issueOf(t, pool, engine, root.Key); issue.Phase != standing {
			t.Fatalf("%s after the merge of #1 stands in %s, want %s, where it stood", root.Key, issue.Phase, standing)
		}
		// check, whose id generation 1 gave legion/LEGION-208.check, takes the next give of its slug.
		gaveBranches(t, pool, root.Key, root.Generation, []string{"check"}, map[string]string{"check": "legion/LEGION-208.check.2"})
	})

	t.Run("a plan of several after a run before plans", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		// Generation 1 ran before plans, on the issue's branch, which no plan_branches row names,
		// with its open pull request #1. The owner gave the plan of generation 2 before its first step.
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")
		seedTasks(t, pool, root.Key, root.Generation,
			planTask{id: "design", steps: []string{"planner"}, agents: []claim.Agent{"planner"}, state: record.TaskWaiting},
			planTask{id: "impl", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"design"}, state: record.TaskWaiting})

		firstStepOf(t, pool, engine, root.Key)

		if design := tasksOf(t, pool, engine, root.Key)["design"]; design.Branch != "legion/LEGION-208.design" {
			t.Fatalf("design took %q, want legion/LEGION-208.design: the run before plans holds #1 on legion/LEGION-208", design.Branch)
		}
		want := []record.TaskPullRequestsClose{{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{1}, Comment: runAgainComment}}
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, want) {
			t.Fatalf("the closes of pull requests of %s = %+v, want %+v", root.Key, got, want)
		}
		if got := recordedNumbers(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the record holds the pull requests %v of %s, want none: #1 is of the run before", got, root.Key)
		}
		// The row of #1 is gone, and its branch stays given: impl, which starts once design is
		// done, does not take it either.
		gaveBranches(t, pool, root.Key, root.Generation, []string{"impl"}, map[string]string{"impl": "legion/LEGION-208.impl"})
	})

	t.Run("a plan of one task keeps the issue's branch and its pull request", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		// Generation 1 ran a plan of several: design on the issue's branch with #1, impl on its own
		// with #2. The owner gave a plan of one task for generation 2 before its first step. The
		// tasks of generation 1 are left waiting, each with the branch its start gave it: nothing
		// of generation 2 reads their states.
		seedTasks(t, pool, root.Key, 1,
			planTask{id: "design", steps: []string{"planner"}, agents: []claim.Agent{"planner"}, state: record.TaskWaiting},
			planTask{id: "impl", steps: []string{"implementer"}, agents: []claim.Agent{"implementer"}, needs: []string{"design"}, state: record.TaskWaiting})
		gaveBranches(t, pool, root.Key, 1, []string{"design", "impl"},
			map[string]string{"design": "legion/LEGION-208", "impl": "legion/LEGION-208.impl"})
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")
		openPR(t, pool, root.Key, 2, "legion/LEGION-208.impl")
		seedStepsPlan(t, pool, root.Key, root.Generation, "implementer", "reviewer")

		firstStep(t, pool, engine, root.Key)

		if a := tasksOf(t, pool, engine, root.Key)["a"]; a.Branch != "legion/LEGION-208" {
			t.Fatalf("the one task a took %q, want the issue's branch legion/LEGION-208", a.Branch)
		}
		if pr := pullRequestOfTask(t, pool, root.Key, "a"); pr.Number != 1 || pr.State != record.PullRequestOpen {
			t.Fatalf("the pull request of a = #%d %s, want #1 open: the issue goes on on its branch", pr.Number, pr.State)
		}
		want := []record.TaskPullRequestsClose{{Generation: root.Generation, Repo: "sjawhar/legion", Numbers: []int{2}, Comment: runAgainComment}}
		if got := closesOf(t, pool, root.Key); !reflect.DeepEqual(got, want) {
			t.Fatalf("the closes of pull requests of %s = %+v, want %+v", root.Key, got, want)
		}
		if got := recordedNumbers(t, pool, root.Key); !reflect.DeepEqual(got, []int{1}) {
			t.Fatalf("the record holds the pull requests %v of %s, want #1 alone", got, root.Key)
		}
	})

	t.Run("the default steps keep the issue's branch and its pull request", func(t *testing.T) {
		pool := migratedPool(t)
		engine := returnEngine()
		root := rootRunAgain(t, pool)
		// Generation 1 ran the default steps on the issue's branch, with #1 open; the owner gave no plan.
		seedDefaultPlan(t, pool, root.Key, 1)
		gaveBranches(t, pool, root.Key, 1, []string{plan.DefaultTask}, map[string]string{plan.DefaultTask: "legion/LEGION-208"})
		openPR(t, pool, root.Key, 1, "legion/LEGION-208")

		firstStep(t, pool, engine, root.Key)

		if task := tasksOf(t, pool, engine, root.Key)[plan.DefaultTask]; task.Branch != "legion/LEGION-208" {
			t.Fatalf("the task default took %q, want the issue's branch legion/LEGION-208", task.Branch)
		}
		if pr := pullRequestOfTask(t, pool, root.Key, plan.DefaultTask); pr.Number != 1 || pr.State != record.PullRequestOpen {
			t.Fatalf("the pull request of default = #%d %s, want #1 open", pr.Number, pr.State)
		}
		if got := closesOf(t, pool, root.Key); len(got) != 0 {
			t.Fatalf("the closes of pull requests of %s = %+v, want none", root.Key, got)
		}
	})
}
