package record

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// numbersOfRows is the number of each of prs, in order.
func numbersOfRows(prs []PullRequest) []int {
	numbers := []int{}
	for _, pr := range prs {
		numbers = append(numbers, pr.Number)
	}
	return numbers
}

// The merge of an issue reads the one rule of which tasks merge (StoredPlan.TasksThatMerge): the
// pull requests of an issue's merges are the rows of each task that has a merge, done or to come,
// in task id order (MergePullRequests), and the issue's merge pull request is the row of the first
// of them that is not done, else of the last (MergePullRequest, and PullRequestsOf for several
// issues). Of a (no task needs it, done at its merge, #1 merged), c (b needs it, done at its last
// step, #3 open) and b (needs c, its block says merges, at its merge, #2 open), the merges are #1
// and #2, and the issue's is #2, b's, the first that is not done; with b done at its merge it is
// #2 still, the last. A later version that adds d, needing a, takes a out of the tasks that will
// merge, and a's #1 is still among the merges: a's work merged (WorkMerged). The issue's merge is
// then d's, the first that is not done, which has no pull request yet. An issue with no plan rows
// has its one row, of the task "", for each read.
//
// The fault it catches is the merge of an issue read from the one task that no task needs, so a
// plan in which two tasks merge has no merge pull request, or the wrong one (its control: the reads
// on plan.Sinks's one task, under which the first read answers none).
func TestTheMergePullRequestsOfAnIssueAreThoseOfItsMergingTasks(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue, noPlan := issueFixture(planIssue), issueFixture("LEGION-209")
	merge := TaskPlace{Kind: PlaceMerge}
	row := func(version int, task string, needs []string, merges bool, state TaskState, place TaskPlace) PlanTask {
		return PlanTask{Issue: planIssue, Generation: issue.Generation, Task: task, PlanVersion: version, BlockText: "Task " + task + ".",
			Needs: needs, Merges: merges, State: state, Place: place}
	}
	version := func(n int) PlanVersion {
		return PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: n, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}
	}
	first := []PlanTask{
		row(1, "a", nil, false, TaskDone, merge),
		row(1, "b", []string{"c"}, true, TaskRunning, merge),
		row(1, "c", nil, false, TaskDone, TaskPlace{Kind: PlaceStep, Position: 1}),
	}
	numbers := map[string]int{"a": 1, "b": 2, "c": 3}
	alone := taskPullRequest(noPlan.Key, "", "legion/LEGION-209", 43)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutIssue(ctx, tx, noPlan))
		putPlan(t, tx, version(1), first, nil)
		for _, task := range []string{"a", "b", "c"} {
			branch, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, task)
			if err != nil {
				t.Fatalf("give task %s its branch: %v", task, err)
			}
			pr := taskPullRequest(planIssue, task, branch, numbers[task])
			if task == "a" {
				pr.State, pr.MergeSHA = PullRequestMerged, "merge-a"
			}
			must(t, records.PutPullRequest(ctx, tx, pr))
		}
		must(t, records.PutPullRequest(ctx, tx, alone))
	})
	reads := func(what string, merges []int, issueMerge int) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			every, err := records.MergePullRequests(ctx, tx, planIssue)
			must(t, err)
			if got := numbersOfRows(every); !slices.Equal(got, merges) {
				t.Fatalf("%s: the merge pull requests of %s = %v, want %v", what, planIssue, got, merges)
			}
			got, err := records.MergePullRequest(ctx, tx, planIssue)
			must(t, err)
			if issueMerge == 0 && got != nil || issueMerge != 0 && (got == nil || got.Number != issueMerge) {
				t.Fatalf("%s: the merge pull request of %s = %+v, want #%d (0 for none)", what, planIssue, got, issueMerge)
			}
			of, err := records.PullRequestsOf(ctx, tx, []string{planIssue, noPlan.Key})
			must(t, err)
			if pr, named := of[planIssue]; issueMerge == 0 && named || issueMerge != 0 && pr.Number != issueMerge || of[noPlan.Key].Number != alone.Number {
				t.Fatalf("%s: the merge pull requests of %s and %s = %+v, want #%d (0 for none) and #%d", what, planIssue, noPlan.Key, of, issueMerge, alone.Number)
			}
		})
	}
	reads("b at its merge", []int{1, 2}, 2)

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.MoveTask(ctx, tx, planIssue, issue.Generation, "b", merge, TaskDone))
	})
	reads("b done at its merge", []int{1, 2}, 2)

	inTx(t, st, func(tx pgx.Tx) {
		later := []PlanTask{
			row(2, "a", nil, false, TaskDone, merge),
			row(2, "b", []string{"c"}, true, TaskDone, merge),
			row(2, "c", nil, false, TaskDone, TaskPlace{Kind: PlaceStep, Position: 1}),
			row(2, "d", []string{"a"}, false, TaskWaiting, TaskPlace{}),
		}
		putPlan(t, tx, version(2), later, nil)
		stored, err := records.Plan(ctx, tx, planIssue, issue.Generation)
		must(t, err)
		if merging := stored.Merging(); !slices.Equal(merging, []string{"b", "d"}) {
			t.Fatalf("premise: the tasks that will merge after version 2 = %v, want [b d]: d needs a", merging)
		}
	})
	reads("version 2 adds d, needing a", []int{1, 2}, 0)

	inTx(t, st, func(tx pgx.Tx) {
		every, err := records.MergePullRequests(ctx, tx, noPlan.Key)
		must(t, err)
		if len(every) != 1 || every[0].Number != alone.Number || every[0].Task != "" {
			t.Fatalf("the merge pull requests of %s, which has no plan rows = %+v, want its one row #%d, of the task \"\"", noPlan.Key, every, alone.Number)
		}
		got, err := records.MergePullRequest(ctx, tx, noPlan.Key)
		must(t, err)
		if got == nil || got.Number != alone.Number {
			t.Fatalf("the merge pull request of %s, which has no plan rows = %+v, want #%d", noPlan.Key, got, alone.Number)
		}
	})
}

// A parked issue's merge is still its task's pull request: a person parked LEGION-208, a child of one
// task c, at c's review, so its leave wrote c left at its third step with its pull request #7 open.
// The merge of the issue (MergePullRequest), its merges (MergePullRequests) and the merge of each of
// several issues (PullRequestsOf) answer #7, as each did before tasks had merges of their own, and
// once a person merges #7 they answer it merged.
//
// The fault it catches is a parked issue that loses its pull request in the served state and in the
// reads of several issues, and whose merge a later leave at done never checks (its control: the
// tasks that have a merge as the tasks that will merge and those whose work merged, under which
// MergePullRequest answers none and PullRequestsOf holds no entry for the child).
func TestAParkedIssuesMergePullRequestIsItsLeftTasks(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	issue := issueFixture(planIssue)
	a := taskPullRequest(planIssue, "c", "legion/LEGION-208", 7)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		putPlan(t, tx, PlanVersion{Issue: planIssue, Generation: issue.Generation, Version: 1, ArtifactID: planAuthor, Source: PlanSourceOwner,
			GivenBy: planOwner, GivenAt: planGivenAt}, []PlanTask{{Issue: planIssue, Generation: issue.Generation, Task: "c", PlanVersion: 1,
			BlockText: "Build the widget.", State: TaskLeft, Place: TaskPlace{Kind: PlaceStep, Position: 3}}}, nil)
		if _, err := records.AssignTaskBranch(ctx, tx, planIssue, issue.Generation, "c"); err != nil {
			t.Fatalf("give task c its branch: %v", err)
		}
		must(t, records.PutPullRequest(ctx, tx, a))
	})
	reads := func(what string, state PullRequestState) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			got, err := records.MergePullRequest(ctx, tx, planIssue)
			must(t, err)
			if got == nil || got.Number != a.Number || got.State != state {
				t.Fatalf("%s: the merge pull request of %s = %+v, want #%d %s", what, planIssue, got, a.Number, state)
			}
			every, err := records.MergePullRequests(ctx, tx, planIssue)
			must(t, err)
			if got := numbersOfRows(every); !slices.Equal(got, []int{a.Number}) {
				t.Fatalf("%s: the merge pull requests of %s = %v, want [%d]", what, planIssue, got, a.Number)
			}
			of, err := records.PullRequestsOf(ctx, tx, []string{planIssue})
			must(t, err)
			if of[planIssue].Number != a.Number {
				t.Fatalf("%s: the merge pull requests of [%s] = %+v, want #%d", what, planIssue, of, a.Number)
			}
		})
	}
	reads("c left at its review", PullRequestOpen)
	inTx(t, st, func(tx pgx.Tx) {
		merged := a
		merged.State, merged.MergeSHA = PullRequestMerged, "merge-7"
		must(t, records.PutPullRequest(ctx, tx, merged))
	})
	reads("#7 merged by a person", PullRequestMerged)
}

// The queued closes of an issue's pull requests are the rows that close pull requests of that issue
// at that generation which the outbox has not finished, in any attempt. Of LEGION-208's rows: one of
// generation 7 naming #45, due; one of generation 7 naming #43, backing off after a failed attempt;
// one of generation 6 naming #47; and one of generation 7 naming #49, which ran and is gone. The
// queued closes at generation 7 are #43 and #45, in number order.
//
// The fault it catches is a notice that hides a pull request of an earlier run's close, which
// finishes without acting (its control: the read without its generation, which answers #47 too).
func TestQueuedPullRequestClosesReadsTheUnfinishedRowsOfTheGeneration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(planIssue)))
		for _, queued := range []struct {
			generation uint64
			number     int
			failed     bool
		}{{7, 45, false}, {7, 43, true}, {6, 47, false}, {7, 49, false}} {
			row, err := NewOutboxRow(planIssue, TaskPullRequestsClose{Generation: queued.generation, Numbers: []int{queued.number}, Comment: "Merged in #2."}, now)
			must(t, err)
			if queued.failed {
				row.Attempts, row.LastError, row.NextAt = 1, "GitHub answered 502", now.Add(time.Minute)
			}
			must(t, records.Enqueue(ctx, tx, row))
		}
		if _, err := tx.Exec(ctx, `delete from outbox where kind = $1 and payload->'numbers' @> '[49]'`, string(OutboxKindTaskPullRequestsClose)); err != nil {
			t.Fatalf("finish the close of #49: %v", err)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.QueuedPullRequestCloses(ctx, tx, planIssue, 7)
		must(t, err)
		if !slices.Equal(got, []int{43, 45}) {
			t.Fatalf("the queued closes of %s at generation 7 = %v, want [43 45]", planIssue, got)
		}
		if none, err := records.QueuedPullRequestCloses(ctx, tx, "LEGION-209", 7); err != nil || len(none) != 0 {
			t.Fatalf("the queued closes of LEGION-209, which has none = %v, %v, want none", none, err)
		}
	})
}
