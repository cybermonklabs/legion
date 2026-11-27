package main

import (
	"strings"
	"testing"
)

// TestAStaleCutsReadAfterItsFirstPaintIsHeldToTheBudget: by the architect's ruling
// (dispatch://INFRA-2916/comment/813ac6b9-2c45-41b0-a5e4-b839542f0d0c, option A) a read of a stale
// cut made after its first paint is folded is held to the first paint's budget, 150 ms for an
// 84-day read with a compare range and 60 ms for the 28-day read, and fails over it, naming the
// read and its time; a read made before the first paint is folded is recorded and never fails on
// its time; and a read whose items differ from the stored read's fails in both states. The
// information round judges no time.
func TestAStaleCutsReadAfterItsFirstPaintIsHeldToTheBudget(t *testing.T) {
	t.Parallel()
	read := func(name string, ms, budget float64, painted, equal bool) staleRead {
		return staleRead{name: name, line: "k16 fallback case=" + name, dbMs: ms, budgetMs: budget, painted: painted, equal: equal}
	}
	for _, row := range []struct {
		name  string
		read  staleRead
		info  bool
		fails []string
	}{
		{"an 84-day read after the first paint at 150.01 ms", read("list-84d-compare-repo-stale-ready", 150.01, budget84Ms, true, true), false,
			[]string{"list-84d-compare-repo-stale-ready", "150.01 ms"}},
		{"an 84-day read one window back after the first paint at 150.01 ms", read("list-84d-compare-repo-past-ready-all", 150.01, budget84Ms, true, true), false,
			[]string{"list-84d-compare-repo-past-ready-all", "150.01 ms"}},
		{"an 84-day read after the first paint at 149 ms", read("list-84d-compare-stale-all-ready", 149, budget84Ms, true, true), false, nil},
		{"a 28-day read after the first paint at 60.5 ms", read("list-28d-repo-stale-ready", 60.5, budget28Ms, true, true), false,
			[]string{"list-28d-repo-stale-ready", "60.50 ms"}},
		{"an 84-day read before the first paint at 900 ms", read("list-84d-compare-repo-stale-all", 900, budget84Ms, false, true), false, nil},
		{"a read before the first paint whose items differ", read("list-28d-repo-stale", 10, budget28Ms, false, false), false,
			[]string{"list-28d-repo-stale", "other items"}},
		{"a read after the first paint whose items differ", read("list-28d-repo-stale-ready", 10, budget28Ms, true, false), false,
			[]string{"list-28d-repo-stale-ready", "other items"}},
		{"a read after the first paint at 200 ms in the information round", read("list-84d-compare-repo-stale-ready", 200, budget84Ms, true, true), true, nil},
	} {
		judged := &report{prefix: "RESULT"}
		judgeStaleReads(judged, row.info, []staleRead{row.read})
		if row.fails == nil {
			if len(judged.fails) != 0 {
				t.Errorf("%s was judged %q; want no failure", row.name, judged.fails)
			}
			continue
		}
		if len(judged.fails) != 1 {
			t.Errorf("%s was judged %q; want one failure", row.name, judged.fails)
			continue
		}
		for _, word := range row.fails {
			if !strings.Contains(judged.fails[0], word) {
				t.Errorf("%s was judged %q, which does not name %q", row.name, judged.fails[0], word)
			}
		}
	}
}

// TestAReadThroughTheRowIndexAloneIsToldFromOneThatReadsAnother: k2 holds the window, items and
// compare reads to eval_readings_row alone. A plan that reads eval_readings through that index, by
// an index scan or a bitmap of it, passes; one that also reads the table through another index, a
// bitmap that or-s it with another, or a sequential scan fails, as does a plan that does not scan
// the table, where the old rule passed any plan that named the index somewhere and scanned no
// table whole.
func TestAReadThroughTheRowIndexAloneIsToldFromOneThatReadsAnother(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		plan  string
		alone bool
	}{
		{"Limit\n  ->  Index Scan using eval_readings_row on eval_readings  (cost=0.56..8.58 rows=1 width=300)", true},
		{"Bitmap Heap Scan on eval_readings\n  Recheck Cond: (eval_id = 'INFRA/a'::text)\n  ->  Bitmap Index Scan on eval_readings_row\n        Index Cond: (eval_id = 'INFRA/a'::text)", true},
		{"Nested Loop\n  ->  Index Scan using eval_readings_row on eval_readings r\n  ->  Index Scan using eval_readings_pkey on eval_readings s", false},
		{"Bitmap Heap Scan on eval_readings\n  ->  BitmapOr\n        ->  Bitmap Index Scan on eval_readings_row\n        ->  Bitmap Index Scan on eval_readings_replaces", false},
		{"Seq Scan on eval_readings\n  Filter: (eval_id = 'INFRA/a'::text)", false},
		{"Index Scan using eval_readings_row on eval_readings_archive", false},
		{"", false},
	} {
		if alone := readsOnlyThrough(row.plan, "eval_readings", "eval_readings_row"); alone != row.alone {
			t.Errorf("the plan\n%s\nreads eval_readings through eval_readings_row alone: %t; want %t", row.plan, alone, row.alone)
		}
	}
}
