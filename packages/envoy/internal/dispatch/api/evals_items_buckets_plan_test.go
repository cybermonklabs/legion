package api

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestTheStepReadsUseTheSeriesIndex covers the M4 statement beside the same-named evals test
// for the two older step reads. Leave sequential scans enabled.
func TestTheStepReadsUseTheSeriesIndex(t *testing.T) {
	t.Parallel()
	_, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	const id = "EVL/steps"
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := database.Pool.Exec(ctx, `
		insert into eval_readings (eval_id, series, row_key, row_kind, col, value, measured_at, recorded_by)
		select e.id, s.series, n::text, 'sample', s.col, n, $2::timestamptz + n * interval '1 minute', '{"kind":"user","id":"alice"}'
		from (values ($1), ('EVL/other')) as e(id),
		     (values ('cap',''), ('queue','value'), ('queue','limit'), ('holds','')) as s(series,col),
		     generate_series(1,8000) as n`, id, base); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, "analyze eval_readings"); err != nil {
		t.Fatal(err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := &explaintest.RecordedTx{Tx: tx}
	ranges := []evals.Range{
		{From: base.Add(4000 * time.Minute), To: base.Add(10000 * time.Minute)},
		{From: base.Add(2000 * time.Minute), To: base.Add(4000 * time.Minute)},
	}
	kept, err := bucketedReadingsIn(ctx, q, id, []string{"queue"}, nil, nil, ranges, evalItemsRequest{buckets: 160})
	if err != nil {
		t.Fatal(err)
	}
	for i, rows := range kept {
		if len(rows) == 0 || len(rows) > 2*160*5 {
			t.Errorf("range %d kept %d readings, want 1 to %d", i, len(rows), 2*160*5)
		}
	}
	if len(q.Sent) != 1 {
		t.Fatalf("M4 read sent %d statements, want one for both ranges", len(q.Sent))
	}
	stmt := q.Sent[0]
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+stmt.SQL, stmt.Args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	plan := explaintest.OnePlan(t, raw)
	outline := explaintest.Outline(t, plan)
	if explaintest.SeqScansRelation(t, plan, "eval_readings") {
		t.Errorf("M4 scanned eval_readings sequentially:\n%s", outline)
	}
	var timeBound bool
	for _, scan := range explaintest.IndexScans(t, plan) {
		if !strings.HasPrefix(scan.Index, "eval_readings") {
			continue
		}
		if scan.Index != "eval_readings_series" || !strings.Contains(scan.Cond, "eval_id =") {
			t.Errorf("M4 used %s with condition %q, want the series index constrained by eval ID:\n%s", scan.Index, scan.Cond, outline)
		}
		timeBound = timeBound || (strings.Contains(scan.Cond, "measured_at >=") && strings.Contains(scan.Cond, "measured_at <"))
	}
	if !timeBound {
		t.Errorf("M4 did not apply the time range in the index:\n%s", outline)
	}
	t.Logf("M4 plan:\n%s", outline)
}
