package graphcensus

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// c-carried-from: the backfill's pass over artifact_reviews reads it in batches of backfillBatch
// rows, in the index-led keyset order of (artifact_id, model.ReviewOrder), over the index
// artifact_reviews_by_artifact_event_seq, which migration 0120 adds in that order.

// reviewCarryIndex is the index the pass reads artifact_reviews through.
const reviewCarryIndex = "artifact_reviews_by_artifact_event_seq"

// reviewCarryPlanDocument and reviewCarryPlanReview stand for the document and the review a page
// follows in each planned read.
const (
	reviewCarryPlanDocument = "00000000-0000-4000-8000-00000000d0c0"
	reviewCarryPlanReview   = "00000000-0000-4000-8000-00000000fe11"
)

// requireReviewCarryPagesReadTheReviewOrderIndex fails t unless each page the pass reads
// (readReviewCarryPage) - the first page, a page after a sequenced review, and a page after an
// unsequenced one - reads artifact_reviews through index scans of reviewCarryIndex alone, the read
// that walks its entries in their own order. Each plan is built with sequential and bitmap scans
// off: a fixture's rows are read fastest whole, and a bitmap scan reads its rows out of index
// order, so only then does the plan say which index the read walks.
func requireReviewCarryPagesReadTheReviewOrderIndex(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	at := time.Now()
	for _, page := range []struct {
		name, statement string
		arguments       []any
	}{
		{"the first page", reviewCarryFirstPage, []any{backfillBatch}},
		{"a page after a sequenced review", reviewCarryAfterSequence,
			[]any{reviewCarryPlanDocument, int64(1), at, reviewCarryPlanReview, backfillBatch}},
		{"a page after an unsequenced review", reviewCarryAfterUnsequenced,
			[]any{reviewCarryPlanDocument, at, reviewCarryPlanReview, backfillBatch}},
	} {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the plan's transaction: %v", err)
		}
		for _, setting := range []string{"set local enable_seqscan = off", "set local enable_bitmapscan = off"} {
			if _, err := tx.Exec(ctx, setting); err != nil {
				t.Fatalf("%s: %v", setting, err)
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "explain (format json) "+page.statement, page.arguments...).Scan(&raw); err != nil {
			t.Fatalf("explain %s of the pass over artifact_reviews: %v", page.name, err)
		}
		_ = tx.Rollback(ctx)
		plan := explaintest.OnePlan(t, raw)
		reads := 0
		for _, node := range explaintest.Nodes(t, plan) {
			if node.Relation != "artifact_reviews" {
				continue
			}
			reads++
			if (node.Type != "Index Scan" && node.Type != "Index Only Scan") || node.Index != reviewCarryIndex {
				t.Errorf("%s of the pass reads artifact_reviews by %s, want an index scan of %s, the keyset order of (artifact_id, model.ReviewOrder): %s",
					page.name, node, reviewCarryIndex, plan)
			}
		}
		if reads == 0 {
			t.Errorf("%s of the pass reads artifact_reviews through no scan: %s", page.name, plan)
		}
		t.Logf("%s of the pass reads artifact_reviews in %d index scan(s) of %s", page.name, reads, reviewCarryIndex)
	}
}
