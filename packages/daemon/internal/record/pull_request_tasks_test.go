package record

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// taskPullRequest is an open pull request of task of issue on branch, numbered number.
func taskPullRequest(issue, task, branch string, number int) PullRequest {
	return PullRequest{Issue: issue, Task: task, Repo: "acme/widgets", Number: number, Branch: branch, HeadSHA: "head-" + branch,
		Failing: []string{}, State: PullRequestOpen}
}

// numbersOf is the task of each of prs and its branch without legion/, in order.
func numbersOf(prs []PullRequest) []string {
	var got []string
	for _, pr := range prs {
		got = append(got, pr.Task+"#"+strings.TrimPrefix(pr.Branch, "legion/"))
	}
	return got
}

// Each task of an issue keeps its own pull request, on its own branch: the pull request of task b
// opened after task a's stands beside it rather than taking its place, and a later write of a's
// row leaves b's as it was. Each task's branch is the one its start gave it (AssignTaskBranch).
// Each task's row is read by the issue and the task; the row of the issue's merge, b's, the one task
// that merges, is read by the issue alone, for one issue and for several at once; and every
// row of the issue is read in task order. An issue with no plan rows keeps its one row under the
// task "", which is the row of its merge.
func TestTwoTasksOfOneIssueKeepTwoPullRequests(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue, noPlan := issueFixture(planIssue), issueFixture("LEGION-209")
	a := taskPullRequest(planIssue, "a", "legion/LEGION-208", 41)
	b := taskPullRequest(planIssue, "b", "legion/LEGION-208.b", 42)
	alone := taskPullRequest(noPlan.Key, "", "legion/LEGION-209", 43)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutIssue(ctx, tx, noPlan))
		v := PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}
		putPlan(t, tx, v, []PlanTask{
			{Issue: planIssue, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: TaskRunning},
			{Issue: planIssue, Generation: issue.Generation, Task: "b", PlanVersion: 1, BlockText: "Test the widget.", Needs: []string{"a"},
				Merges: true, State: TaskWaiting},
		}, nil)
		for _, task := range []string{"a", "b"} {
			if _, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, task); err != nil {
				t.Fatalf("give task %s its branch: %v", task, err)
			}
		}
		must(t, records.PutPullRequest(ctx, tx, a))
		must(t, records.PutPullRequest(ctx, tx, b))
		must(t, records.PutPullRequest(ctx, tx, alone))
		a.HeadSHA = "head-a-2"
		must(t, records.PutPullRequest(ctx, tx, a))
	})
	inTx(t, st, func(tx pgx.Tx) {
		for _, want := range []PullRequest{a, b, alone} {
			got, err := records.PullRequestOfTask(ctx, tx, want.Issue, want.Task)
			must(t, err)
			if got == nil || got.Number != want.Number || got.Branch != want.Branch || got.HeadSHA != want.HeadSHA || got.Task != want.Task {
				t.Fatalf("the pull request of task %q of %s = %+v, want #%d on %s at %s: each task keeps its own row", want.Task, want.Issue, got, want.Number, want.Branch, want.HeadSHA)
			}
		}
		if got, err := records.PullRequestOfTask(ctx, tx, planIssue, "c"); err != nil || got != nil {
			t.Fatalf("the pull request of task c, which has none = %+v, %v, want none", got, err)
		}
		for _, want := range []PullRequest{b, alone} {
			got, err := records.MergePullRequest(ctx, tx, want.Issue)
			must(t, err)
			if got == nil || got.Number != want.Number {
				t.Fatalf("the merge pull request of %s = %+v, want #%d, the row of task %q", want.Issue, got, want.Number, want.Task)
			}
		}
		every, err := records.TaskPullRequests(ctx, tx, planIssue)
		must(t, err)
		if got, want := numbersOf(every), []string{"a#LEGION-208", "b#LEGION-208.b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the pull requests of %s = %v, want %v: each task's, in task order", planIssue, got, want)
		}
		merges, err := records.PullRequestsOf(ctx, tx, []string{planIssue, noPlan.Key})
		must(t, err)
		if len(merges) != 2 || merges[planIssue].Number != b.Number || merges[noPlan.Key].Number != alone.Number {
			t.Fatalf("the pull requests of %s and %s = %+v, want #%d and #%d, the row of each one's merge", planIssue, noPlan.Key, merges, b.Number, alone.Number)
		}
	})
}

// A pull request's task is the task that holds the branch it is on, read whenever the row is read,
// so the record holds no task a branch contradicts. #41 is recorded on legion/LEGION-208 while
// LEGION-208 has no plan rows, so it reads the task "", which is its merge's. Then a plan of two
// tasks is given, a and b, b needing a and merging, both waiting with no branch: a task that waits
// with none holds none, so #41 is no task's and b's merge has no pull request. Task a's first
// start gives it legion/LEGION-208 (AssignTaskBranch), and #41 is a's from then on, read by task,
// by number and among the issue's; b's merge still has none, since b holds no branch. #42,
// recorded on legion/LEGION-208.b while b waits with no branch, is no task's, and b's once b's
// start gives it that branch, and it is then the merge's. A second pull request opened on
// legion/LEGION-208, #44, keeps a row of its own beside #41 (the record is keyed by the repository
// and the number, migration 20261127031536), and #41 stays a's own: neither is on a's base, so the
// first opened is (dispatch://INFRA-5910 decision 5), and #44 is another pull request of a.
//
// The fault it catches is a task the row stored when it was written (its control: the reads on
// the task the write named, so #41 reads "" after a takes its branch).
func TestThePullRequestOfABranchIsTheTaskThatHoldsIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture(planIssue)
	first := taskPullRequest(planIssue, "", "legion/LEGION-208", 41)
	ofB := taskPullRequest(planIssue, "", "legion/LEGION-208.b", 42)
	again := taskPullRequest(planIssue, "", "legion/LEGION-208", 44)
	read := func(what string, task string, want *PullRequest) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			got, err := records.PullRequestOfTask(ctx, tx, planIssue, task)
			must(t, err)
			if (got == nil) != (want == nil) || got != nil && (got.Number != want.Number || got.Task != task) {
				t.Fatalf("%s: the pull request of task %q = %+v, want %+v under task %q", what, task, got, want, task)
			}
		})
	}
	merge := func(what string, want *PullRequest) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			got, err := records.MergePullRequest(ctx, tx, planIssue)
			must(t, err)
			if (got == nil) != (want == nil) || got != nil && got.Number != want.Number {
				t.Fatalf("%s: the merge pull request = %+v, want %+v", what, got, want)
			}
		})
	}
	every := func(what string, want []string) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			prs, err := records.TaskPullRequests(ctx, tx, planIssue)
			must(t, err)
			if got := numbersOf(prs); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: the pull requests of %s = %v, want %v", what, planIssue, got, want)
			}
		})
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutPullRequest(ctx, tx, first))
	})
	read("no plan rows", "", &first)
	merge("no plan rows", &first)

	inTx(t, st, func(tx pgx.Tx) {
		v := PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}
		putPlan(t, tx, v, []PlanTask{
			{Issue: planIssue, Generation: issue.Generation, Task: "a", PlanVersion: 1, BlockText: "Build the widget.", State: TaskWaiting},
			{Issue: planIssue, Generation: issue.Generation, Task: "b", PlanVersion: 1, BlockText: "Test the widget.", Needs: []string{"a"},
				Merges: true, State: TaskWaiting},
		}, nil)
	})
	read("both tasks wait with no branch", "a", nil)
	merge("both tasks wait with no branch", nil)
	every("both tasks wait with no branch", []string{"#LEGION-208"})

	inTx(t, st, func(tx pgx.Tx) {
		if branch, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, "a"); err != nil || branch != "legion/LEGION-208" {
			t.Fatalf("task a's start took %q, %v, want the issue's branch", branch, err)
		}
	})
	read("a holds legion/LEGION-208", "a", &first)
	merge("a holds legion/LEGION-208 and b none", nil)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutPullRequest(ctx, tx, ofB))
	})
	every("b waits with no branch", []string{"#LEGION-208.b", "a#LEGION-208"})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.PullRequestByNumber(ctx, tx, first.Repo, first.Number)
		must(t, err)
		if got == nil || got.Task != "a" {
			t.Fatalf("#%d read by number = %+v, want it a's", first.Number, got)
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		if branch, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, "b"); err != nil || branch != "legion/LEGION-208.b" {
			t.Fatalf("task b's start took %q, %v, want legion/LEGION-208.b", branch, err)
		}
	})
	read("b holds legion/LEGION-208.b", "b", &ofB)
	merge("b holds legion/LEGION-208.b", &ofB)

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutPullRequest(ctx, tx, again))
	})
	read("a second pull request opened on a's branch", "a", &first)
	every("a second pull request opened on a's branch", []string{"a#LEGION-208", "a#LEGION-208", "b#LEGION-208.b"})
}
