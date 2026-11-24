package record

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A name any generation of an issue gave a task is never given to a task of a later generation whose
// plan holds several tasks, the issue's own branch among them: GitHub keeps each branch and its
// commits across generations, and a run that is not carried starts each task from its start. So the
// first task of the new generation to start takes its own slug's branch, not the issue's branch and
// an earlier task's commits, and an id given again takes the next give of its slug.
//
// The fault: a new generation that takes the plan of an earlier one from its start gives the issue's
// branch to whichever task starts first. The control: AssignTaskBranch reading the names of its own
// generation alone, where design takes legion/LEGION-208.
func TestANameAnEarlierGenerationGaveIsNotGivenAgain(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "impl", "b")
		if impl, b := assign(t, tx, planIssue, 1, "impl"), assign(t, tx, planIssue, 1, "b"); impl != "legion/LEGION-208" || b != "legion/LEGION-208.b" {
			t.Fatalf("at generation 1 impl took %q and b %q, want legion/LEGION-208 and legion/LEGION-208.b", impl, b)
		}
		putTasks(t, tx, planIssue, 2, 1, "design", "b")
		if design := assign(t, tx, planIssue, 2, "design"); design != "legion/LEGION-208.design" {
			t.Fatalf("the first task to start at generation 2 took %q, want legion/LEGION-208.design: generation 1 gave the issue's branch to impl", design)
		}
		if b := assign(t, tx, planIssue, 2, "b"); b != "legion/LEGION-208.b.2" {
			t.Fatalf("task b given again at generation 2 took %q, want legion/LEGION-208.b.2: generation 1 gave legion/LEGION-208.b", b)
		}
	})
}

// The branch of a pull request of an earlier run, whose row the first step of a run started again
// deletes, stays given to the issue (RecordBranchGiven): an issue that ran before plans gave its
// own branch in no plan_branches row, and GitHub keeps that branch and its commits. So a task of a
// plan of several at that generation takes its own slug's branch; the one task of a plan of one at
// a later generation still takes the issue's branch. A name recorded twice is recorded once.
//
// The fault: the first task of a plan of several takes the branch of the run before plans, with its
// commits. The control: RecordBranchGiven writing nothing, where design takes legion/LEGION-208.
func TestTheBranchOfAPullRequestOfAnEarlierRunStaysGiven(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RecordBranchGiven(ctx, tx, planIssue, 3, "legion/LEGION-208"))
		must(t, records.RecordBranchGiven(ctx, tx, planIssue, 3, "legion/LEGION-208"))
		putTasks(t, tx, planIssue, 3, 1, "design", "impl")
		if design := assign(t, tx, planIssue, 3, "design"); design != "legion/LEGION-208.design" {
			t.Fatalf("the first task to start at generation 3 took %q, want legion/LEGION-208.design: the earlier run's pull request was on legion/LEGION-208", design)
		}
		putTasks(t, tx, planIssue, 4, 1, "default")
		if task := assign(t, tx, planIssue, 4, "default"); task != "legion/LEGION-208" {
			t.Fatalf("the one task of a plan of one at generation 4 took %q, want the issue's branch legion/LEGION-208", task)
		}
	})
}

// A plan of one task goes on as an issue always did: its one task takes the issue's own branch
// whatever an earlier generation gave, so an issue run again works on its branch and its open pull
// request, which a new generation keeps (ClearGeneration). A later version of that generation that
// adds a task gives it a name no generation gave.
//
// The fault: a plain issue run again leaves its branch and its pull request for a branch of its one
// task's slug. The control: AssignTaskBranch without the exception of a plan of one task, where
// default takes legion/LEGION-208.default.
func TestTheOneTaskOfAPlanTakesTheIssuesBranchAtEveryGeneration(t *testing.T) {
	st := migratedStore(t)
	inTx(t, st, func(tx pgx.Tx) {
		putTasks(t, tx, planIssue, 1, 1, "a", "b")
		assign(t, tx, planIssue, 1, "a")
		assign(t, tx, planIssue, 1, "b")
		putTasks(t, tx, planIssue, 3, 1, "default")
		if task := assign(t, tx, planIssue, 3, "default"); task != "legion/LEGION-208" {
			t.Fatalf("the one task of the plan at generation 3 took %q, want the issue's branch legion/LEGION-208: a plan of one goes on on it", task)
		}
		putTasks(t, tx, planIssue, 3, 2, "b")
		if b := assign(t, tx, planIssue, 3, "b"); b != "legion/LEGION-208.b.2" {
			t.Fatalf("task b, which a later version added at generation 3, took %q, want legion/LEGION-208.b.2: generation 1 gave legion/LEGION-208.b", b)
		}
	})
}
