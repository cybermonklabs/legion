package record

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// What holds each task held at its end is read from the store: for a fix hold, the check and the
// head of the fix of the plan's task that reads its job; for a CI hold, each check a CI job runs
// that has not passed at the head of the open pull request, in the set's order, so a check that
// passed and one no CI job runs are not named.
//
// The controls name nothing, each for one reason the read finds nothing: a fix that ended, a
// generation with no plan rows, a pull request that is closed, a head with no set of checks held,
// and an issue held for its fix where its step keeps its slot, which the held list never gives
// the word, so nothing of it is read. An issue held for no end reads nothing either, and a pass
// with no such issue gets an empty map, never nil.
func TestWhatHoldsATasksEndIsReadFromTheStore(t *testing.T) {
	st := migratedStore(t)
	ctx := context.Background()
	records := NewStore()
	const repo, head = "acme/widgets", fixHeadY
	held := func(key string, from phase.Phase, reason HoldReason) Issue {
		issue := issueFixture(key)
		issue.Phase, issue.Hold, issue.Status = phase.Held, &Hold{From: from, Reason: reason}, "needs_review"
		return issue
	}
	issues := []Issue{
		held("LEGION-80", phase.Retro, HoldForFix),
		held("LEGION-81", phase.Retro, HoldForCI),
		held("LEGION-82", phase.Retro, HoldForFix),
		held("LEGION-83", phase.Retro, HoldForFix),
		held("LEGION-84", phase.Retro, HoldForCI),
		held("LEGION-85", phase.Retro, HoldForCI),
		held("LEGION-86", phase.Reviewing, HoldForFix),
		held("LEGION-87", phase.Retro, HoldEscalated),
	}
	fixOf := func(key string) TaskFix {
		return TaskFix{Issue: key, Generation: 3, Task: "a", Fix: 1, State: FixReading, Check: fixCheck, Head: fixHeadX, Place: 1}
	}
	pullRequestOf := func(key string, number int, state PullRequestState) PullRequest {
		return PullRequest{Issue: key, Repo: repo, Number: number, Branch: "legion/" + key, HeadSHA: head, Generation: 3, State: state}
	}
	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range issues {
			must(t, records.PutIssue(ctx, tx, issue))
		}
		// LEGION-80 and LEGION-86 each run a fix of their plan's task that reads its job; LEGION-82's
		// fix ended; LEGION-83's generation has no plan rows, so no task names its fix.
		for _, key := range []string{"LEGION-80", "LEGION-82", "LEGION-86"} {
			v, tasks, steps := ownersPlan(key, 3, 1, planOwner, planGivenAt)
			putPlan(t, tx, v, tasks, steps)
			must(t, records.PutFix(ctx, tx, fixOf(key)))
		}
		ended := fixOf("LEGION-82")
		ended.State, ended.Entry, ended.Result, ended.Way = FixEnded, 7, "passed", 1
		must(t, records.UpdateFix(ctx, tx, ended))
		must(t, records.PutFix(ctx, tx, fixOf("LEGION-83")))
		// LEGION-81's head holds a check whose job failed, one whose job runs, one that passed and
		// one no CI job runs; LEGION-84's pull request is closed; LEGION-85's head holds no set.
		must(t, records.PutPullRequest(ctx, tx, pullRequestOf("LEGION-81", 81, PullRequestOpen)))
		must(t, records.PutPullRequest(ctx, tx, pullRequestOf("LEGION-84", 84, PullRequestClosed)))
		must(t, records.PutPullRequest(ctx, tx, pullRequestOf("LEGION-85", 85, PullRequestOpen)))
		for _, key := range []string{"LEGION-81", "LEGION-84"} {
			_, err := records.PutCheckSet(ctx, tx, key, repo, head, []string{"k-failed", "k-passed", "k-unit", "k-running"}, 1)
			must(t, err)
			for i, state := range []CheckState{
				{Check: "k-failed", State: "no", Reason: "fail", CICase: "failure"},
				{Check: "k-passed", State: "yes", CICase: "success"},
				{Check: "k-unit", State: "no", Reason: "fail"},
				{Check: "k-running", State: "no", Reason: "no_result", CICase: CINotCompleted},
			} {
				state.Issue, state.Repo, state.Head, state.Seq = key, repo, head, int64(i+2)
				_, err := records.PutCheckState(ctx, tx, state)
				must(t, err)
			}
		}
	})

	inTx(t, st, func(tx pgx.Tx) {
		got, err := EndHoldsOf(ctx, tx, records, issues)
		must(t, err)
		want := map[string][]string{
			"LEGION-80": {fixCheck, fixHeadX},
			"LEGION-81": {"k-failed", "k-running"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("EndHoldsOf = %v, want %v", got, want)
		}
		none, err := EndHoldsOf(ctx, tx, records, issues[len(issues)-1:])
		must(t, err)
		if none == nil || len(none) != 0 {
			t.Fatalf("EndHoldsOf of an issue held for no end = %#v, want an empty map", none)
		}
	})
}
