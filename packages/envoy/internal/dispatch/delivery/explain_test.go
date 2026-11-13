package delivery

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// analyzedPlan runs statement under `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` with sequential
// scans off, in a transaction of its own that is rolled back, and answers its one plan node for
// explaintest's checks and the plan's text, which a failure prints. This package's fixtures hold
// a handful of rows, which is read fastest whole however a statement is written, so without that
// setting the planner answers a sequential scan whatever indexes exist and the plan says nothing
// about the statement; with it, a read that no index serves still shows up, as a sequential scan
// or a scan carrying no index condition. ANALYZE runs the statement, so the plan is the one that
// read the rows, with the buffers it touched; the rollback keeps a write it makes out of the
// database.
func analyzedPlan(t *testing.T, ctx context.Context, pool *store.Pool, statement string, arguments ...any) (json.RawMessage, string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local enable_seqscan = off"); err != nil {
		t.Fatalf("disable sequential scans: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, arguments...).Scan(&raw); err != nil {
		t.Fatalf("explain the statement: %v", err)
	}
	return explaintest.OnePlan(t, raw), string(raw)
}

// indexScanOf is the one scan of plan that reads relation through index, failing the test when
// the plan holds none or more than one.
func indexScanOf(t *testing.T, plan json.RawMessage, raw, relation, index string) explaintest.IndexScan {
	t.Helper()
	var found []explaintest.IndexScan
	for _, scan := range explaintest.IndexScans(t, plan) {
		if scan.Index == index {
			found = append(found, scan)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the plan reads %s through %s %d times; want once:\n%s", relation, index, len(found), raw)
	}
	return found[0]
}

// scannedRelations is every relation a node of plan reads, once each.
func scannedRelations(t *testing.T, plan json.RawMessage) []string {
	t.Helper()
	var relations []string
	for _, scan := range explaintest.Scans(t, plan) {
		if !slices.Contains(relations, scan.Relation) {
			relations = append(relations, scan.Relation)
		}
	}
	return relations
}
