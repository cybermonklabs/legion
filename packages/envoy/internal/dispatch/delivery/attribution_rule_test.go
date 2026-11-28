package delivery

import (
	"testing"
	"time"
)

// TestRule9OffersMergedHeadsStoredUnderRules6And8 holds that the current rule reaches the merged
// head rows stored under rule 6 and under rule 8. Rule 8 is the tie-break alone, which a build of
// that rule stamps on its merged rows without the inputs rules 3 to 5 store, so a row it stamped
// must be judged once more. A head judged by the current rule is not offered again.
func TestRule9OffersMergedHeadsStoredUnderRules6And8(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 93)
	checked := time.Now()
	const (
		underRule6 = 601
		underRule8 = 603
		current    = 602
	)
	for _, number := range []int{underRule6, underRule8, current} {
		head := attributedHeadRow(number, &issue, sourceTitleBody)
		head.State, head.EventAt, head.CheckedAt = "merged", &checked, &checked
		if err := unrenamed().UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
			t.Fatalf("store merged head #%d: %v", number, err)
		}
	}
	// The upsert stamped every head with AttributionRuleVersion. Restore the older stamps.
	for number, rule := range map[int]int{underRule6: 6, underRule8: 8} {
		if _, err := pool.Exec(ctx, `
			update pull_request_heads set attribution_rule = $2 where repo = 'acme/widgets' and number = $1
		`, number, rule); err != nil {
			t.Fatalf("stamp merged head #%d with rule %d: %v", number, rule, err)
		}
	}

	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	offered := map[int]bool{}
	for _, head := range batch {
		if head.Repo == "acme/widgets" {
			offered[head.Number] = true
		}
	}
	if len(batch) != 2 || !offered[underRule6] || !offered[underRule8] {
		t.Fatalf("stale batch = %+v, want acme/widgets#%d under rule 6 and #%d under rule 8, not #%d under rule %d",
			batch, underRule6, underRule8, current, AttributionRuleVersion)
	}
}
