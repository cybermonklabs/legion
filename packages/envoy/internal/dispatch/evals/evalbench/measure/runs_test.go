package main

import (
	"path/filepath"
	"testing"
)

// TestEachRunOfTheFirstPaintIsHeldToItsBudget: k8 holds every one of the first paint's eight runs
// to the budgets, so the runs fail when one run of one read, or of one group's reads together, is
// over its budget, whatever the median of the runs, which one slow run of eight leaves under it;
// and they fail when the file lacks a run, which a paint that ended before it recorded its reads
// leaves out, so the median and the maximum would be the passing runs' alone. Eight runs within
// every budget pass.
func TestEachRunOfTheFirstPaintIsHeldToItsBudget(t *testing.T) {
	read := func(name string, ms, budget float64, group string, groupBudget float64) measuredCase {
		return measuredCase{paintCase: paintCase{name: name, budgetMs: budget, group: group, groupBudgetMs: groupBudget},
			own: []statement{{queryID: 1, query: "select 1", calls: 1, execMs: ms}}}
	}
	// judge records runs runs of the paint, each with the 28-day read's and the 84-day timeline's
	// database time times answers for it, into a runs file, and answers how many FAIL lines the
	// runs over it print when they ask for eight.
	judge := func(t *testing.T, runs int, times func(run int) (list28, timeline float64)) int {
		path := filepath.Join(t.TempDir(), "paint-runs.jsonl")
		for run := 1; run <= runs; run++ {
			list28, timeline := times(run)
			measured := []measuredCase{
				read("list-84d-compare", 100, budget84Ms, "", 0),
				read("list-28d", list28, budget28Ms, "", 0),
				read("timeline-84d", timeline, 0, "timeline-84d-compare", budget84Ms),
				read("measures-compare-84d", 60, 0, "timeline-84d-compare", budget84Ms),
			}
			if err := recordRun(path, runOf(run, measured)); err != nil {
				t.Fatalf("record run %d: %v", run, err)
			}
		}
		recorded, err := readRuns(path)
		if err != nil {
			t.Fatalf("read the runs: %v", err)
		}
		judged := &report{prefix: "RESULT"}
		judgeRuns(judged, recorded, 8)
		return len(judged.fails)
	}
	steady := func(int) (float64, float64) { return 40, 70 }
	if failed := judge(t, 8, steady); failed != 0 {
		t.Errorf("eight runs within every budget printed %d FAIL lines; want none", failed)
	}
	slowRead := func(run int) (float64, float64) {
		if run == 5 {
			return 72, 70
		}
		return 40, 70
	}
	if failed := judge(t, 8, slowRead); failed != 1 {
		t.Errorf("a 28-day read of 72 ms in run 5, against its 60 ms with a median of 40 ms, printed %d FAIL lines; want 1", failed)
	}
	slowGroup := func(run int) (float64, float64) {
		if run == 3 {
			return 40, 95
		}
		return 40, 70
	}
	if failed := judge(t, 8, slowGroup); failed != 1 {
		t.Errorf("the 84-day timeline and its compare range's measures at 155 ms together in run 3, against their 150 ms, printed %d FAIL lines; want 1", failed)
	}
	if failed := judge(t, 7, steady); failed != 1 {
		t.Errorf("a runs file of seven runs where eight were asked for printed %d FAIL lines; want 1", failed)
	}
}
