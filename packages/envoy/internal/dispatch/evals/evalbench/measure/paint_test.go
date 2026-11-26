package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// withinEveryBudget is a read of the evals' first paint that holds every budget judgePaint judges:
// it answered 200, ran a statement of its own, which was planned, in 11 ms of planning and
// execution, and its server time and body are within their budgets.
func withinEveryBudget() measuredCase {
	return measuredCase{
		paintCase: paintCase{name: "list-84d-compare", budgetMs: budget84Ms, serverBudgetMs: budgetServerMs},
		status:    200, body: []byte(`{"items":[]}`), serverMs: 100,
		own:   []statement{{queryID: 1, query: "select figures from eval_range_figures", calls: 1, planMs: 1, execMs: 10}},
		plans: map[int64]string{1: "Index Scan using eval_range_figures_pkey on eval_range_figures"},
	}
}

// TestEachBudgetOfTheFirstPaintFailsItsRead: judgePaint holds each read of the first paint to its
// answer, its own statements, its database time (planning and execution, which a statement prepared
// at the first paint spends both of), its compressed body and its server time, and the paint to a
// plan examined; a group of reads to its database time together. Each row breaks one of them and
// nothing else, and is judged one FAIL line, which names what it broke; a read within every budget
// is judged none.
func TestEachBudgetOfTheFirstPaintFailsItsRead(t *testing.T) {
	t.Parallel()
	judged := &report{prefix: "RESULT"}
	judgePaint(judged, options{mode: "head"}, []measuredCase{withinEveryBudget()})
	if len(judged.fails) != 0 {
		t.Fatalf("a read within every budget was judged %q; want no failure", judged.fails)
	}
	for _, row := range []struct {
		name   string
		breaks func(*measuredCase)
		names  string
	}{
		{"an answer other than 200", func(read *measuredCase) { read.status = 500 }, "answered 500"},
		{"no statement of its own", func(read *measuredCase) { read.own = nil }, "no statement of its own"},
		{"database time over the budget, by its planning", func(read *measuredCase) { read.own[0].execMs, read.own[0].planMs = 140, 10.01 },
			"db_ms=150.01 is over its budget of 150 ms"},
		{"a body over 300 KB compressed", func(read *measuredCase) { read.body = incompressible(400 * 1000) }, "gzip_bytes="},
		{"server time over the budget", func(read *measuredCase) { read.serverMs = 401 }, "server_ms=401 is over its budget of 400 ms"},
		{"no server time logged", func(read *measuredCase) { read.serverMs = -1 }, "server_ms=-1"},
		{"no plan examined", func(read *measuredCase) { read.plans = nil }, "no plan examined"},
	} {
		read := withinEveryBudget()
		row.breaks(&read)
		judged := &report{prefix: "RESULT"}
		judgePaint(judged, options{mode: "head"}, []measuredCase{read})
		if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], row.names) {
			t.Errorf("a read with %s was judged %q; want one failure naming %q", row.name, judged.fails, row.names)
		}
	}

	group := func(name string, ms float64) measuredCase {
		read := withinEveryBudget()
		read.name, read.budgetMs, read.group, read.groupBudgetMs, read.delivery = name, 0, "timeline-84d-compare", budget84Ms, true
		read.own[0].planMs, read.own[0].execMs = 0, ms
		return read
	}
	judged = &report{prefix: "RESULT"}
	judgePaint(judged, options{mode: "head"}, []measuredCase{group("timeline-84d", 90), group("measures-compare-84d", 60.5)})
	if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], "paint-group group=timeline-84d-compare db_ms=150.50") {
		t.Errorf("a group of reads at 150.50 ms together, against its 150 ms, was judged %q; want one failure naming the group", judged.fails)
	}
	informational := withinEveryBudget()
	informational.status, informational.own[0].execMs, informational.serverMs = 500, 900, 900
	judged = &report{prefix: "INFO"}
	judgePaint(judged, options{mode: "head", info: true}, []measuredCase{informational})
	if len(judged.fails) != 0 {
		t.Errorf("the information round, which judges no budget, was judged %q; want no failure", judged.fails)
	}
}

// TestATenTimesRunRecordsTheBudgetsNoCheckGatesThere: the root's R14 and k8 state the first
// paint's database-time budgets at the stated run rate, and at ten times that rate the database
// time is judged by the head's reads set beside the base's. So at ten times a read over its
// database budget, a group of reads over theirs, a read over its budget in one of the runs and a
// base within this head's budget (k8's control) each print a RECORD line, which names the budget,
// and fail nothing, where at the stated rate each fails. What ten times changes nothing of still
// fails there: the first paint's limit, which R14 holds at ten times too (a server time over
// 0.4 s, a body over 300 KB compressed), a read that answered 500, a read whose server time the
// server did not log, and a stale cut's read over its budget after its first paint, which the
// architect's ruling gates on the run at ten times the rate.
func TestATenTimesRunRecordsTheBudgetsNoCheckGatesThere(t *testing.T) {
	t.Parallel()
	over := withinEveryBudget()
	over.own[0].planMs, over.own[0].execMs, over.serverMs, over.body = 0, 200, 900, incompressible(400*1000)
	group := func(name string, ms float64) measuredCase {
		read := withinEveryBudget()
		read.name, read.budgetMs, read.serverBudgetMs, read.group, read.groupBudgetMs, read.delivery = name, 0, 0, "timeline-84d-compare", budget84Ms, true
		read.own[0].planMs, read.own[0].execMs = 0, ms
		return read
	}
	paint := []measuredCase{over, group("timeline-84d", 120), group("measures-compare-84d", 60)}
	// runs judges eight runs of a 28-day read, at 72 ms against its 60 ms in run 5.
	runs := func(t *testing.T, judged *report) {
		path := filepath.Join(t.TempDir(), "paint-runs.jsonl")
		for run := 1; run <= 8; run++ {
			ms := 40.0
			if run == 5 {
				ms = 72
			}
			read := measuredCase{paintCase: paintCase{name: "list-28d", budgetMs: budget28Ms}, own: []statement{{queryID: 1, query: "select 1", calls: 1, execMs: ms}}}
			if err := recordRun(path, runOf(run, []measuredCase{read})); err != nil {
				t.Fatalf("record run %d: %v", run, err)
			}
		}
		recorded, err := readRuns(path)
		if err != nil {
			t.Fatalf("read the runs: %v", err)
		}
		judgeRuns(judged, recorded, 8)
	}
	// base is the base's first paint at 120 ms of database time at 84 days with a compare range,
	// within this head's 150 ms, with history on its page and a run-jobs read of the failed jobs.
	base := func() []measuredCase {
		var reads []measuredCase
		for _, one := range baseCases("", time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)) {
			reads = append(reads, measuredCase{paintCase: one, status: 200,
				body:       []byte(`{"prs":[{"issue":"INFRA-1"}],"measures":{"measures":{"change_failure_rate":{"per_pr":{"total":3}}}}}`),
				own:        []statement{{queryID: 1, query: "select run_id from delivery_run_jobs", calls: 1, execMs: 60}},
				plans:      map[int64]string{1: "Index Scan using delivery_run_jobs_failed on delivery_run_jobs"},
				statements: map[int64]string{1: "select run_id from delivery_run_jobs where conclusion = 'failure'"}})
		}
		return reads
	}
	judge := func(tenTimes bool) *report {
		judged := &report{prefix: "RESULT", tenTimes: tenTimes}
		judgePaint(judged, options{mode: "head"}, paint)
		runs(t, judged)
		baseControls(judged, base(), map[string]bool{"delivery_run_jobs_failed": true})
		return judged
	}
	recorded := []string{"db_ms=200.00 is over its budget of 150 ms", "group=timeline-84d-compare db_ms=180.00 is over its budget of 150 ms",
		"k8 case=list-28d is over its budget of 60 ms in 1 of 8 runs: run 5 (72.00 ms)", "k8-control: the base's first paint costs 120.00 ms"}
	failed := []string{"gzip_bytes=400055 is over its budget of 300000", "server_ms=900 is over its budget of 400 ms"}
	// names reports whether each of wants is named by one of lines, in any order.
	names := func(lines, wants []string) bool {
		for _, want := range wants {
			if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, want) }) {
				return false
			}
		}
		return true
	}
	if stated := judge(false); len(stated.fails) != len(recorded)+len(failed) || !names(stated.fails, append(slices.Clone(recorded), failed...)) || len(stated.records) != 0 {
		t.Errorf("at the stated rate the reads over their budgets were judged %q and recorded %q; want a failure naming each of %q and no record",
			stated.fails, stated.records, append(slices.Clone(recorded), failed...))
	}
	if tenTimes := judge(true); len(tenTimes.fails) != len(failed) || !names(tenTimes.fails, failed) || len(tenTimes.records) != len(recorded) || !names(tenTimes.records, recorded) {
		t.Errorf("at ten times the stated rate the reads over their budgets were judged %q and recorded %q; want the failures %q and the records %q",
			tenTimes.fails, tenTimes.records, failed, recorded)
	}

	broken := withinEveryBudget()
	broken.status, broken.serverMs = 500, -1
	judged := &report{prefix: "RESULT", tenTimes: true}
	judgePaint(judged, options{mode: "head"}, []measuredCase{broken})
	if len(judged.fails) != 2 || !strings.Contains(judged.fails[0], "answered 500") || !strings.Contains(judged.fails[1], "server_ms=-1") || len(judged.records) != 0 {
		t.Errorf("at ten times a read that answered 500 with no server time logged was judged %q and recorded %q; want the two failures", judged.fails, judged.records)
	}
	judged = &report{prefix: "RESULT", tenTimes: true}
	judgeStaleReads(judged, false, []staleRead{{name: "list-84d-compare-repo-stale-ready", line: "k16 fallback case=list-84d-compare-repo-stale-ready",
		dbMs: 150.01, budgetMs: budget84Ms, painted: true, equal: true}})
	if len(judged.fails) != 1 || len(judged.records) != 0 {
		t.Errorf("at ten times a stale cut's read of 150.01 ms after its first paint was judged %q and recorded %q; want one failure", judged.fails, judged.records)
	}
}

// TestTheTimelineBodyIsNoBoundOfTheFirstPaint: spec R8 bounds each body of the first paint at
// 300 KB compressed, and the root's R3 keeps the row-level merge timeline out of the first paint
// (it pages, dispatch://INFRA-2918 R13). So the bench judges a read of the eval first paint whose
// body is over 300 KB a failure, and does not judge the Delivery timeline's body by that bound: a
// head whose every bound of R8 holds prints no FAIL line, and its verdict can pass. With the
// timeline judged by the first paint's body budget, every run of the head fails, and a run that
// always fails tells a regression from nothing.
func TestTheTimelineBodyIsNoBoundOfTheFirstPaint(t *testing.T) {
	t.Parallel()
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
	if len(judged.fails) != 0 {
		t.Errorf("the Delivery timeline's reads, every database bound held, were judged %q for a body over %d bytes; want none",
			judged.fails, budgetGzip)
	}

	evals := []measuredCase{
		{paintCase: paintCase{name: "list-84d-compare", budgetMs: budget84Ms}, status: 200, body: body, own: own, plans: plans, serverMs: 100},
	}
	control := &report{prefix: "RESULT"}
	judgePaint(control, options{mode: "head"}, evals)
	if len(control.fails) != 1 || !strings.Contains(control.fails[0], "gzip_bytes=") {
		t.Errorf("an eval read of the first paint whose body is over %d bytes, every other budget held, was judged %q; want one failure of its body, which R8 bounds",
			budgetGzip, control.fails)
	}
}

// compressedAt is how many bytes body takes compressed at level.
func compressedAt(t *testing.T, body []byte, level int) int {
	t.Helper()
	var packed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&packed, level)
	if err != nil {
		t.Fatalf("a gzip writer at level %d: %v", level, err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("compress: %v", err)
	}
	return packed.Len()
}

// TestABodyIsJudgedByTheBytesTheServerSends: the server compresses its answers at gzip.BestSpeed
// (api/response_compression.go, gzipWriters), which sends about a ninth more bytes than the default
// level 6. An eval list whose body the server sends in more than 300 KB fails R8's body budget,
// although level 6 would pack it under the budget: the bench judges the bytes on the wire.
func TestABodyIsJudgedByTheBytesTheServerSends(t *testing.T) {
	t.Parallel()
	source := rand.New(rand.NewPCG(2916, 50))
	var items []string
	list := func(n int) []byte {
		for len(items) < n {
			index := len(items)
			items = append(items, fmt.Sprintf(`{"id":"INFRA/eval-%d","series":[{"series":"minutes","window":{"n":%d,"median":%.4f,"p90":%.4f},"compare":{"n":%d,"median":%.4f}}]}`,
				index, source.IntN(500), source.Float64()*100, source.Float64()*400, source.IntN(500), source.Float64()*100))
		}
		return []byte(`{"items":[` + strings.Join(items[:n], ",") + `]}`)
	}
	// The fewest items whose list the server sends in more than the budget.
	low, high := 1, 1024
	for compressedAt(t, list(high), gzip.BestSpeed) <= budgetGzip {
		low, high = high, high*2
	}
	for low < high {
		middle := (low + high) / 2
		if compressedAt(t, list(middle), gzip.BestSpeed) > budgetGzip {
			high = middle
		} else {
			low = middle + 1
		}
	}
	body := list(high)
	if packed := compressedAt(t, body, gzip.DefaultCompression); packed > budgetGzip {
		t.Fatalf("the test body packs into %d bytes at level 6, over the %d budget, so it tells the two levels apart in nothing", packed, budgetGzip)
	}
	read := withinEveryBudget()
	read.body = body
	judged := &report{prefix: "RESULT"}
	judgePaint(judged, options{mode: "head"}, []measuredCase{read})
	if len(judged.fails) != 1 || !strings.Contains(judged.fails[0], fmt.Sprintf("gzip_bytes=%d", compressedAt(t, body, gzip.BestSpeed))) {
		t.Errorf("an eval list the server sends in %d bytes (%d at level 6) was judged %q; want one failure of its body at the bytes the server sends",
			compressedAt(t, body, gzip.BestSpeed), compressedAt(t, body, gzip.DefaultCompression), judged.fails)
	}
}

// TestEachListReadOfTheFirstPaintComesFirstAfterARestart: R8 holds each read of the first paint to
// its budget on the first read after a restart, the 28-day reads as well as the 84-day ones, so over
// k8's eight runs each of the four list reads leads two of them, every run makes the same reads, and
// every read of the evals' first paint, with a repository or with none, is held to the first
// paint's server time (R16's 0.4 s).
func TestEachListReadOfTheFirstPaintComesFirstAfterARestart(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	firsts := map[string]int{}
	var first []string
	for run := 1; run <= 8; run++ {
		cases := headCases("INFRA", today, run)
		firsts[cases[0].name]++
		var names []string
		for _, one := range cases {
			names = append(names, one.name)
			if !one.delivery && one.serverBudgetMs != budgetServerMs {
				t.Errorf("run %d: %s is held to a server budget of %.0f ms; want the first paint's %d", run, one.name, one.serverBudgetMs, budgetServerMs)
			}
		}
		names = slices.Sorted(slices.Values(names))
		if first == nil {
			first = names
		} else if !slices.Equal(names, first) {
			t.Errorf("run %d makes the reads %q; run 1 made %q", run, names, first)
		}
	}
	for _, name := range []string{"list-84d-compare", "list-84d-compare-repo", "list-28d", "list-28d-repo"} {
		if firsts[name] != 2 {
			t.Errorf("%s is the first read after the restart in %d of eight runs; want 2 (firsts %v)", name, firsts[name], firsts)
		}
	}
}

// TestNoReadOfTheEvalsFirstPaintReadsThePopulation: k8 holds that no read of the page reads a
// project's population, which each plan shows. A read of the evals' first paint may read the
// stored figures, events by their id alone, and issues through issues_open_p0, the open P0 issues
// alone; a scan of any other table of the population fails, by index as well as in whole, and so
// does any other scan of issues. The Delivery timeline's reads are not this rule's.
func TestNoReadOfTheEvalsFirstPaintReadsThePopulation(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		plan  string
		fails bool
	}{
		{"Index Scan using eval_range_figures_pkey on eval_range_figures", false},
		{"Index Scan using issues_open_p0 on issues", false},
		{"Index Only Scan using events_pkey on events", false},
		{"Index Scan using delivery_pull_requests_merged on delivery_pull_requests", true},
		{"Bitmap Heap Scan on delivery_runs\n  ->  Bitmap Index Scan on delivery_runs_repo", true},
		{"Index Scan using eval_readings_row on eval_readings", true},
		{"Index Scan using issues_pkey on issues", true},
		{"Seq Scan on issues", true},
		{"Seq Scan on issue_facts", true},
		{"Index Scan using legion_observations_pkey on legion_observations", true},
	} {
		read := measuredCase{paintCase: paintCase{name: "list-84d-compare"},
			plans: map[int64]string{1: row.plan}, statements: map[int64]string{1: "select key from t"}}
		judged := &report{prefix: "RESULT"}
		checkFirstPaintReads(judged, []measuredCase{read})
		if failed := len(judged.fails) > 0; failed != row.fails {
			t.Errorf("a read of the evals' first paint with the plan %q was judged %q; want failed=%t", row.plan, judged.fails, row.fails)
		}
		timeline := measuredCase{paintCase: paintCase{name: "timeline-84d", delivery: true},
			plans: map[int64]string{1: row.plan}, statements: map[int64]string{1: "select key from t"}}
		evals := measuredCase{paintCase: paintCase{name: "list-28d"},
			plans: map[int64]string{2: "Index Scan using eval_day_figures_pkey on eval_day_figures"}, statements: map[int64]string{2: "select figures from eval_day_figures"}}
		judged = &report{prefix: "RESULT"}
		checkFirstPaintReads(judged, []measuredCase{timeline, evals})
		if len(judged.fails) != 0 {
			t.Errorf("a Delivery read with the plan %q was judged %q by the evals' population rule; want none", row.plan, judged.fails)
		}
	}
}

// TestTheBaseControlFailsABaseWithinTheBudget: k8's negative control is that the base's first
// paint of 84 days with a compare range costs more database time than this head's 84-day budget
// (564 ms at the commit before the change). A base whose paint costs 564 ms passes the control,
// and a base within the budget fails it, since it shows no read the change brought under the
// budget, however much the base's 28-day read costs beside it: the control counts the reads of
// baseCases' group alone.
func TestTheBaseControlFailsABaseWithinTheBudget(t *testing.T) {
	t.Parallel()
	failed := map[string]bool{"delivery_run_jobs_failed": true}
	timeline := `{"prs":[{"issue":"INFRA-1"}],"measures":{"measures":{"change_failure_rate":{"per_pr":{"total":3}}}}}`
	measures := `{"measures":{"change_failure_rate":{"per_pr":{"total":2}}}}`
	today := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	base := func(ms, ms28 float64) []measuredCase {
		var reads []measuredCase
		for _, one := range baseCases("", today) {
			cost, body := ms/2, timeline
			if one.group == "" {
				cost = ms28
			}
			if one.kind == "measures" {
				body = measures
			}
			reads = append(reads, measuredCase{paintCase: one, status: 200, body: []byte(body),
				own:        []statement{{queryID: 1, query: "select run_id from delivery_run_jobs", calls: 1, execMs: cost}},
				plans:      map[int64]string{1: "Index Scan using delivery_run_jobs_failed on delivery_run_jobs"},
				statements: map[int64]string{1: "select run_id from delivery_run_jobs where conclusion = 'failure'"}})
		}
		return reads
	}
	over := &report{prefix: "RESULT"}
	baseControls(over, base(564, 50), failed)
	if len(over.fails) != 0 {
		t.Errorf("a base whose first paint costs 564 ms was judged %q; want the control to pass", over.fails)
	}
	within := &report{prefix: "RESULT"}
	baseControls(within, base(120, 500), failed)
	if len(within.fails) != 1 || !strings.Contains(within.fails[0], "within this head's budget") {
		t.Errorf("a base whose first paint costs 120 ms, within the 150 ms budget, beside a 28-day read of 500 ms, was judged %q; want one failure of k8's control", within.fails)
	}
}

// TestTheBasesReadsAreTheHeadsTimelineReads: the base's Delivery page is read at 84 days with a
// compare range, as one group, and at 28 days, the reads the head's timeline reads are set beside,
// each under a budget of none, and the history a failure day is added to is read as the history
// without it, its reads named with the prefix the head's failure-day reads carry.
func TestTheBasesReadsAreTheHeadsTimelineReads(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	for _, prefix := range []string{"", "failure-day-"} {
		head := timelineCases(prefix, today)
		var names, groups []string
		for _, one := range baseCases(prefix, today) {
			names = append(names, one.name)
			groups = append(groups, one.group)
			if one.budgetMs != 0 || one.groupBudgetMs != 0 || one.serverBudgetMs != 0 || !one.delivery || one.view == nil {
				t.Errorf("the base's read %s (prefix %q) carries a budget, is no Delivery read or is no live view: %+v", one.name, prefix, one)
			}
		}
		wantNames := []string{prefix + "base-timeline-84d", prefix + "base-measures-compare-84d", prefix + "base-timeline-28d"}
		wantGroups := []string{head[0].group, head[1].group, ""}
		if !slices.Equal(names, wantNames) || !slices.Equal(groups, wantGroups) {
			t.Errorf("the base's reads (prefix %q) are %q in groups %q; want %q in groups %q, the head's timeline reads' own", prefix, names, groups, wantNames, wantGroups)
		}
	}
}

// TestAStatementTheCatalogueHoldsCountsForTheReadThatRanIt: a statement the background catalogue
// holds, which a background loop also runs, is the read's own as far as the read ran it, on its own
// backend, and the background's where a loop ran it on another; a statement on the read's backend
// alone is the read's whole; a write is background whatever runs it. So a read's own statement is
// not lost from its time because a loop runs the same text.
func TestAStatementTheCatalogueHoldsCountsForTheReadThatRanIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return at.Add(time.Duration(n) * time.Millisecond) }
	all := []statement{
		{queryID: 1, query: "select figures from eval_range_figures", calls: 1, execMs: 10},
		{queryID: 2, query: "select through from eval_figures_built", calls: 2, execMs: 4},
		{queryID: 3, query: "select project_key from eval_figure_cuts_stale", calls: 1, execMs: 6},
		{queryID: 4, query: "select position from eval_days_cursor", calls: 1, execMs: 3},
		{queryID: 5, query: "insert into issue_facts (key) values ($1)", calls: 1, execMs: 5},
	}
	catalog := map[int64]bool{2: true, 3: true, 4: true}
	executions := []logged{
		{at: ms(10), pid: 7, queryID: 1},
		{at: ms(20), pid: 7, queryID: 2},
		{at: ms(40), pid: 9, queryID: 2},
		{at: ms(50), pid: 7, queryID: 3},
		{at: ms(60), pid: 9, queryID: 4},
		{at: ms(70), pid: 7, queryID: 5},
	}
	own, background, catalogued := ownAndBackground(all, catalog, executions, at, ms(100), ms(600))
	if got := dbMs(own); got != 18 {
		t.Errorf("the read's own time is %.2f ms (%+v); want 18: its own statement, half the calls of the statement a loop shares, and the catalogued statement it alone ran", got, own)
	}
	if got := dbMs(background); got != 10 {
		t.Errorf("the background's time is %.2f ms (%+v); want 10: the loop's half of the shared statement, the loop's own statement and the write", got, background)
	}
	if catalogued != 2 {
		t.Errorf("%d catalogued statements were counted as the read's own; want 2", catalogued)
	}
}

// TestAWriteInAReadsWindowIsNoTimeOfTheRead: a background writer whose statement first runs while a
// read is in flight (a Legion facts pass's insert, which once counted 267 ms against a 28-day
// read) is the server's background, not the read's own time, while every statement a read runs
// stays its own.
func TestAWriteInAReadsWindowIsNoTimeOfTheRead(t *testing.T) {
	t.Parallel()
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
