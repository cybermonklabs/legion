package main

import (
	"math/rand/v2"
	"testing"
)

// incompressible is n bytes a compressor cannot shrink, so a body of it gzips to about n bytes.
func incompressible(n int) []byte {
	source := rand.New(rand.NewPCG(2916, 18))
	body := make([]byte, n)
	for index := range body {
		body[index] = byte(source.UintN(256))
	}
	return body
}

// TestTheTimelineBodyIsNoBoundOfTheFirstPaint: spec R8 bounds each body of the first paint at
// 300 KB compressed, and the root's R3 keeps the row-level merge timeline out of the first paint
// (it pages, dispatch://INFRA-2918 R13). So the bench judges a read of the eval first paint whose
// body is over 300 KB a failure, and does not judge the Delivery timeline's body by that bound: a
// head whose every bound of R8 holds prints no FAIL line, and its verdict can pass. With the
// timeline judged by the first paint's body budget, every run of the head fails, and a run that
// always fails tells a regression from nothing.
func TestTheTimelineBodyIsNoBoundOfTheFirstPaint(t *testing.T) {
	body := incompressible(400 * 1000)
	if size := gzipSize(body); size <= budgetGzip {
		t.Fatalf("the test body gzips to %d bytes, within the %d budget, so it tests nothing", size, budgetGzip)
	}
	own := []statement{{queryID: 1, query: "select 1", calls: 1, execMs: 10}}
	plans := map[int64]string{1: "Index Scan using eval_days_window on eval_days"}
	timeline := []measuredCase{
		{paintCase: paintCase{name: "timeline-84d", group: "timeline-84d-compare", groupBudgetMs: budget84Ms, delivery: true},
			status: 200, body: body, own: own, plans: plans, serverMs: 100},
		{paintCase: paintCase{name: "measures-compare-84d", group: "timeline-84d-compare", groupBudgetMs: budget84Ms, delivery: true},
			status: 200, body: []byte("{}"), own: own, plans: plans, serverMs: 100},
		{paintCase: paintCase{name: "timeline-28d", budgetMs: budget28Ms, delivery: true},
			status: 200, body: body, own: own, plans: plans, serverMs: 40},
	}
	judged := &report{prefix: "RESULT"}
	judgePaint(judged, options{mode: "head"}, timeline)
	if judged.failed != 0 {
		t.Errorf("the Delivery timeline's reads, every database bound held, were judged %d failures for a body over %d bytes; want none",
			judged.failed, budgetGzip)
	}

	evals := []measuredCase{
		{paintCase: paintCase{name: "list-84d-compare", budgetMs: budget84Ms}, status: 200, body: body, own: own, plans: plans, serverMs: 100},
	}
	control := &report{prefix: "RESULT"}
	judgePaint(control, options{mode: "head"}, evals)
	if control.failed == 0 {
		t.Errorf("an eval read of the first paint whose body is over %d bytes was judged no failure; R8 bounds it", budgetGzip)
	}
}

// TestAWriteInAReadsWindowIsNoTimeOfTheRead: a background writer whose statement first runs while a
// read is in flight (a Legion facts pass's insert, which once counted 267 ms against a 28-day
// read) is the server's background, not the read's own time, while every statement a read runs
// stays its own.
func TestAWriteInAReadsWindowIsNoTimeOfTheRead(t *testing.T) {
	for _, query := range []string{
		"insert into issue_facts (key, project) values ($1, $2) on conflict (key) do update set project = excluded.project",
		"update eval_days set listed = $4 where project_key = $1",
		"delete from delivery_changes_pending where id = any($1)",
		"with moved as (delete from delivery_changes_pending returning *) insert into delivery_changes select * from moved",
		"\n\t\tINSERT INTO issue_facts (key) VALUES ($1)",
	} {
		if !writes(query) {
			t.Errorf("%q is counted as a read's own statement, want the background", query)
		}
	}
	for _, query := range []string{
		"select id, updated_at from evals_current where project_key = $1",
		"select d.day, d.n from eval_days d where d.project_key = $1 and d.listed order by d.day",
		"select count(*) from issues where deleted_at is null and status <> 'done'",
	} {
		if writes(query) {
			t.Errorf("%q is counted as the background, want a read's own statement", query)
		}
	}
}
