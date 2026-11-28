package explaintest

import (
	"os"
	"testing"
)

// bitmapRecheckPlan is testdata/bitmap_recheck.json, PostgreSQL 18.7's `EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON)` of a lossy bitmap heap scan: 2,000,000 rows of one integer fill about 8,850 heap
// pages, more than the about 1,024 exact pages a 64 kB bitmap holds, so the bitmap holds most
// pages lossily and the scan's recheck removes the rows of those pages its condition does not
// hold. It was captured at commit deb045601b7a of cybermonklabs/legion, on the server
// LEGION_TEST_PG_DSN names, whose `select version()` answers "PostgreSQL 18.7 (Debian
// 18.7-1.pgdg13+5) on x86_64-pc-linux-gnu, compiled by gcc (Debian 14.2.0-19) 14.2.0, 64-bit", by:
//
//	psql "$LEGION_TEST_PG_DSN" -X -q -At -f capture.sql
//
// where capture.sql is:
//
//	create temp table t as select (random() * 1e6)::int as a from generate_series(1, 2000000); create index on t (a); analyze t; set enable_indexscan = off; set enable_seqscan = off; set work_mem = '64kB'; explain (analyze, buffers, format json) select * from t where a < 500000;
//
// A capture whose recheck removes no row read no lossy page, and is captured again with more rows.
const bitmapRecheckPlan = "testdata/bitmap_recheck.json"

// Nodes reads the rows a bitmap heap scan's index recheck removed ("Rows Removed by Index
// Recheck") as RowsRemovedByIndexRecheck: the fixture's scan of t, which tests its rows by its
// Recheck Cond and by no Filter, removed rows by its recheck and none by a Filter, and the Bitmap
// Index Scan under it, which rechecks nothing, removed none.
func TestNodesReadTheRowsAnIndexRecheckRemoved(t *testing.T) {
	raw, err := os.ReadFile(bitmapRecheckPlan)
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	nodes := Nodes(t, OnePlan(t, raw))
	if len(nodes) != 2 || nodes[0].String() != "Bitmap Heap Scan on t" || nodes[1].Type != "Bitmap Index Scan" {
		t.Fatalf("Nodes = %v; want the Bitmap Heap Scan on t over one Bitmap Index Scan", nodes)
	}
	scan, bitmap := nodes[0], nodes[1]
	if scan.RecheckCond == "" || scan.Filter != "" {
		t.Fatalf("the scan's Recheck Cond is %q and its Filter %q; want a recheck and no filter", scan.RecheckCond, scan.Filter)
	}
	if scan.RowsRemovedByIndexRecheck <= 0 {
		t.Errorf("the scan's recheck removed %v rows; want the rows of its lossy pages its condition does not hold", scan.RowsRemovedByIndexRecheck)
	}
	if scan.RowsRemovedByFilter != 0 {
		t.Errorf("the scan's Filter removed %v rows; want none, since it has no Filter", scan.RowsRemovedByFilter)
	}
	if bitmap.RowsRemovedByIndexRecheck != 0 {
		t.Errorf("the Bitmap Index Scan's recheck removed %v rows; want none", bitmap.RowsRemovedByIndexRecheck)
	}
}
