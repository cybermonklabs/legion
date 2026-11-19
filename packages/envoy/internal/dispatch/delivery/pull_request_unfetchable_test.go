package delivery

// MarkPullRequestUnfetchable ends the read of a timeline row that GitHub proved absent. It has two
// callers: the facts line, for a timeline row with no results row (readFactsLine, settleFacts),
// and the completion of a partial row (completePartialPullRequest). Each test drives the callers
// against batchGitHub, whose repository holds #40 and no other pull request, so its absence query
// proves each other number absent.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// unfetchableRepo is the repository of the pull requests that GitHub proves absent.
const unfetchableRepo = "acme/widgets"

// dropTimelineAttributionColumns drops the columns of delivery_pull_requests that the release
// before this build reads and writes and that this build names nowhere. The contracting migration
// of the next release drops them, so a statement of this build that names one fails there.
func dropTimelineAttributionColumns(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `alter table delivery_pull_requests
		drop column attribution_title_keys, drop column attribution_cited_issues, drop column attribution_branch_keys,
		drop column attribution_commit_keys, drop column attribution_checked_at`); err != nil {
		t.Fatalf("drop the attribution columns of delivery_pull_requests: %v", err)
	}
}

// absentGitHub is GitHub with one pull request, #40 of unfetchableRepo. Its REST read of each
// other number answers 404, and its absence query proves that number absent.
func absentGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fake := newFakeGitHub(t)
	github := serveBatchGitHub(t, fake)
	github.set(unfetchableRepo, 40, mergedPull(40, time.Now().UTC().Add(-time.Hour).Truncate(time.Second)))
	return fake
}

// Both marks of a pull request that GitHub proved absent run on the schema that the next release
// leaves: delivery_pull_requests with none of its attribution_* columns. The facts line marks a
// timeline row with no results row, and the completion marks a partial row. A mark that names
// such a column fails there, and the row then stays in the facts line, or partial, for good.
func TestBothMarksOfAnAbsentPullRequestRunWithoutTheTimelinesAttributionColumns(t *testing.T) {
	t.Run("the_facts_line", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		dropTimelineAttributionColumns(t, ctx, pool)
		storeStatementRow(t, ctx, pool, statementRow{repo: unfetchableRepo, number: 43, title: changeTitle(43), noHead: true, timeline: &statementTimeline{}})

		readFactsLineOrFail(t, ctx, NewReconcile(pool, absentGitHub(t).newTestClient(), events.NewBroker()))

		if refused, unfetchable := timelineStampsOf(t, ctx, pool, unfetchableRepo, 43); refused || !unfetchable {
			t.Errorf("the timeline row %s#43 holds facts_refused_at %v and unfetchable_at %v, want unfetchable_at only: GitHub proved it absent",
				unfetchableRepo, refused, unfetchable)
		}
	})
	t.Run("the_completion_of_a_partial_row", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		dropTimelineAttributionColumns(t, ctx, pool)
		storePartialRow(t, ctx, pool, unfetchableRepo, 45)
		pr := DeliveryPullRequest{Repo: unfetchableRepo, Number: 45}

		if err := NewReconcile(pool, absentGitHub(t).newTestClient(), events.NewBroker()).completePartialPullRequest(ctx, pr, &passRateLimit{}); err != nil {
			t.Fatalf("complete the partial row: %v", err)
		}

		if _, unfetchable := timelineStampsOf(t, ctx, pool, unfetchableRepo, 45); !unfetchable {
			t.Errorf("the partial row %s#45 holds no unfetchable_at, want it set: GitHub proved it absent, and the completion logs a failed mark and goes on",
				unfetchableRepo)
		}
	})
}

// A mark takes only a row that still needs a read: a partial row, or a timeline row with no
// results row under any name of its repository, which is the row that the facts line reads. A
// timeline row that a results row heads is GitHub's, from a write that came between the proof and
// the mark, so the mark leaves it. A timeline row that the release before gave its attribution
// inputs has no results row all the same, and the facts line marks it and reads it no more.
func TestAMarkOfAnAbsentPullRequestTakesOnlyARowThatStillNeedsARead(t *testing.T) {
	t.Run("a_row_the_release_before_gave_attribution_inputs", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		storeStatementRow(t, ctx, pool, statementRow{repo: unfetchableRepo, number: 46, title: changeTitle(46), noHead: true, timeline: &statementTimeline{}})
		if _, err := pool.Exec(ctx, `update delivery_pull_requests
			set attribution_title_keys = '{}', attribution_cited_issues = '{}', attribution_branch_keys = '{}', attribution_commit_keys = '{}',
				attribution_checked_at = now()
			where repo = $1 and number = 46`, unfetchableRepo); err != nil {
			t.Fatalf("give %s#46 the attribution inputs of the release before: %v", unfetchableRepo, err)
		}

		readFactsLineOrFail(t, ctx, NewReconcile(pool, absentGitHub(t).newTestClient(), events.NewBroker()))

		if _, unfetchable := timelineStampsOf(t, ctx, pool, unfetchableRepo, 46); !unfetchable {
			t.Errorf("the timeline row %s#46 holds no unfetchable_at, want it set: GitHub proved it absent, and it has no results row", unfetchableRepo)
		}
		if counts := attributionCountsOrFail(t, ctx, pool); counts.ReadsLeft != 0 {
			t.Errorf("after the facts line reads_left = %d, want 0: a pass reads a row GitHub proved absent no more", counts.ReadsLeft)
		}
	})
	t.Run("the_rows_a_mark_takes", func(t *testing.T) {
		pool, ctx := deliveryTestPool(t)
		renameAcme(t, ctx, pool)
		storeStatementRow(t, ctx, pool, statementRow{repo: unfetchableRepo, number: 47, title: changeTitle(47), noHead: true, timeline: &statementTimeline{}})
		storeStatementRow(t, ctx, pool, statementRow{repo: unfetchableRepo, number: 48, title: changeTitle(48), timeline: &statementTimeline{}})
		storeStatementRow(t, ctx, pool, statementRow{repo: unfetchableRepo, number: 49, title: changeTitle(49), timeline: &statementTimeline{}})
		if _, err := pool.Exec(ctx, `update delivery_pull_requests set partial = true where repo = $1 and number = 49`, unfetchableRepo); err != nil {
			t.Fatalf("make %s#49 partial: %v", unfetchableRepo, err)
		}
		storeStatementRow(t, ctx, pool, statementRow{repo: renamedRepo, number: 50, title: changeTitle(50)})
		storeStatementRow(t, ctx, pool, statementRow{repo: renamedToRepo, number: 50, title: changeTitle(50), noHead: true, timeline: &statementTimeline{}})

		for _, row := range []struct {
			repo   string
			number int
			marked bool
			why    string
		}{
			{unfetchableRepo, 47, true, "a timeline row with no results row, which the facts line reads"},
			{unfetchableRepo, 48, false, "a results row heads it, so GitHub answered it"},
			{unfetchableRepo, 49, true, "a partial row, which the completion reads"},
			{renamedToRepo, 50, false, "a results row heads it under the old name of its repository"},
		} {
			if err := MarkPullRequestUnfetchable(ctx, pool, row.repo, row.number, "GitHub proved that no such pull request exists"); err != nil {
				t.Fatalf("mark %s#%d unfetchable: %v", row.repo, row.number, err)
			}
			if _, unfetchable := timelineStampsOf(t, ctx, pool, row.repo, row.number); unfetchable != row.marked {
				t.Errorf("the mark of %s#%d left unfetchable_at set %v, want %v: %s", row.repo, row.number, unfetchable, row.marked, row.why)
			}
		}
	})
}
