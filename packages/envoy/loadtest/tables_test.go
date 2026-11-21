package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTablesMarkAStepWhoseStackProcessExited(t *testing.T) {
	busy := 0.42
	ok := stepData{name: "step-1x", sum: summary{Config: summaryConfig{Step: "step-1x"}, Resources: &resourcesSummary{}},
		meta: &metaFile{BoxBusyBefore: &busy}}
	killed := stepData{name: "killed", sum: summary{
		Config:     summaryConfig{Step: "killed"},
		StackExits: []stackExit{{Name: "dispatch", PID: 4242, FirstSeenGone: time.Date(2026, 10, 27, 9, 0, 15, 0, time.UTC)}},
	}}
	md := renderTables([]stepData{ok, killed})
	if !strings.Contains(md, "| killed (dispatch exited) |") {
		t.Fatalf("the killed step's row is not marked:\n%s", md)
	}
	if strings.Contains(md, "step-1x (") {
		t.Fatalf("a step with no stack exit is marked:\n%s", md)
	}
	if !strings.Contains(md, "\n(<process> exited): ") {
		t.Fatalf("no note defines the mark:\n%s", md)
	}
	if !strings.Contains(md, "| box busy | box busy before / gate |") || !strings.Contains(md, "| 0% | 42% / - |") {
		t.Fatalf("the Resources table lacks box busy before 42%% beside no gate reading:\n%s", md)
	}
	// The killed step has no resources: its row keeps the box busy before column, empty here, and
	// its throttling column reads - for a cpu.stat not read.
	if !strings.Contains(section(md, "Resources"), "| killed (dispatch exited) | - | - | - | - | - | - | - | - | - | - | - | - | - | - | - | - |\n") {
		t.Fatalf("the killed step's Resources row is not all -:\n%s", md)
	}
	checkRowWidths(t, md)
}

// TestTablesNameTheQuietGatesReadingBesideBoxBusyBefore: the Resources table's box busy before /
// gate cell gives meta.json's box_busy_before, then the reading of run-step.sh's quiet-box gate the
// step started on (box_busy_gate), "-" for a reading not taken, and "-" alone for a step with
// neither; summary.json carries the two readings side by side.
func TestTablesNameTheQuietGatesReadingBesideBoxBusyBefore(t *testing.T) {
	before, gate := 0.01, 0.04
	step := func(name string, meta *metaFile) stepData {
		return stepData{name: name, sum: summary{Config: summaryConfig{Step: name}, Resources: &resourcesSummary{}}, meta: meta}
	}
	md := renderTables([]stepData{
		step("s-gated", &metaFile{BoxBusyBefore: &before, BoxBusyGate: &gate}),
		step("s-ungated", &metaFile{BoxBusyBefore: &before}),
		step("s-gate-only", &metaFile{BoxBusyGate: &gate}),
		step("s-no-meta", nil),
	})
	text := section(md, "Resources")
	if !strings.Contains(text, "| box busy | box busy before / gate | Dispatch RSS max MiB |") {
		t.Fatalf("the Resources header does not name the gate's reading beside box busy before:\n%s", text)
	}
	for name, want := range map[string]string{"s-gated": "1% / 4%", "s-ungated": "1% / -", "s-gate-only": "- / 4%", "s-no-meta": "-"} {
		if got, ok := columnCell(text, name, "box busy before / gate"); !ok || got != want {
			t.Errorf("%s: the box busy before / gate cell %q, want %q:\n%s", name, got, want, text)
		}
	}
	if !strings.Contains(text, "\nbox busy before / gate: ") {
		t.Errorf("no note says what the box busy before / gate cell holds:\n%s", text)
	}
	checkRowWidths(t, md)

	b, err := json.Marshal(summary{BoxBusyBefore: &before, BoxBusyGate: &gate})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"box_busy_before":0.01,"box_busy_gate":0.04`) {
		t.Errorf("summary.json does not carry box_busy_before and box_busy_gate side by side:\n%s", b)
	}
	if b, err = json.Marshal(summary{}); err != nil || !strings.Contains(string(b), `"box_busy_before":null,"box_busy_gate":null`) {
		t.Errorf("a summary.json without the readings does not carry them as null (%v):\n%s", err, b)
	}
}

// TestTablesShowThePodsThrottlingInTheResourcesTable: the Resources table's last column is how often
// and how long CFS stopped the processes of the pod loadgen run ran in over the window (summary.json
// resources.cpu_throttled_periods and resources.cpu_throttled_ms), 0 / 0 for a step never
// throttled and "-" for one that read no cpu.stat.
func TestTablesShowThePodsThrottlingInTheResourcesTable(t *testing.T) {
	periods, ms, noPeriods, noMs := int64(1234), 5678.4, int64(0), 0.0
	step := func(name string, r resourcesSummary) stepData {
		return stepData{name: name, sum: summary{Config: summaryConfig{Step: name}, Resources: &r}}
	}
	md := renderTables([]stepData{
		step("throttled", resourcesSummary{CPUThrottledPeriods: &periods, CPUThrottledMs: &ms}),
		step("never-throttled", resourcesSummary{CPUThrottledPeriods: &noPeriods, CPUThrottledMs: &noMs}),
		step("cpu-stat-unread", resourcesSummary{}),
	})
	text := section(md, "Resources")
	if !strings.Contains(text, "| Dispatch CPU-s per operation | "+podThrottledH+" |\n") {
		t.Fatalf("the Resources header does not end on the throttling column:\n%s", text)
	}
	for name, want := range map[string]string{"throttled": "1,234 / 5,678", "never-throttled": "0 / 0", "cpu-stat-unread": "-"} {
		if got, ok := columnCell(text, name, podThrottledH); !ok || got != want {
			t.Errorf("%s: the throttling cell %q, want %q:\n%s", name, got, want, text)
		}
	}
	checkRowWidths(t, md)
}

// section is the text of the markdown section that title opens, up to the next section.
func section(md, title string) string {
	_, rest, _ := strings.Cut(md, "## "+title+"\n")
	text, _, _ := strings.Cut(rest, "\n## ")
	return text
}

// checkRowWidths fails when a table row has more or fewer cells than its table's header.
func checkRowWidths(t *testing.T, md string) {
	t.Helper()
	width := 0
	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(line, "|---") {
			continue
		}
		if !strings.HasPrefix(line, "| ") {
			width = 0
			continue
		}
		cells := len(strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | "))
		if width == 0 {
			width = cells
			continue
		}
		if cells != width {
			t.Errorf("row has %d cells, its header %d: %s", cells, width, line)
		}
	}
}

func TestTablesRenderTheSamplersAndTheirFailedReads(t *testing.T) {
	xids := 412.3
	cur := summary{
		Config:    summaryConfig{Step: "new"},
		Events:    eventsSummary{DeliveryMsFromSent: stat{N: 3, P50: 120, P99: 900}},
		Resources: &resourcesSummary{XIDsPerS: &xids},
		Live:      liveSummary{Frames: 1200, OrderViolations: 2, FrameMsFromCreatedAt: stat{N: 1200, P50: 40, P99: 300}, FrameMsFromWriteResponse: stat{N: 400, P50: 5, P99: 60}},
		UngrantedLocks: ungrantedSummary{Samples: 100, ShareWithAny: 0.97, Groups: []ungrantedGroup{{
			LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock", WaitersP50: 12, WaitersMax: 40, ShareWithWaiter: 0.96,
			TopPairs: []waitPair{{WaiterQuery: "select a | b", BlockerQuery: "", Count: 800}},
		}}},
		XIDHolds: &xactSummary{IntervalMs: 10, Samples: 17000, SamplesPerS: 94.4, XIDHolders: 640, CommittedEvents: 700,
			XIDHoldMs: stat{N: 640, P50: 180, P99: 1900, Max: 2400}, XactMs: stat{N: 640, P50: 400, P99: 2600, Max: 3100},
			IdleInXactWithXIDMs: stat{N: 640, P50: 120, P99: 1500, Max: 2000}, IdleInXactNoXIDMs: stat{N: 640, P50: 0, P99: 300, Max: 450},
			LongestXIDHolders: []xidHolder{{HoldMs: 2400, XactMs: 3100, Query: "update a | b"}, {HoldMs: 2100, XactMs: 2500, Query: ""}}},
	}
	// Samplers that failed: the lock probe and the transaction-id hold sampler never connected, the
	// ungranted-lock sampler's reads all failed, the transaction-id probe has no rate.
	failed := summary{
		Config:          summaryConfig{Step: "failed"},
		Resources:       &resourcesSummary{},
		CommitOrderLock: lockSummary{Errors: 1},
		XIDHolds:        &xactSummary{IntervalMs: 10, Errors: 1},
		UngrantedLocks:  ungrantedSummary{Errors: 160},
	}
	// The transaction-id hold sampler turned off (-xact-interval 0).
	off := summary{Config: summaryConfig{Step: "off"}, XIDHolds: &xactSummary{}}
	md := renderTables([]stepData{{name: "failed", sum: failed}, {name: "new", sum: cur}, {name: "off", sum: off}})

	for _, c := range []struct{ title, failed, cur string }{
		{"Delivery from sending, and live socket frames", "| failed | - | - | - | 0 | 0 |", "| new | 120 / 900 | 40 / 300 | 5 / 60 | 1,200 | 2 |"},
		{"Commit-order lock", "| failed (1 failed read) | - | - | - | - | - |", "| new | - | - | - | - | - |"},
		{"Ungranted locks", "| failed (160 failed reads) | - | - | - |", `| new | 97% | advisory commit_order_lock ExclusiveLock 96%, 12 / 40 | select a \| b → (none) (800) |`},
		{"Transaction ids", "| failed | - |", "| new | 412.3 |"},
		{"Transaction-id holds", "| failed (1 failed read) | - | - | - | - | - | - | - |",
			`| new | 10 (94) | 640 / 700 | 180 / 1,900 / 2,400 | 400 / 2,600 / 3,100 | 120 / 1,500 / 2,000 | 0 / 300 / 450 | 2,400 update a \| b; 2,100 (none) |`},
		{"Transaction-id holds", "| off | off | - | - | - | - | - | - |", "| off | off | - | - | - | - | - | - |"},
	} {
		text := section(md, c.title)
		if !strings.Contains(text, c.failed+"\n") || !strings.Contains(text, c.cur+"\n") {
			t.Errorf("section %q lacks %q or %q:\n%s", c.title, c.failed, c.cur, text)
		}
	}
	for _, c := range []struct{ title, view, key string }{
		{"Commit-order lock", "pg_locks", "commit_order_lock.errors"},
		{"Ungranted locks", "pg_locks", "ungranted_locks.errors"},
		{"Transaction-id holds", "pg_stat_activity", "xid_holds.errors"},
	} {
		if note := noteLine(section(md, c.title), "(<n> failed reads)"); !strings.Contains(note, "read of "+c.view) || !strings.Contains(note, "`"+c.key+"`") {
			t.Errorf("section %q has no note of the failed-reads mark that names the read of %s and summary.json `%s`:\n%s", c.title, c.view, c.key, md)
		}
	}
	if strings.Contains(md, "| new (") {
		t.Errorf("a step whose probes read every time is marked:\n%s", md)
	}
	checkRowWidths(t, md)
}

// noteLine is the line of text that defines mark, "" when none does.
func noteLine(text, mark string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, mark) {
			return line
		}
	}
	return ""
}

// rowCells is the cells of the table row in text whose step cell starts with step, nil when no row
// does.
func rowCells(text, step string) []string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "| "+step) {
			return strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
		}
	}
	return nil
}

// columnCell is the cell of the table row in text whose step cell starts with step (rowCells), in
// the column headed column; false where text has no such row or its table no such column. The
// header is text's first table line, which is the row rowCells finds for an empty step.
func columnCell(text, step, column string) (string, bool) {
	row := rowCells(text, step)
	if index := slices.Index(rowCells(text, ""), column); index >= 0 && index < len(row) {
		return row[index], true
	}
	return "", false
}

// cellOf is columnCell for a cell the test needs: it fails t where text has no such row or column.
func cellOf(t *testing.T, text, step, column string) string {
	t.Helper()
	cell, ok := columnCell(text, step, column)
	if !ok {
		t.Fatalf("no %q cell in the row of %s:\n%s", column, step, text)
	}
	return cell
}

func TestTablesResourcesShowFailedSamplerReadsAsMissing(t *testing.T) {
	step := func(name string, ss ...sample) stepData {
		return stepData{name: name, sum: summary{Config: summaryConfig{Step: name}, Resources: summarizeResources(ss, 0, 15)}}
	}
	md := renderTables([]stepData{
		step("unread", fakeSample(0, 0, 0, 0, readPostgres, readGoroutines, readHeap), fakeSample(5, 0, 0, 0, readPostgres, readGoroutines),
			fakeSample(10, 0, 0, 0, readPostgres, readGoroutines)),
		step("partial", fakeSample(0, 0, 0, 0, readPostgres, readGoroutines), fakeSample(5, 10, 100, 0), fakeSample(10, 12, 120, 0)),
		step("clean", fakeSample(0, 8, 90, 20), fakeSample(5, 9, 95, 0), fakeSample(10, 9, 95, 0)),
	})
	text := section(md, "Resources")
	// The columns: heap in use, goroutines, pool waiters, Postgres connections max, connections
	// waiting on a lock.
	const heap, goroutines, pool, pgMax, lockWait = 10, 11, 12, 13, 14

	unread := rowCells(text, "unread")
	if unread == nil || unread[0] != "unread (7 failed reads)" {
		t.Fatalf("the row whose reads all failed is not marked with its 7 failed reads:\n%s", text)
	}
	for _, col := range []int{heap, goroutines, pool, pgMax, lockWait} {
		if unread[col] != "-" {
			t.Errorf("column %d of the row whose reads all failed reads %q, want -:\n%s", col, unread[col], text)
		}
	}
	if unread[1] == "-" {
		t.Errorf("the /proc columns of the row whose reads failed are empty too:\n%s", text)
	}

	partial := rowCells(text, "partial")
	if partial == nil || partial[0] != "partial (2 failed reads)" {
		t.Fatalf("the row with two failed reads is not marked with them:\n%s", text)
	}
	if partial[pgMax] != "12" || partial[goroutines] != "100 / 120" {
		t.Errorf("the row with two failed reads: Postgres max %q, goroutines %q; want 12 and 100 / 120", partial[pgMax], partial[goroutines])
	}

	if clean := rowCells(text, "clean"); clean == nil || clean[0] != "clean" {
		t.Errorf("a row whose reads all succeeded is marked:\n%s", text)
	}
	note := noteLine(text, "(<n> failed reads)")
	for _, key := range []string{"resources.pg_read_errors", "resources.pprof_read_errors"} {
		if !strings.Contains(note, "`"+key+"`") {
			t.Errorf("the failed-reads note of the Resources table does not name summary.json `%s`:\n%s", key, text)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesDoNotShowUnreadCommitsAsZero: a step whose public.loadtest_event_commits could not be
// read (events.commits_error: a foreign run's database without the commit trigger, or an
// unreachable -pg) knows nothing about its commits, receipts or delivery: Committed is 0 because
// nothing was read, not because nothing committed. Its tables must not present that 0 as a
// measurement: the committed rate, the issue-topic receipts and each kind's committed count are
// not 0; each table with cells that depend on the commit rows marks the row, shows "-" in those
// cells and names events.commits_error in a note; the other cells keep their values; and neither
// a step whose commit rows were read nor a table that no commit row feeds carries the mark.
func TestTablesDoNotShowUnreadCommitsAsZero(t *testing.T) {
	perOp := 0.0123
	read := stepData{name: "read", sum: summary{
		Config:    summaryConfig{Step: "read", EventsPerS: 1},
		Events:    eventsSummary{Committed: 30, CommittedPerS: 1, DeliveryMsFromSent: stat{N: 60, P50: 120, P99: 900}},
		Resources: &resourcesSummary{DispatchCPUSPerOp: &perOp},
		Live:      liveSummary{Frames: 40, FrameMsFromWriteResponse: stat{N: 30, P50: 5, P99: 60}},
	}}
	// What run.go writes for a step whose commit rows were not read: no delivery latency, no CPU
	// per operation, and its live frames from created_at, which need no commit row.
	unread := stepData{name: "foreign-notrigger", sum: summary{
		Config: summaryConfig{Step: "foreign-notrigger", EventsPerS: 1, Sessions: 10},
		Events: eventsSummary{
			Offered:     31,
			OfferedPerS: 1.03,
			ByKind: map[string]kindSummary{
				kindComment:  {Offered: 1, WriteMs: stat{N: 1, P50: 118, P99: 118}},
				kindAnchored: {Offered: 8, WriteMs: stat{N: 8, P50: 670, P99: 720}},
			},
			CommitsError: `read public.loadtest_event_commits (is commit-trigger.sql installed?): ERROR: relation "public.loadtest_event_commits" does not exist (SQLSTATE 42P01)`,
		},
		Resources: &resourcesSummary{},
		Live:      liveSummary{Frames: 40, FrameMsFromCreatedAt: stat{N: 40, P50: 30, P99: 80}},
	}}
	md := renderTables([]stepData{read, unread})

	events := section(md, "Events, delivery and requests")
	if rate := cellOf(t, events, "foreign-notrigger", "events/s target / offered / committed"); strings.HasSuffix(rate, "/ 0.00") {
		t.Errorf("the committed rate of a step whose commit rows were not read reads %q, a measured zero:\n%s", rate, md)
	}
	if receipts := cellOf(t, events, "foreign-notrigger", "issue-topic receipts got / expected"); receipts == "0 / 0" {
		t.Errorf("the issue-topic receipts of a step whose commit rows were not read read %q, a measured zero:\n%s", receipts, md)
	}
	for line := range strings.SplitSeq(section(md, "Write latency by kind"), "\n") {
		if strings.HasPrefix(line, "| foreign-notrigger") && strings.Contains(line, "(0 / ") {
			t.Errorf("a kind's committed count of a step whose commit rows were not read reads 0:\n%s", line)
		}
	}

	const mark = "foreign-notrigger " + commitsUnreadMark
	for _, c := range []struct {
		title   string
		columns []string // the cells that depend on the commit rows
	}{
		{"Events, delivery and requests", []string{"delivery ms from arrival p50 / p99", "delivery ms from commit p50 / p99", "issue-topic receipts got / expected"}},
		{"Write latency by kind", nil},
		{"Resources", []string{"Dispatch CPU-s per operation"}},
		{"Delivery from sending, and live socket frames", []string{"NATS delivery ms from sent p50 / p99", "live frame ms from write response p50 / p99"}},
	} {
		text := section(md, c.title)
		if label, _ := columnCell(text, "foreign-notrigger", "step"); label != mark {
			t.Errorf("section %q: the unread step's row is not marked %q:\n%s", c.title, commitsUnreadMark, text)
			continue
		}
		for _, column := range c.columns {
			if cell := cellOf(t, text, "foreign-notrigger", column); cell != "-" {
				t.Errorf("section %q: the %s cell of the unread step reads %q, want -:\n%s", c.title, column, cell, text)
			}
		}
		if note := noteLine(text, commitsUnreadMark); !strings.Contains(note, "`events.commits_error`") {
			t.Errorf("section %q has no note of the mark that names summary.json `events.commits_error`:\n%s", c.title, text)
		}
		if label, _ := columnCell(text, "read", "step"); label != "read" {
			t.Errorf("section %q: the step whose commit rows were read is marked:\n%s", c.title, text)
		}
	}
	// The cells that do not depend on the commit rows keep their values.
	delivery := section(md, "Delivery from sending, and live socket frames")
	if frames, fromCreated := cellOf(t, delivery, "foreign-notrigger", "live frames"), cellOf(t, delivery, "foreign-notrigger", "live frame ms from created_at p50 / p99"); frames != "40" || fromCreated != "30 / 80" {
		t.Errorf("the unread step's live frames %q and their latency from created_at %q, want 40 and 30 / 80", frames, fromCreated)
	}
	if rate := cellOf(t, events, "foreign-notrigger", "events/s target / offered / committed"); rate != "1.00 / 1.03 / -" {
		t.Errorf("the unread step's events/s: %q, want the target and offered rates and - committed", rate)
	}
	// The tables that no commit row feeds carry no mark.
	for _, title := range []string{"Commit-order lock", "Ungranted locks", "Transaction ids"} {
		if strings.Contains(section(md, title), commitsUnreadMark) {
			t.Errorf("section %q, which no commit row feeds, carries the mark:\n%s", title, md)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesMarkAFailedTransactionIDRead: a step whose transaction-id probe failed shows "-" with
// the mark "(1 failed read)", and the note names resources.xid_read_error; a step with no
// resources shows "-" unmarked, as every cell of its Resources row.
func TestTablesMarkAFailedTransactionIDRead(t *testing.T) {
	md := renderTables([]stepData{
		{name: "xid-failed", sum: summary{Config: summaryConfig{Step: "xid-failed"}, Resources: &resourcesSummary{XIDReadError: "xid probe: failed to connect"}}},
		{name: "no-window", sum: summary{Config: summaryConfig{Step: "no-window"}}},
	})
	text := section(md, "Transaction ids")
	if cells := rowCells(text, "xid-failed"); cells == nil || cells[0] != "xid-failed (1 failed read)" || cells[1] != "-" {
		t.Errorf("the failed transaction-id read is not '-' with the mark: %q\n%s", cells, text)
	}
	if cells := rowCells(text, "no-window"); cells == nil || cells[0] != "no-window" || cells[1] != "-" {
		t.Errorf("the step with no resources: %q\n%s", cells, text)
	}
	if note := noteLine(text, "(1 failed read)"); !strings.Contains(note, "`resources.xid_read_error`") {
		t.Errorf("no note of the failed-reads mark names summary.json `resources.xid_read_error`:\n%s", text)
	}
	checkRowWidths(t, md)
}

func TestTablesRenderRefusalsPoolsAndRecovery(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	refusals := func(o, d, r, e, ct, cr, rs, rt int, ms stat) refusalSummary {
		return refusalSummary{
			Codes:              map[string]int{"OVERLOADED": o, "DATABASE_BUSY": d, "REQUEST_TIMEOUT": r, "EDIT_QUEUE_FULL": e},
			ConnectionFailures: map[string]int{connectTimeout: ct, connectRefused: cr, connReset: rs, responseTimeout: rt},
			RefusalMs:          ms,
		}
	}
	sched := schedule{{Factor: 1, DurationS: 60}, {Factor: 5, DurationS: 60}, {Factor: 1, DurationS: 120}}
	cur := summary{
		Config: summaryConfig{Step: "burst", EventsPerS: 4, RequestsPerS: 60, Sessions: 400, Schedule: sched},
		Events: eventsSummary{OfferedPerS: 7.9, CommittedPerS: 7.5, Refusals: refusals(3, 1, 0, 2, 4, 0, 1, 2, stat{N: 6, P50: 2, P99: 950})},
		Requests: requestsSummary{Sent: 100, OK2xx: 90, SentPerS: 119,
			Refusals: refusals(1200, 30, 7, 0, 0, 5, 0, 1, stat{N: 1237, P50: 1, P99: 4}),
			Routes: map[string]routeSummary{
				"browser GET /api/v1/issues/{key}": {Count: 100, Failures: 10, LatencyMs: stat{N: 100, P50: 5, P99: 80}, Refusals: map[string]int{"OVERLOADED": 8, "REQUEST_TIMEOUT": 2}},
			}},
		Metrics: metricsSummary{
			Scraped: 49, Skipped: 1,
			Pools: map[string]poolMetrics{
				"background": {WaitingMax: f(0), InUseMax: f(2), AcquireP99AboveMs: f(30000), Checkouts: f(10)},
				"requests":   {WaitingMax: f(40), InUseMax: f(64), AcquireP99Ms: f(250), Checkouts: f(2400)},
			},
			CheckoutsPerRequest: f(1.5), HTTPQueuedMax: map[string]float64{"read": 12, "write": 3},
			HTTPInFlightMax: map[string]float64{"read": 16, "write": 4}, HTTPRefused: map[string]float64{"queue_full": 1200},
			DocWriterQueuedMax: f(2), SettlementsRunningMax: f(1), SettlementsWaitingMax: f(0), LogDropped: f(0),
		},
		Recovery: &recoverySummary{
			Schedule: sched.String(), DropS: 120, FactorBefore: 5, FactorAfter: 1, WindowS: 10, RefP99Ms: f(90), RefSource: recoveryRefLast,
			RecoverySeconds: f(10),
			Windows: []recoveryStep{
				{StartS: 0, Requests: 600, RequestsOK: 300, ReqMs: stat{N: 600, P99: 4000}, Offered: 40, Committed: 20},
				{StartS: 10, Requests: 600, RequestsOK: 600, ReqMs: stat{N: 600, P99: 100}, Offered: 40, Committed: 40, Recovered: true},
			},
		},
	}
	// A step from before these columns: its summary.json has none of them.
	old := summary{Config: summaryConfig{Step: "old", EventsPerS: 4, RequestsPerS: 60}}
	notRecovered := summary{Config: summaryConfig{Step: "stuck", Schedule: sched}, Recovery: &recoverySummary{
		Schedule: sched.String(), DropS: 120, FactorBefore: 5, FactorAfter: 1, WindowS: 10, RefP99Ms: f(250), RefSource: recoveryRefFlag,
		Windows: []recoveryStep{{StartS: 0}},
	}}
	md := renderTables([]stepData{{name: "burst", sum: cur}, {name: "old", sum: old}, {name: "stuck", sum: notRecovered}})

	events := section(md, "Events, delivery and requests")
	for _, header := range []string{
		"write refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL", "write connect timeout / refused / reset / response timeout",
		"write refusal ms p50 / p99", "req refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL",
		"req connect timeout / refused / reset / response timeout", "req refusal ms p50 / p99",
	} {
		if !strings.Contains(events, "| "+header+" |") {
			t.Errorf("the events table has no column %q:\n%s", header, events)
		}
	}
	want := map[string]string{
		"events/s target / offered / committed":                                         "8.00 / 7.90 / 7.50",
		"write refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL": "3 / 1 / 0 / 2",
		"write connect timeout / refused / reset / response timeout":                    "4 / 0 / 1 / 2",
		"write refusal ms p50 / p99":                                                    "2 / 950",
		"req/s target / sent":                                                           "120.0 / 119.0",
		"req refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL":   "1,200 / 30 / 7 / 0",
		"req connect timeout / refused / reset / response timeout":                      "0 / 5 / 0 / 1",
		"req refusal ms p50 / p99":                                                      "1 / 4",
	}
	for column, cell := range want {
		if got := cellOf(t, events, "burst", column); got != cell {
			t.Errorf("the %s cell of burst: %q, want %q (the targets are 1x times the mean factor 2):\n%s", column, got, cell, events)
		}
	}
	for column, cell := range map[string]string{
		"events/s target / offered / committed":                                         "4.00 / 0.00 / 0.00",
		"write refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL": "-",
		"write connect timeout / refused / reset / response timeout":                    "-",
		"write refusal ms p50 / p99":                                                    "-",
		"req refusals OVERLOADED / DATABASE_BUSY / REQUEST_TIMEOUT / EDIT_QUEUE_FULL":   "-",
	} {
		if got := cellOf(t, events, "old", column); got != cell {
			t.Errorf("the %s cell of a step without refusals or a schedule: %q, want %q", column, got, cell)
		}
	}

	routes := section(md, "Requests by route")
	if !strings.Contains(routes, "| 5 / 80 (10/100; OVERLOADED 8, REQUEST_TIMEOUT 2) |") {
		t.Errorf("the route table lacks the route's refusals:\n%s", routes)
	}

	pools := section(md, "Pools and admission")
	if got, want := rowCells(pools, "burst"), []string{"burst", "49 / 1", "requests 40, background 0", "requests 64, background 2",
		"requests 250, background >30,000", "1.50", "12 / 3", "16 / 4", "1,200 / -", "2", "1 / 0", "0"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("pools row %q, want %q", got, want)
	}
	for column, cell := range map[string]string{"metrics samples scraped / skipped": "0 / 0", "pool waiting max": "-", "log records dropped": "-"} {
		if got := cellOf(t, pools, "old", column); got != cell {
			t.Errorf("the %s cell of a step without metrics: %q, want %q", column, got, cell)
		}
	}

	recovery := section(md, "Recovery")
	for step, cells := range map[string]string{
		"burst": "| burst | 1x:60s,5x:60s,1x:120s | 120 (5x -> 1x) | 90 (last 3 windows) | 10 |",
		"old":   "| old | - | - | - | - |",
		"stuck": "| stuck | 1x:60s,5x:60s,1x:120s | 120 (5x -> 1x) | 250 (-recovery-ref-p99) | not recovered |",
	} {
		if !strings.Contains(recovery, cells+"\n") {
			t.Errorf("the Recovery row of %s is not %q:\n%s", step, cells, recovery)
		}
	}
	windows := section(md, "Recovery windows")
	for _, cells := range []string{
		"| burst | 0-10 | 50.0% | 20 / 40 | 4,000 | no |", "| burst | 10-20 | 100.0% | 40 / 40 | 100 | yes |",
		"| old | - | - | - | - | - |", "| stuck | 0-10 | - | 0 / 0 | - | no |",
	} {
		if !strings.Contains(windows, cells+"\n") {
			t.Errorf("the Recovery windows table lacks %q:\n%s", cells, windows)
		}
	}
	if strings.Contains(md, "%!") {
		t.Errorf("the tables print a formatting error (%%!):\n%s", md)
	}
	checkRowWidths(t, md)
}

// TestRecoveryTablesDoNotShowUnreadCommitsAsAVerdict: a window's verdict needs its committed
// events, so a step whose commit rows were not read shows no recovery seconds, no committed count
// and no per-window verdict: each is "-", and the row is marked. Its request cells keep their
// values.
func TestRecoveryTablesDoNotShowUnreadCommitsAsAVerdict(t *testing.T) {
	sched := schedule{{Factor: 5, DurationS: 60}, {Factor: 1, DurationS: 60}}
	unread := summary{
		Config: summaryConfig{Step: "unread", Schedule: sched},
		Events: eventsSummary{CommitsError: "read loadtest.event_commits: failed to connect"},
		Recovery: &recoverySummary{
			Schedule: sched.String(), DropS: 60, FactorBefore: 5, FactorAfter: 1, WindowS: 10, RefSource: recoveryRefLast,
			Windows: []recoveryStep{{StartS: 0, Requests: 100, RequestsOK: 100, ReqMs: stat{N: 100, P99: 50}, Offered: 40}},
		},
	}
	md := renderTables([]stepData{{name: "unread", sum: unread}})
	mark := "unread " + commitsUnreadMark
	if !strings.Contains(section(md, "Recovery"), "| "+mark+" | 5x:60s,1x:60s | 60 (5x -> 1x) | - (last 3 windows) | - |\n") {
		t.Errorf("the Recovery row of a step whose commit rows were not read is not marked with '-' recovery seconds:\n%s", md)
	}
	windows := section(md, "Recovery windows")
	if !strings.Contains(windows, "| "+mark+" | 0-10 | 100.0% | - / 40 | 50 | - |\n") {
		t.Errorf("the Recovery windows row of a step whose commit rows were not read is not marked with '-' committed and verdict:\n%s", windows)
	}
	checkRowWidths(t, md)
}

func TestTablesRenderTheRestartScenario(t *testing.T) {
	dispatchCPU, pgCPU := 41.5, 30.25
	cur := summary{
		Config: summaryConfig{Step: "restart"},
		LiveReconnects: &reconnectSummary{Stale: 1, Groups: map[string]reconnectGroup{
			"all": {Reconnects: 3, Complete: 1, AfterRestart: 2, ReplayBytesTotal: 20 << 20, Refetches: 2, RefetchRequests: 12, RefetchFailures: 1,
				RefetchBytes: 300 << 10, RefetchMs: stat{N: 12, P50: 40, P99: 90}, ReplayBytes: stat{N: 3, P50: 2048, Max: 20 << 20}},
			reconnectStale: {Reconnects: 1, ReplayBytesTotal: 20 << 20, ReplayFrames: stat{N: 1, P50: 12600, Max: 12600}, Refetches: 1},
		}},
		Restart: &restartSummary{AtS: 30, DurationS: 4.25, OldPID: 10, NewPID: 20, DispatchCPUS: &dispatchCPU,
			DispatchCPUSPer5s: []float64{0.5, 6, 4.25}, PostgresCPUS: &pgCPU},
		CommentMentions: &mentionSummary{Frac: 0.2, PerComment: 3,
			Window:       mentionSplit{With: mentionGroup{Offered: 10, OK2xx: 9, WriteMs: stat{N: 9, P50: 700, P99: 1500}}, Without: mentionGroup{Offered: 40, OK2xx: 40, WriteMs: stat{N: 40, P50: 60, P99: 200}}},
			AfterRestart: &mentionSplit{With: mentionGroup{Offered: 2, OK2xx: 1, WriteMs: stat{N: 1, P50: 900, P99: 900}}},
		},
	}
	md := renderTables([]stepData{{name: "old", sum: summary{Config: summaryConfig{Step: "old"}}}, {name: "restart", sum: cur}})

	for _, c := range []struct {
		title, old string
		cur        []string
	}{
		{"Live reconnects", "| old | - | - | - | - | - | - | - | - | - | - | - | - | - | - |", []string{
			"| restart | all | 3 (1) | 2 | 0 | - | - | - | 2 / 20,480 | 20,480 | - | 2 | 12 (1) | 300 | 40 / 90 |",
			"| restart | stale | 1 (0) | 0 | 0 | - | - | 12,600 / 12,600 | - | 20,480 | - | 1 | 0 (0) | 0 | - |",
		}},
		{"Restart", "| old | - | - | - | - | - | - | - | - |", []string{"| restart | 30 | 4.2 | 10 → 20 | 41.50 | 0.50 6.00 4.25 | 30.25 | - | - |"}},
		{"Comment mentions", "| old | - | - | - | - | - |", []string{"| restart | 20% / 3 | 700 / 1,500 (9 / 10) | 60 / 200 (40 / 40) | 900 / 900 (1 / 2) | - |"}},
	} {
		text := section(md, c.title)
		if !strings.Contains(text, c.old+"\n") {
			t.Errorf("section %q lacks %q:\n%s", c.title, c.old, text)
		}
		for _, want := range c.cur {
			if !strings.Contains(text, want+"\n") {
				t.Errorf("section %q lacks %q:\n%s", c.title, want, text)
			}
		}
	}
}

func TestTablesRenderEditorsAndRoomsPerStep(t *testing.T) {
	var old summary
	if err := json.Unmarshal([]byte(`{"config":{"step":"old"}}`), &old); err != nil {
		t.Fatal(err)
	}
	perUpdate, share, bytesPerS := 1.75, 0.25, 2048.0
	count, written, total := int64(4), int64(4), 31.2
	edited := summary{
		Config: summaryConfig{Step: "editors-1x"},
		Editors: &editorsSummary{
			Editors: 3, Docs: 2, SingleEditorDocs: []string{"LGA-0002"}, TargetPerS: 6, Sent: 1000, SentPerS: 5.6, Acknowledged: 990, Refused: 2,
			NotSent: map[string]int{"disconnected": 4}, Connects: 3, ConnectFailures: map[string]int{"http 503": 2}, Closes: map[string]int{"1006": 1},
			RelayMs: stat{N: 600, P50: 12, P99: 80}, Markers: 20, MarkersFound: 19, DurableMs: stat{N: 19, P50: 40, P99: 150},
		},
		Rooms: &roomsSummary{
			RoundTrips:  &roundTripsSummary{All: 9000, Room: 1750, RoomPerUpdate: &perUpdate},
			Connections: &roomConnections{Samples: 1800, Held: stat{N: 1800, P50: 2, P99: 9, Max: 12}, SharedPoolShare: &share},
			Batches:     &dispatchRooms{AppendBatches: 400, AppendedUpdates: 1000, AppendRoundTrips: 800, BatchSizes: map[string]uint64{"1": 100, "3-4": 50, "2": 250, "5-8": 0}},
			Versions:    versionsSummary{BytesPerS: &bytesPerS, Written: &written, InsertCount: &count, InsertTotalMs: &total, InsertMs: stat{N: 4, P50: 6.5, P99: 12}},
		},
	}
	restOnly := summary{
		Config: summaryConfig{Step: "rest-only"},
		Rooms:  &roomsSummary{RoundTripsNote: "stack.json names no proxy_stats port", ConnectionsNote: "the room sampler runs only in a step with editors", BatchesNote: notServed},
	}
	// A build whose /debug/vars answered 404: the note carries the response body, newline and "|"
	// included; the room sampler's one read failed.
	notFound := summary{
		Config: summaryConfig{Step: "rooms-404"},
		Rooms: &roomsSummary{RoundTripsNote: "stack.json names no proxy_stats port", Connections: &roomConnections{Errors: 1},
			BatchesNote: "not served by this build (http://127.0.0.1:16061/debug/vars: 404 404 page | not found\n)"},
	}
	md := renderTables([]stepData{{name: "old", sum: old}, {name: "editors-1x", sum: edited}, {name: "rest-only", sum: restOnly}, {name: "rooms-404", sum: notFound}})
	const notFoundCell = `not served by this build (http://127.0.0.1:16061/debug/vars: 404 404 page \| not found )`
	for _, c := range []struct{ title, want string }{
		{"Editors", "| old | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "| editors-1x | 3 on 2 (1) | 6.0 / 5.6 | 1,000 / 990 / 2 | 4 (disconnected 4) | 12 / 80 | 40 / 150 (19 / 20) | 3 / 2 (http 503 2) | 1006: 1 |\n"},
		{"Editors", "| rest-only | - | - | - | - | - | - | - | - |\n"},
		{"Editors", "Documents with one editor, which have no relay latency: editors-1x: LGA-0002."},
		{"Document rooms", "| old | - | - | - | - | - | - | - | - | - |\n"},
		{"Document rooms", "| editors-1x | 1,750 / 9,000 | 1.75 | 2 / 9 / 12 | 25% | 400 / 1,000 / 800 | 1: 100, 2: 250, 3-4: 50 | 2.0 | 4 | 6.5 / 12.0 (4, 31) |\n"},
		{"Document rooms", "| rest-only | - | - | - | - | not served by this build | not served by this build | - | - | - |\n"},
		{"Document rooms", "rest-only: round trips not measured: stack.json names no proxy_stats port."},
		{"Document rooms", "\n| rooms-404 (1 failed read) | - | - | - | - | " + notFoundCell + " | " + notFoundCell + " | - | - | - |\n"},
	} {
		if text := section(md, c.title); !strings.Contains(text, c.want) {
			t.Errorf("section %q lacks %q:\n%s", c.title, c.want, text)
		}
	}
	if note := noteLine(section(md, "Document rooms"), "(<n> failed reads)"); !strings.Contains(note, "`rooms.connections.errors`") {
		t.Errorf("the Document rooms table has no note of the failed-reads mark that names summary.json `rooms.connections.errors`:\n%s", md)
	}
	for _, step := range []string{"editors-1x (", "rest-only ("} {
		if strings.Contains(section(md, "Document rooms"), "| "+step) {
			t.Errorf("a step whose room sampler read every time, or never ran, is marked:\n%s", md)
		}
	}
	checkRowWidths(t, md)
}

// TestTablesNameTheDashboardTransport: the live table names the transport per step as soon as one
// step followed the event feed on something other than the dashboard live socket, and a step of a
// run from before the dashboard probe, whose summary.json has no config.dashboard, reads live
// there.
func TestTablesNameTheDashboardTransport(t *testing.T) {
	step := func(name, dashboard string) stepData {
		return stepData{name: name, sum: summary{Config: summaryConfig{Step: name, Dashboard: dashboard}}}
	}
	md := renderTables([]stepData{step("socket", dashboardLive), step("older", ""), step("events", dashboardSSE)})
	text := section(md, "Delivery from sending, and live socket frames")
	for step, want := range map[string]string{"socket": dashboardLive, "older": dashboardLive, "events": dashboardSSE} {
		cells := rowCells(text, step)
		if cells == nil || cells[len(cells)-1] != want {
			t.Errorf("step %s names transport %q, want %q:\n%s", step, cells, want, text)
		}
	}
	if note := noteLine(text, "transport:"); !strings.Contains(note, "`config.dashboard`") {
		t.Errorf("no note of the transport column names summary.json `config.dashboard`:\n%s", text)
	}
	checkRowWidths(t, md)

	ws := renderTables([]stepData{step("socket", dashboardLive), step("older", "")})
	wsText := section(ws, "Delivery from sending, and live socket frames")
	if strings.Contains(wsText, "transport") {
		t.Errorf("a table whose every step followed the live socket names the transport:\n%s", wsText)
	}
	checkRowWidths(t, ws)
}

// TestTablesReadTheOlderLiveTransport: a step directory of a run whose loadgen named the dashboard
// transport in live.transport (ws, sse or none) and wrote no config.dashboard is read from that
// field: sse renders as sse, with the transport column and its note, and ws as the live socket.
func TestTablesReadTheOlderLiveTransport(t *testing.T) {
	read := func(name, transport string) stepData {
		t.Helper()
		dir := t.TempDir()
		body := fmt.Sprintf(`{"config": {"step": %q}, "live": {"transport": %q}}`, name, transport)
		if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := readStep(dir)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	md := renderTables([]stepData{read("older-sse", "sse"), read("older-ws", "ws")})
	text := section(md, "Delivery from sending, and live socket frames")
	for step, want := range map[string]string{"older-sse": dashboardSSE, "older-ws": dashboardLive} {
		cells := rowCells(text, step)
		if cells == nil || cells[len(cells)-1] != want {
			t.Errorf("step %s, whose summary.json names live.transport alone, reads transport %q, want %q:\n%s", step, cells, want, text)
		}
	}
	if note := noteLine(text, "transport:"); !strings.Contains(note, "`live.transport`") {
		t.Errorf("no note of the transport column names summary.json `live.transport`:\n%s", text)
	}
	checkRowWidths(t, md)
}

// TestTablesShowEachPopulationOfATwoPopulationStep: a step that drove both populations
// (summary.json `populations`) gets a row per population, so the report shows production's own
// writes and reads beside the loop's at every factor; a step that drove one adds no such table.
func TestTablesShowEachPopulationOfATwoPopulationStep(t *testing.T) {
	one := stepData{name: "step-0x", sum: summary{Config: summaryConfig{Step: "step-0x"}}}
	if md := renderTables([]stepData{one}); strings.Contains(md, "Each population") {
		t.Fatalf("a step that drove one population got a populations table:\n%s", md)
	}
	two := stepData{name: "step-6x", sum: summary{
		Config: summaryConfig{Step: "step-6x", EventsPerS: 25, RequestsPerS: 291},
		Events: eventsSummary{Offered: 250, OfferedPerS: 25},
		Populations: []populationSummary{
			{Name: populationBaseline, Seed: "/work/seed.json", EventMix: eventMix{Name: defaultEventMix},
				RouteMix: defaultRouteMixName, EventsPerS: 4, RequestsPerS: 60,
				Events:   eventsSummary{OfferedPerS: 4, CommittedPerS: 4, IssueTopicReceipts: 120, ExpectedIssueTopicReceipts: 120},
				Requests: requestsSummary{Sent: 600, SentPerS: 60, OK2xx: 600}},
			{Name: populationLoop, Seed: "/work/loopseed.seed.json", EventMix: eventMix{Name: loopEventMix},
				RouteMix: "routes-spec-loop-2026-11-03.txt", EventsPerS: 21, RequestsPerS: 231,
				Events:   eventsSummary{OfferedPerS: 21, CommittedPerS: 20, IssueTopicReceipts: 600, ExpectedIssueTopicReceipts: 630},
				Requests: requestsSummary{Sent: 2310, SentPerS: 231, OK2xx: 2300}},
		},
	}}
	md := renderTables([]stepData{two})
	text := section(md, "Each population of a two-population step")
	for _, want := range []string{"| step-6x | baseline | /work/seed.json | " + defaultEventMix + " | 4.00 / 4.00 / 4.00 |",
		"| step-6x | loop | /work/loopseed.seed.json | " + loopEventMix + " | 21.00 / 21.00 / 20.00 |",
		"| 60.0 / 60.0 | 100.0% |", "| 231.0 / 231.0 | 99.6% |"} {
		if !strings.Contains(text, want) {
			t.Errorf("the populations table lacks %q:\n%s", want, text)
		}
	}
	checkRowWidths(t, md)
}
