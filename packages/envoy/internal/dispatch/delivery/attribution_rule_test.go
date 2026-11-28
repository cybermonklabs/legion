package delivery

import (
	"testing"
	"time"
)

// TestRule8OffersMergedHeadsStoredUnderRule6 holds that rule 8's tie-break reaches
// the merged head rows stored under rule 6. A head judged by the current rule is
// not offered again.
func TestRule8OffersMergedHeadsStoredUnderRule6(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 93)
	// One reading: the heads' stamps and the list's moment. The list offers a merged row by its
	// rule and its null stamp, never by a time.
	checked := time.Now()
	const (
		prior   = 601
		current = 602
	)
	for _, number := range []int{prior, current} {
		head := attributedHeadRow(number, &issue, sourceTitleBody)
		head.State, head.EventAt, head.CheckedAt = "merged", &checked, &checked
		if err := UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
			t.Fatalf("store merged head #%d: %v", number, err)
		}
	}
	// The upsert stamped both heads with AttributionRuleVersion. Restore the older stamp on one.
	if _, err := pool.Exec(ctx, `
		update pull_request_heads set attribution_rule = 6 where repo = 'acme/widgets' and number = $1
	`, prior); err != nil {
		t.Fatalf("stamp merged head #%d with rule 6: %v", prior, err)
	}

	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, checked)
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	if len(batch) != 1 || batch[0].Repo != "acme/widgets" || batch[0].Number != prior {
		t.Fatalf("stale batch = %+v, want only acme/widgets#%d under rule 6, not #%d under rule %d",
			batch, prior, current, AttributionRuleVersion)
	}
}
