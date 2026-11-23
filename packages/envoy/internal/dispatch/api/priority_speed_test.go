// priority_speed_test.go is what the `advances` relation and the priorities read cost at a
// deployment's size: a page of the issue listing with the relation read in place against the
// same page without it, and the whole priorities body against the paged listing it stands
// beside. The corpus both are measured over is one build (priority_speed_seed_test.go), and
// what each number is held to is written at the test that holds it.
//
// # One corpus, two tests
//
// A database storetest.Open hands back lives as long as the test it was opened for, and these
// are two tests, so the second could not read the first's. The corpus is therefore built once,
// by whichever of the two runs first, and both sets of readings are taken over it in that run;
// the other test reads its own out of what that run left here. Every number either test prints
// is a reading taken against the one corpus, in the one run, minutes apart at most.
//
// # What these numbers are, and what they are not
//
// The relation read is held to counts of work, not to a time (speedWork): the rows every node of
// the statement's plan produced or read and threw away, and the shared buffers it accessed, as
// Postgres counts them while it runs the statement. A busy box makes the same plan do the same
// work more slowly, and the plan is made from the corpus's own statistics (settleSpeedCorpus),
// not from the box, so the counts do not move with its load. They are taken under both plans the
// server runs the statement with (speedCustomPlans): the plan made for the page's own arguments
// and the generic plan a connection's later runs get. When the read was timed instead, the box's
// load decided the ratio both ways: correct code read at 2.2 times the page without it in a
// whole-package run on a loaded box, over the bound, and a listing that walks each row's chain on
// its own read at 1.51 to 1.92 times with the box out of the way (two CPUs of their own,
// 2026-11-20) and passed, where its rows stand 4.6 times apart.
//
// The Go side of the relation read, the scan of its three columns into each row and the merge of
// each row's own ids with the ids it inherits (advancesScan), is held in the objects the page's
// scan allocates beyond the same scan without the relation (speedScanAllocsPerID). The timed row
// covered it inside its noise; a change there that costs CPU and allocates nothing more is held
// by no test now.
//
// The priorities read is still held to times. They are one box: this pod's Postgres, which
// carries every other suite running beside it. The time of record is the server's own — the
// clock covers one handler.ServeHTTP, with everything else off it — and the least of speedRuns
// runs is reported, after speedWarmups runs that are not counted, so every timed run runs the
// plan the server's connections run (speedCustomPlans). A busy box only ever adds to a run's
// time, so each operation's least is the run the box reached least, and the runs interleave the
// operations, so every operation is read over the same stretch of the run's load.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// speedRuns is how many times each priorities operation is run and timed after its warm-up runs
// (speedWarmups), interleaved with the other two: how many runs a load that slows one operation
// and spares another has to reach before the least of each reads it as cost. Twenty-one was sized
// when the relation read was timed too, on that read's ratio to the page without it (2026-11-16):
// groups of 5 passes drawn from five runs of 61 put that ratio over its bound in 31 of 100,000,
// and groups of 11 or more in none.
const speedRuns = 21

// speedCustomPlans is how many runs of a prepared statement on one connection Postgres plans for
// that run's own arguments before it chooses, at every run after, between such a plan and the
// one generic plan it then makes and keeps (choose_custom_plan in PostgreSQL's
// src/backend/utils/cache/plancache.c). pgx prepares every statement the server sends
// (QueryExecModeCacheStatement, the pool's default), so a deployment's connections run each
// statement's first five runs under the first kind and every later run under what the choice
// gives: twelve runs of each listing text on one connection gave five custom plans and seven
// generic ones (pg_prepared_statements, 2026-11-20).
const speedCustomPlans = 5

// speedWarmups is how many times each timed operation runs before any run is timed: past the
// runs Postgres plans for their own arguments, so every timed run runs the plan a deployment's
// connections run from then on. With one warm-up run, the first three timed runs of the whole
// priorities body read 22.2 to 23.0 ms and every later one 43.9 to 83.3 ms (2026-11-20): the
// least of the runs was a reading of the plan a connection runs five times and then leaves.
const speedWarmups = speedCustomPlans

// The bounds the two tests hold their readings to, from the issue's own checks: the relation
// read at no more than twice the same page without it, in rows and in buffer accesses (its scan's
// allocations are held closer, speedScanAllocsPerID); the whole priorities body under two seconds
// and under five times the paged listing; the summary under 300 ms.
const (
	speedAdvancesRatio    = 2.0
	speedPrioritiesBound  = 2 * time.Second
	speedPrioritiesRatio  = 5.0
	speedPrioritySummary  = 300 * time.Millisecond
	speedPriorityFullName = "the whole priorities body"
)

// speedGenericBuffersRatio is the most buffer accesses the page with the relation may make under
// the generic plan, against the reference the other bounds use: the page without the relation
// under the plan made for its arguments. The issue's 2.00x cannot hold that plan: it reads each
// step of every walk through a probe of issues_pkey, the repository walk's as much as the
// relation's, so correct code accesses 2.94 times the reference (8662 against 2946). The bound
// stands between correct code and the cheapest listing the row exists to fail, as measured on
// PostgreSQL 18.6 (2026-11-20 and 21): a listing that walks each row's chain on its own accesses
// 3.62 times the reference, one that reads each chain row's tuples through the log's index 3.77,
// one that walks from every issue the filters match 12.90.
const speedGenericBuffersRatio = 3.25

// speedTiming is one operation's readings: every run, in the order they were taken, so a number
// in the report can be read against the spread it came from.
type speedTiming struct {
	name string
	runs []time.Duration
}

// least is the reading of record: the quickest of the runs, the one the box's load added least
// to, which noise can only raise.
func (s speedTiming) least() time.Duration {
	return slices.Min(s.runs)
}

func (s speedTiming) String() string {
	readings := make([]string, 0, len(s.runs))
	for _, run := range s.runs {
		readings = append(readings, fmt.Sprintf("%.1f", msOf(run)))
	}
	return fmt.Sprintf("%s: %.1f ms (least of %v ms)", s.name, msOf(s.least()), readings)
}

// speedWork is the work Postgres did to answer one statement under one plan, by its own count.
// EXPLAIN (ANALYZE, BUFFERS) runs the statement and reports, for each node of the plan it ran, how
// many times the node ran, the rows it produced each time and the rows its filters read and threw
// away; and, for the whole statement, how many times it accessed a shared buffer, each access one
// read of one page of a table or an index, cached or not, so a page read twice is counted twice.
// rows is the first summed over every node and every run of it, and buffers the second; neither
// is a time. Postgres writes a node's rows as the average of one of its runs, so each is
// multiplied by the node's runs, and a node's buffer accesses cover the nodes below it, so only the
// top node's are read. answered is the rows the statement answered and walks the inheritance
// walks (Recursive Union nodes) its plan holds, which countSpeedPage and measureSpeedCorpus read
// to tell a plan read whole from one this read could not see into.
type speedWork struct {
	name     string
	nodes    int
	walks    int
	answered float64
	rows     float64
	buffers  float64
}

func (w speedWork) String() string {
	return fmt.Sprintf("%s: %.0f rows and %.0f buffer accesses over %d plan nodes", w.name, w.rows, w.buffers, w.nodes)
}

// speedReadings is every reading the two tests report, taken over the one corpus: the page with
// the relation read under each of its two plans and the page without it under its own, the Go
// side of reading each (speedScanAllocs), and the three timings with what the plan counts said of
// the runs they timed (assertSpeedPlansSettled).
type speedReadings struct {
	advancesCustom  speedWork
	advancesGeneric speedWork
	advancesUnread  speedWork
	scanRead        speedScan
	scanUnread      speedScan
	pagedIssues     speedTiming
	prioritiesFull  speedTiming
	prioritiesLite  speedTiming
	unsettledRuns   []string
}

// speedScanRuns is how many scans of the page testing.AllocsPerRun averages, after the one it does
// not count. A scan's allocations are the same on every run (4563 with the relation and 3532
// without it on each of three readings of five scans, 2026-11-21), so a few runs make the
// average exact.
const speedScanRuns = 5

// The most objects the relation read's Go side may allocate, beyond the scan of the same page
// without it (speedScanAllocs): one for each id the page's rows answer, the string the scan
// decodes it into, and three for each row, the slices of its own ids, of the ids it inherits and
// of the entries resolve answers (advancesScan). Correct code allocates 1031 objects beyond 3532
// for 753 ids over 250 rows, under the bound of 1503 (2026-11-21). A resolve that dedupes each
// inherited id against a set it builds again from the entries merged so far allocates 2628
// beyond, and fails; against twice the page without the relation, the issue's own form of the
// bound, it passes, at 1.74 times.
const (
	speedScanAllocsPerID  = 1
	speedScanAllocsPerRow = 3
)

// measuredSpeedReadings is what the first of the two tests to run left behind. It is written and
// read by tests of one package, which the testing package runs one after another; neither calls
// t.Parallel, so nothing reads it while it is being written.
var measuredSpeedReadings *speedReadings

// speedCorpusReadings answers the readings, taking them over a corpus of its own the first time
// it is asked. A build that fails leaves nothing behind, so the other test builds again rather
// than reporting readings nobody took.
func speedCorpusReadings(t *testing.T) speedReadings {
	t.Helper()
	if measuredSpeedReadings == nil {
		taken := measureSpeedCorpus(t)
		measuredSpeedReadings = &taken
	}
	return *measuredSpeedReadings
}

// TestAdvancesListingSpeed holds the `advances` relation's own cost on the listing: one page of
// 250 issues by the statement the server runs, against the same page by the one statement text
// that leaves the relation out (issueSummaryQuery's readAdvances). Both run on the same corpus
// with the same arguments, so what is between them is the relation read alone — the inheritance
// walk over the page's chains, the reading of the tuple log for every issue those chains name,
// and the three columns' join — and it is held to at most twice the work of the page without it
// (speedWork).
//
// The statement is counted under both plans the server runs it with (speedCustomPlans), each
// against one reference: the page without the relation under the plan made for its arguments,
// the fewest rows and buffer accesses it takes. Under the plan made for the page's arguments the
// relation read is held to twice the reference in rows and in buffer accesses. Under the generic
// plan it is held to twice the reference in rows, and in buffer accesses to
// speedGenericBuffersRatio, which says why that plan's buffer accesses cannot be held to twice.
// Against the page without the relation under its own generic plan (5344) no bound separates:
// the listing that walks each row's chain on its own accesses 1.99 times it, correct code 1.62.
// The Go side of the read, the scan of the three columns and the merge of each row's ids, is held
// in allocations (speedScanAllocs).
//
// The page is the heaviest the corpus holds: the chain as deep as the parent-depth cap, the
// issue that states every priority the store holds, and the issues below both.
func TestAdvancesListingSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	reference, custom, generic := readings.advancesUnread, readings.advancesCustom, readings.advancesGeneric
	t.Log(reference.String())
	t.Log(custom.String())
	t.Log(generic.String())
	rows, buffers := custom.rows/reference.rows, custom.buffers/reference.buffers
	genericRows, genericBuffers := generic.rows/reference.rows, generic.buffers/reference.buffers
	t.Logf("under the plan made for its arguments the relation read costs %.2fx the rows and %.2fx the buffer accesses of the same page without it, at most %.2fx each",
		rows, buffers, speedAdvancesRatio)
	t.Logf("under the generic plan it costs %.2fx the rows, at most %.2fx, and %.2fx the buffer accesses, at most %.2fx",
		genericRows, speedAdvancesRatio, genericBuffers, speedGenericBuffersRatio)
	if rows > speedAdvancesRatio || buffers > speedAdvancesRatio {
		t.Errorf("%s reads its advances relations in %.0f rows and %.0f buffer accesses against %.0f rows and %.0f buffer accesses without them (%.2fx and %.2fx), want each at most %.2fx",
			custom.name, custom.rows, custom.buffers, reference.rows, reference.buffers, rows, buffers, speedAdvancesRatio)
	}
	if genericRows > speedAdvancesRatio || genericBuffers > speedGenericBuffersRatio {
		t.Errorf("%s reads its advances relations in %.0f rows and %.0f buffer accesses against %.0f rows and %.0f buffer accesses without them (%.2fx and %.2fx), want at most %.2fx the rows and %.2fx the buffer accesses",
			generic.name, generic.rows, generic.buffers, reference.rows, reference.buffers, genericRows, genericBuffers, speedAdvancesRatio, speedGenericBuffersRatio)
	}
	read, unread := readings.scanRead, readings.scanUnread
	extra, allowed := read.allocs-unread.allocs, float64(speedScanAllocsPerID*read.ids+speedScanAllocsPerRow*speedCorpusPage)
	t.Logf("its Go side allocates %.0f objects for the page against %.0f without the relation (%.2fx): %.0f more for %d ids over %d rows, at most %.0f",
		read.allocs, unread.allocs, read.allocs/unread.allocs, extra, read.ids, speedCorpusPage, allowed)
	if extra > allowed {
		t.Errorf("scanning a page of %d issues with its advances relations allocates %.0f objects against %.0f without them, %.0f more for %d ids, want at most %d an id and %d a row more, %.0f",
			speedCorpusPage, read.allocs, unread.allocs, extra, read.ids, speedScanAllocsPerID, speedScanAllocsPerRow, allowed)
	}
}

// TestPrioritiesReadSpeed holds the priorities read to what the page that draws it can wait for:
// the whole body under two seconds, and under five times the paged listing of every open issue,
// which is the read it stands beside and the one a reader already waits for; and the summary the
// navigation rail reads under 300 ms, since it is drawn on every page. Every timed run runs the
// plans a deployment's connections run (speedWarmups), or the times are of nothing it runs.
func TestPrioritiesReadSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	t.Log(readings.pagedIssues.String())
	t.Log(readings.prioritiesFull.String())
	t.Log(readings.prioritiesLite.String())
	if len(readings.unsettledRuns) > 0 {
		t.Fatalf("the timed runs ran a plan a connection runs for no more than its first %d runs of a statement:\n%s",
			speedCustomPlans, strings.Join(readings.unsettledRuns, "\n"))
	}
	paged, full, summary := readings.pagedIssues.least(), readings.prioritiesFull.least(), readings.prioritiesLite.least()
	ratio := float64(full) / float64(paged)
	t.Logf("%s costs %.2fx the paged listing of every open issue, at most %.2fx", speedPriorityFullName, ratio, speedPrioritiesRatio)
	if full > speedPrioritiesBound {
		t.Fatalf("%s took %.1f ms over %d issues and %d priorities at the least of %d runs, want under %v",
			speedPriorityFullName, msOf(full), speedCorpusIssues, speedCorpusPriorities, speedRuns, speedPrioritiesBound)
	}
	if ratio > speedPrioritiesRatio {
		t.Fatalf("%s took %.1f ms against the paged listing's %.1f ms, the least of %d runs each (%.2fx), want at most %.2fx",
			speedPriorityFullName, msOf(full), msOf(paged), speedRuns, ratio, speedPrioritiesRatio)
	}
	if summary > speedPrioritySummary {
		t.Fatalf("the priorities summary took %.1f ms at the least of %d runs, want under %v", msOf(summary), speedRuns, speedPrioritySummary)
	}
}

// measureSpeedCorpus builds the corpus, holds the reads to what they answer over it, counts the
// work of the two listing texts, and takes every timing: speedWarmups passes over the three timed
// operations that are not counted, then speedRuns passes, interleaved so a box that slows in the
// middle of the run slows each of them alike, every other pass in the reverse order, so no
// operation always runs on what the one before it warmed. The pool's plan counts before and after
// the timed passes say which plans the timed runs ran (assertSpeedPlansSettled).
func measureSpeedCorpus(t *testing.T) speedReadings {
	t.Helper()
	corpus := seedSpeedCorpus(t)
	withRelation := listIssuesPageQuery
	withoutRelation := issueSummaryQuery(false, true, false)
	arguments := speedPageArguments(speedCorpusPage, 0)

	// What each measured read answers, read once before any of them is measured: a reading of a
	// read that answered the wrong thing is worth nothing.
	assertSpeedPage(t, corpus, withRelation, arguments, true)
	assertSpeedPage(t, corpus, withoutRelation, arguments, false)
	assertSpeedPriorities(t, corpus)

	readings := speedReadings{
		advancesCustom:  countSpeedPage(t, corpus, "a page of 250 issues with the advances relation read, under the plan made for its arguments", withRelation, arguments, false),
		advancesGeneric: countSpeedPage(t, corpus, "the same page under the generic plan", withRelation, arguments, true),
		advancesUnread:  countSpeedPage(t, corpus, "the same page with the relation read switched off, under the plan made for its arguments", withoutRelation, arguments, false),
		pagedIssues:     speedTiming{name: "the paged listing of every open issue"},
		prioritiesFull:  speedTiming{name: speedPriorityFullName},
		prioritiesLite:  speedTiming{name: "the priorities summary"},
		scanRead:        speedScanAllocs(t, corpus, withRelation, arguments),
		scanUnread:      speedScanAllocs(t, corpus, withoutRelation, arguments),
	}
	for _, read := range []speedWork{readings.advancesCustom, readings.advancesGeneric} {
		if read.walks != readings.advancesUnread.walks+1 {
			t.Fatalf("%s holds %d walks and the page without the relation %d, want one more, the relation's own: the plan was not read whole, so nothing was measured",
				read.name, read.walks, readings.advancesUnread.walks)
		}
	}
	operations := []struct {
		into *speedTiming
		run  func()
	}{
		{&readings.pagedIssues, func() { speedRequest(t, corpus, speedPagedListingTarget) }},
		{&readings.prioritiesFull, func() { speedRequest(t, corpus, "/api/v1/priorities") }},
		{&readings.prioritiesLite, func() { speedRequest(t, corpus, "/api/v1/priorities?summary=true") }},
	}
	for range speedWarmups {
		for _, operation := range operations {
			operation.run()
		}
	}
	before := speedPlanRuns(t, corpus)
	for pass := range speedRuns {
		order := slices.Clone(operations)
		if pass%2 == 1 {
			slices.Reverse(order)
		}
		for _, operation := range order {
			started := time.Now()
			operation.run()
			operation.into.runs = append(operation.into.runs, time.Since(started))
		}
	}
	readings.unsettledRuns = assertSpeedPlansSettled(before, speedPlanRuns(t, corpus))
	return readings
}

// speedPagedListingTarget is the paged read of every open issue: one page of the most issues a
// page holds, whose total is every open issue the corpus carries.
var speedPagedListingTarget = fmt.Sprintf("/api/v1/issues?open=true&limit=%d", speedCorpusPage)

// speedPageArguments are the arguments one page of the open listing takes, in the order
// listIssues builds them: no filter but `open`, and the page's limit and offset last. Both
// statement texts take the same list, which is what lets one measurement stand against the
// other (issueSummaryQuery).
func speedPageArguments(limit, offset int) []any {
	return []any{
		"", []string{}, "", (*time.Time)(nil), []string{}, true, []int16{}, false,
		(*time.Time)(nil), (*time.Time)(nil), []string{}, false, false, false, limit, offset,
	}
}

// countSpeedPage counts the work of one page by one statement text under one of its two plans
// (speedWork), on one connection of the pool: once to warm the connection, whose first run of a
// statement can read catalog pages a second does not, then twice to be counted. The generic plan
// is the one plan_cache_mode = force_generic_plan makes a prepared statement run from its first
// execution, the plan a connection's later runs get (speedCustomPlans), as
// analyzeGenericListingPlan reads it. Two counts that differ are a count of something beside the
// statement, so the test stops on them rather than hold either to the bound; a count that did not
// answer the page, or counted nothing, is a plan this read could not see into, so it stops on
// those too.
func countSpeedPage(t *testing.T, corpus speedCorpusShape, name, query string, arguments []any, generic bool) speedWork {
	t.Helper()
	ctx := context.Background()
	connection, err := corpus.server.deps.Store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection to count %s: %v", name, err)
	}
	defer connection.Release()
	statement, bound := "explain (analyze, buffers, format json) "+query, arguments
	if generic {
		for _, setup := range []string{"set plan_cache_mode = force_generic_plan", "prepare speed_page as " + query} {
			if _, err := connection.Exec(ctx, setup); err != nil {
				t.Fatalf("prepare %s: %v", name, err)
			}
		}
		defer func() {
			for _, teardown := range []string{"deallocate speed_page", "reset plan_cache_mode"} {
				if _, err := connection.Exec(ctx, teardown); err != nil {
					t.Errorf("%s after counting %s: %v", teardown, name, err)
				}
			}
		}()
		statement, bound = "explain (analyze, buffers, format json) execute speed_page("+speedLiterals(t, arguments)+")", nil
	}
	explain := func() speedWork {
		var raw []byte
		if err := connection.QueryRow(ctx, statement, bound...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		var plans []struct {
			Plan analyzedNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("read the plan of %s: %d plans, %v", name, len(plans), err)
		}
		top := plans[0].Plan
		work := speedWork{name: name, answered: top.Rows, buffers: top.SharedHitBlocks + top.SharedReadBlocks}
		for _, node := range top.everyNode() {
			work.nodes++
			if node.NodeType == "Recursive Union" {
				work.walks++
			}
			work.rows += node.Loops * (node.Rows + node.RemovedByFilter + node.RemovedByJoin + node.RemovedByRecheck)
		}
		return work
	}
	explain()
	first, second := explain(), explain()
	if first != second {
		t.Fatalf("two counts of %s differ, %.0f rows and %.0f buffer accesses, then %.0f rows and %.0f buffer accesses: neither is the statement's work alone",
			name, first.rows, first.buffers, second.rows, second.buffers)
	}
	if first.answered != speedCorpusPage || first.rows == 0 || first.buffers == 0 {
		t.Fatalf("%s answered %.0f rows and counted %.0f rows and %.0f buffer accesses over %d plan nodes, want the page's %d rows answered: the plan was not read, so nothing was measured",
			name, first.answered, first.rows, first.buffers, first.nodes, speedCorpusPage)
	}
	return first
}

// speedScan is the Go side of one page's read by one statement text: the objects one read
// allocates, and the `advances` ids the page's rows answer.
type speedScan struct {
	allocs float64
	ids    int
}

// speedScanAllocs reads the Go side of one page's read (speedScan), in the objects one read
// allocates (testing.AllocsPerRun over speedScanRuns reads): scanIssuePage's scan of every column
// into each row and its resolution of each row, among them the scan of the three `advances`
// columns and the merge of each row's own ids with the ids it inherits (advancesScan). The
// statement's round trip is in it as well, the same for both texts but for the rows they answer.
// An allocation count does not move with the box's load. A Go-side change that costs CPU and
// allocates nothing more is not in it.
func speedScanAllocs(t *testing.T, corpus speedCorpusShape, query string, arguments []any) speedScan {
	t.Helper()
	ctx := context.Background()
	issues, _, err := corpus.server.scanIssuePage(ctx, query, arguments)
	if err != nil {
		t.Fatalf("scan a page of the listing: %v", err)
	}
	scan := speedScan{}
	for _, issue := range issues {
		scan.ids += len(issue.Advances)
	}
	var failed error
	scan.allocs = testing.AllocsPerRun(speedScanRuns, func() {
		if _, _, err := corpus.server.scanIssuePage(ctx, query, arguments); err != nil {
			failed = err
		}
	})
	if failed != nil {
		t.Fatalf("scan a page of the listing: %v", failed)
	}
	if scan.allocs < speedCorpusPage {
		t.Fatalf("scanning a page of %d issues allocated %.0f objects: fewer than one a row, so the scan was not counted", speedCorpusPage, scan.allocs)
	}
	return scan
}

// speedLiterals writes the page's arguments (speedPageArguments) as the literals an EXECUTE names,
// since an EXECUTE under EXPLAIN takes no bound parameter: a string, a flag, a number, an empty
// list and a null time are every value the page's arguments hold, and any other stops the test
// rather than be written wrong.
func speedLiterals(t *testing.T, arguments []any) string {
	t.Helper()
	literals := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		switch value := argument.(type) {
		case string:
			literals = append(literals, "'"+strings.ReplaceAll(value, "'", "''")+"'")
		case bool:
			literals = append(literals, strconv.FormatBool(value))
		case int:
			literals = append(literals, strconv.Itoa(value))
		case []string:
			if len(value) > 0 {
				t.Fatalf("write %q as a literal: only the empty list is written", value)
			}
			literals = append(literals, "'{}'")
		case []int16:
			if len(value) > 0 {
				t.Fatalf("write %v as a literal: only the empty list is written", value)
			}
			literals = append(literals, "'{}'")
		case *time.Time:
			if value != nil {
				t.Fatalf("write %v as a literal: only the null time is written", value)
			}
			literals = append(literals, "null")
		default:
			t.Fatalf("write a %T as a literal: no literal is written for it", argument)
		}
	}
	return strings.Join(literals, ", ")
}

// speedPrepared is one statement prepared on one connection: the connection's server process and
// the statement's name, which pgx derives from its text.
type speedPrepared struct {
	backend int32
	name    string
}

// speedPreparedRuns is how many times one prepared statement has run, each run one plan, custom
// or generic, as pg_prepared_statements counts them, and the start of its text, which a failure
// prints.
type speedPreparedRuns struct {
	runs      int64
	statement string
}

// speedPlanRuns reads the runs of every statement prepared on every idle connection of the pool.
// It reads them by the simple protocol, so the read prepares no statement of its own to be
// counted.
func speedPlanRuns(t *testing.T, corpus speedCorpusShape) map[speedPrepared]speedPreparedRuns {
	t.Helper()
	ctx := context.Background()
	prepared := map[speedPrepared]speedPreparedRuns{}
	for _, connection := range corpus.server.deps.Store.Pool.AcquireAllIdle(ctx) {
		rows, err := connection.Query(ctx,
			`select pg_backend_pid(), name, generic_plans + custom_plans, left(regexp_replace(statement, '\s+', ' ', 'g'), 100) from pg_prepared_statements`,
			pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			connection.Release()
			t.Fatalf("read the plans prepared on a connection: %v", err)
		}
		for rows.Next() {
			var key speedPrepared
			var runs speedPreparedRuns
			if err := rows.Scan(&key.backend, &key.name, &runs.runs, &runs.statement); err != nil {
				rows.Close()
				connection.Release()
				t.Fatalf("read a prepared statement's plans: %v", err)
			}
			prepared[key] = runs
		}
		rows.Close()
		err = rows.Err()
		connection.Release()
		if err != nil {
			t.Fatalf("read the plans prepared on a connection: %v", err)
		}
	}
	return prepared
}

// assertSpeedPlansSettled answers each statement the timed passes ran (its runs grew between
// before and after) whose runs before them were fewer than speedCustomPlans: one of its timed
// runs ran a plan made for its own arguments that a connection runs for those first runs alone.
// A timed pass that ran no prepared statement at all is answered too, since the plans it ran
// could not be read.
func assertSpeedPlansSettled(before, after map[speedPrepared]speedPreparedRuns) []string {
	unsettled := []string{}
	ran := 0
	for key, now := range after {
		earlier := before[key].runs
		if now.runs == earlier {
			continue
		}
		ran++
		if earlier < speedCustomPlans {
			unsettled = append(unsettled, fmt.Sprintf("backend %d ran %q %d times before the timed passes and %d times in them",
				key.backend, now.statement, earlier, now.runs-earlier))
		}
	}
	if ran == 0 {
		unsettled = append(unsettled, fmt.Sprintf("no statement prepared on the %d idle connections read ran in the timed passes, so the plans they ran are unknown", len(after)))
	}
	slices.Sort(unsettled)
	return unsettled
}

// assertSpeedPage holds one of the two texts to what it answers: the page and the whole count
// either way, and the relations the corpus's heaviest issue carries only where the text reads
// them. The issue below it inherits all twenty, which is the walk the measurement is of.
func assertSpeedPage(t *testing.T, corpus speedCorpusShape, query string, arguments []any, readsRelation bool) {
	t.Helper()
	issues, total, err := corpus.server.scanIssuePage(context.Background(), query, arguments)
	if err != nil {
		t.Fatalf("read a page of the listing: %v", err)
	}
	if len(issues) != speedCorpusPage || total != speedCorpusIssues {
		t.Fatalf("the page holds %d issues of %d, want %d of %d",
			len(issues), total, speedCorpusPage, speedCorpusIssues)
	}
	rows := make(map[string]model.IssueSummary, len(issues))
	for _, issue := range issues {
		rows[issue.Key] = issue
	}
	heaviest, drawn := rows[corpus.heaviest]
	if !drawn {
		t.Fatalf("the page does not hold %s, the issue the measurement is of", corpus.heaviest)
	}
	deepest, drawn := rows[corpus.chain[len(corpus.chain)-1]]
	if !drawn {
		t.Fatalf("the page does not hold %s, the deepest issue of the chain", corpus.chain[len(corpus.chain)-1])
	}
	want := 0
	if readsRelation {
		want = contracts.MaxIssueAdvances
	}
	if len(heaviest.Advances) != want {
		t.Fatalf("%s advances %d priorities on this page, want %d",
			corpus.heaviest, len(heaviest.Advances), want)
	}
	if len(deepest.Advances) != want {
		t.Fatalf("%s, %d links below its root, advances %d priorities on this page, want %d",
			deepest.Key, speedCorpusChain-1, len(deepest.Advances), want)
	}
	if readsRelation && !deepest.Advances[0].Inherited {
		t.Fatalf("%s states its relation to %s, want it inherited from the chain's root",
			deepest.Key, deepest.Advances[0].ID)
	}
}

// assertSpeedPriorities holds the three timed requests to what they answer before any of them is
// timed: the page with every open issue counted, the whole body with every priority and the
// relations the corpus holds, and the summary with its two counts and no list.
func assertSpeedPriorities(t *testing.T, corpus speedCorpusShape) {
	t.Helper()
	page := decodeBody[model.IssueSummaryPage](t, speedRequest(t, corpus, speedPagedListingTarget))
	if len(page.Issues) != speedCorpusPage || page.Total != speedCorpusIssues {
		t.Fatalf("the paged listing holds %d issues of %d, want %d of %d",
			len(page.Issues), page.Total, speedCorpusPage, speedCorpusIssues)
	}
	body := decodeBody[model.Priorities](t, speedRequest(t, corpus, "/api/v1/priorities"))
	if len(body.Priorities) != speedCorpusPriorities {
		t.Fatalf("the priorities body draws %d priorities, want the corpus's %d",
			len(body.Priorities), speedCorpusPriorities)
	}
	advanced := 0
	for _, row := range body.Priorities {
		for _, count := range row.Work.ByStatus {
			advanced += count
		}
	}
	if advanced == 0 {
		t.Fatal("the priorities body counts no work at all; a measurement of it measures an empty read")
	}
	summary := decodeBody[model.Priorities](t, speedRequest(t, corpus, "/api/v1/priorities?summary=true"))
	if len(summary.Priorities) != 0 || summary.WorkUnderNoPriority.Count != body.WorkUnderNoPriority.Count {
		t.Fatalf("the summary draws %d priorities and counts %d work under none, want none and the body's %d",
			len(summary.Priorities), summary.WorkUnderNoPriority.Count, body.WorkUnderNoPriority.Count)
	}
}

// speedRequest is one GET against the corpus's server, as a signed-in person makes it, and what
// the three request measurements time: everything the server does to answer, and the
// recorder's write of it.
func speedRequest(t *testing.T, corpus speedCorpusShape, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, bytes.NewReader(nil))
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	corpus.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response
}
