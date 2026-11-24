package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Two checks opened in one transaction at one reading of the clock both stand: a leave at done
// opens the check of each merged pull request of the run that has none, and two such merges get two
// checks. A check's OpenedAt is its identity (record.ProductionCheck.ID), so each new check takes
// one later than every check of the issue: LEGION-208 holds the check of #1, opened at the rig's
// clock, and one transaction at that same clock opens the checks of #2 and #3. The issue holds
// three checks, of #1, #2 and #3, each opened at its own moment, the two new ones after #1's.
//
// The fault it catches is a second check written over the first, which the store's write of a
// check keys by the issue and that moment (its control: OpenedAt read from the clock alone, under
// which the issue holds #1's check and one of the two new ones).
func TestTwoChecksOpenedInOneTransactionBothStand(t *testing.T) {
	rig := newCheckRig(t)
	issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 7, Status: "done", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, rig.pool, issue)
	openMerge(t, rig, issue, 1, "merge-1")
	first := checksOfIssue(t, rig, issue.Key)
	if len(first) != 1 || !first[0].OpenedAt.Equal(checkOpened) {
		t.Fatalf("premise: the checks of %s after #1's merge = %+v, want #1's alone, opened at the rig's clock %s", issue.Key, first, checkOpened)
	}
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(context.Background())
		for _, number := range []int{2, 3} {
			pr := record.PullRequest{Issue: issue.Key, Repo: "sjawhar/legion", Number: number, MergeSHA: fmt.Sprintf("merge-%d", number)}
			if err := rig.engine.openCheck(ctx, tx, issue, pr, false); err != nil {
				return err
			}
		}
		return nil
	})
	checks := checksOfIssue(t, rig, issue.Key)
	numbers := map[int]time.Time{}
	for _, check := range checks {
		numbers[check.Number] = check.OpenedAt
	}
	if len(checks) != 3 || len(numbers) != 3 {
		t.Fatalf("the checks of %s after #2 and #3 opened in one transaction = %+v, want three, of #1, #2 and #3", issue.Key, checks)
	}
	if !numbers[2].After(numbers[1]) || !numbers[3].After(numbers[1]) || numbers[2].Equal(numbers[3]) {
		t.Fatalf("the checks of %s were opened at #1 %s, #2 %s, #3 %s, want #2's and #3's each its own moment after #1's",
			issue.Key, numbers[1], numbers[2], numbers[3])
	}
}
