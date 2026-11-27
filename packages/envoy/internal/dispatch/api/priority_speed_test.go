// priority_speed_test.go is what the `advances` relation and the priorities read cost at a
// deployment's size: a page of the issue listing with the relation read in place against the
// same page without it, and the whole priorities body and its summary against the paged listing
// they stand beside. The corpus both are measured over is one build (priority_speed_seed_test.go),
// and what each number is held to is written at the test that holds it.
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
// The priorities read is held to counts too (speedRequestWork): the statements the server sends
// to answer it and the round trips they go in, as the traced pool it reads through sees them
// (speedTrace), and the buffer accesses of each of those statements under both plans, counted as
// the relation read's are, against the same counts of the paged listing it stands beside. When it
// was timed instead (the whole body under two seconds and under five times the listing, the
// summary under 300 ms, the least of 21 runs each), how fast the server answered decided it: on a
// box at a load near 1,000 the body read 3.10 times the listing (47.4 ms against 15.3 ms), and a
// server that answered each statement of the body's batch 30 ms late, as a loaded one answers
// them, put it at 8.26 times (135.9 ms against 16.5 ms, 2026-11-22), over the bound, though the
// read did the same work in both. The Go side of the read, which was inside those times, is held
// in what it allocates for each relation the store holds more (speedPriorityGrowth); the encoding
// of its body, its fixed costs and any spill of a statement to temporary files are held by no
// test.
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

// speedCustomPlans is how many runs of a prepared statement on one connection Postgres plans for
// that run's own arguments before it chooses, at every run after, between such a plan and the
// one generic plan it then makes and keeps (choose_custom_plan in PostgreSQL's
// src/backend/utils/cache/plancache.c). pgx prepares every statement the server sends
// (QueryExecModeCacheStatement, the pool's default), so a deployment's connections run each
// statement's first five runs under the first kind and every later run under what the choice
// gives: twelve runs of each listing text on one connection gave five custom plans and seven
// generic ones (pg_prepared_statements, 2026-11-20).
const speedCustomPlans = 5

// speedSettleReads is how many times countSpeedStatement reads one statement, beyond the warm-up,
// to settle its buffer count on the fewest any read made. One read past the warm-up is enough once
// the pages' hint bits are set; a few leaves room for a reader that needs more than one to settle,
// at a cost of a few explains a statement.
const speedSettleReads = 5

// The bounds the two tests hold their readings to, from the issue's own checks: the relation
// read at no more than twice the same page without it, in rows and in buffer accesses (its scan's
// allocations are held closer, speedScanAllocsPerID); the whole priorities body at no more than
// five times the buffer accesses of the paged listing of every open issue, and the summary the
// navigation rail draws on every page at no more than that listing's, which a reader already
// waits for. The issue wrote the last two in time: the body under five times the listing's time
// and under two seconds, the summary under 300 ms. Counted, the summary is held to the listing,
// the read the page beside it already waits for, rather than to a number of its own; and the two
// seconds has no count of its own: the body is a fixed number of statements
// (priorityBodyStatements) whose work is held to the listing's.
const (
	speedAdvancesRatio    = 2.0
	speedPrioritiesRatio  = 5.0
	speedSummaryRatio     = 1.0
	speedPriorityFullName = "the whole priorities body"
)

// The statements each priorities read is written as and the round trips they go in
// (readPriorities): the live priorities (prioritystore.List), then their relations
// (priorityRelationsQuery), then one batch of the statements that rest on them, which for the
// whole body is the counts, the newest event under each priority and the two faults, and for the
// summary the two faults alone. The corpus holds twenty priorities and 400 relations, so a read
// that sent a statement for each priority or each relation would send dozens more.
const (
	priorityBodyStatements    = 6
	prioritySummaryStatements = 4
	priorityReadRoundTrips    = 3
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
// side of reading each (speedScanAllocs), the three requests the priorities read is held by
// (speedRequestWork), and the Go side of the body and of the summary (speedPriorityAllocs).
type speedReadings struct {
	advancesCustom  speedWork
	advancesGeneric speedWork
	advancesUnread  speedWork
	scanRead        speedScan
	scanUnread      speedScan
	pagedIssues     speedRequestWork
	prioritiesFull  speedRequestWork
	prioritiesLite  speedRequestWork
	priorityGrowth  speedPriorityGrowth
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

// The most objects the Go side of a priorities read may allocate for each `advances` relation the
// store holds more (speedPriorityGrowth): the objects one read allocates once the store holds
// speedCorpusMoreRelations relations more, beyond the same read over the corpus's
// speedCorpusRelations, for each relation added. It is the read's own growth, which only the
// relations make: the read scans each relation once (readPriorityRelations) and sends the pair of
// arrays they make with every statement of its batch, so a read that allocated for each relation
// more than it does reads one object a relation more here. Whatever the read allocates the same
// at both sizes (the priorities, the counts' rows, the faults' fifty rows, the batch, the trace)
// is in neither reading's difference: a fixed cost is held by no bound here.
//
// The read's whole count is not held, because it is mostly such fixed costs and moves with the
// build: correct code allocates 10,876 to 10,878 objects for the body over the corpus in the
// plain build and 11,069 to 11,128 under the race detector, whose sync.Pool drops one Put in four
// (64 race readings on amd64, 40 of them the reviewer's, 2026-11-22 and 23). The growth does not
// move with it. Correct code grows by 13.04 to 13.05 objects a relation for the whole body and
// 9.02 for the summary in the plain build (11 readings), and by 12.96 to 13.09 and 8.96 to 9.06 in
// the race build (12 readings); a read that copies each relation's issue key once more grows by
// 14.04 and 10.02 (4 readings), and 13.99 to 14.09 and 9.99 to 10.04 (3 readings), all on amd64
// in the build cluster, 2026-11-23. The bounds stand halfway between. CI's run of this package is
// the race build on arm64, where no reading was taken.
const (
	speedBodyAllocsPerRelation    = 13.5
	speedSummaryAllocsPerRelation = 9.5
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

// TestPrioritiesReadSpeed holds the priorities read to what the page that draws it can afford, in
// the work it costs rather than the time it takes (speedRequestWork): the whole body in the six
// statements and three round trips it is written as, over a store of twenty priorities and 400
// relations, and in no more than five times the buffer accesses of the paged listing of every
// open issue, the read it stands beside and the one a reader already waits for; and the summary
// the navigation rail reads on every page in its four statements and three round trips, in no
// more than that listing's buffer accesses. Hit plus read is every page a statement asked for,
// cached or not, so the count is the read's work and not the cache's state.
//
// The work is counted under both plans a connection runs each statement with
// (speedCustomPlans), each against the listing under the same kind of plan: a connection runs
// every statement of the three requests under the plan made for its arguments five times and
// under the generic plan from then on (twelve runs of each request on one connection gave five
// custom plans and seven generic ones for every statement, pg_prepared_statements, PostgreSQL
// 18.6, 2026-11-22), so the two pairs are what a reader waits for on a connection's first five
// reads and on every read after. Correct code reads 0.39 times the listing's buffer accesses for
// the body and 0.13 times for the summary under the plans made for their arguments (1,193 and 393
// against 3,052), and 2.20 and 0.69 times under the generic plans (20,188 and 6,345 against 9,162).
//
// The rows each plan's nodes produced or threw away are reported beside the buffer accesses and
// held to nothing: the body's statements aggregate the seed's descent through hash joins where
// the listing walks one page through its indexes, so a row of one is not the work of a row of the
// other. Under the generic plans the body produces 10.75 times the listing's rows (438,298 against
// 40,786) where it accesses 2.20 times its buffers, and when the two were timed side by side the
// body took 3.10 times the listing (47.4 ms against 15.3 ms, the least of 21 runs each).
//
// The Go side of each read, the scan of its rows and the building of the body, is held in what
// it allocates for each relation the store holds more (speedBodyAllocsPerRelation,
// speedSummaryAllocsPerRelation): the objects a read allocates over the corpus's relations and
// over speedCorpusMoreRelations more, the faults it answers the same at both. What it allocates
// whatever the store holds is held by no bound.
//
// Buffer accesses are hit and read shared blocks. A statement that spills a hash or a sort to
// temporary files reads and writes local blocks no count here reads; the timed bound took those
// in, and no bound holds them now.
func TestPrioritiesReadSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	paged := readings.pagedIssues
	for _, read := range []speedRequestWork{paged, readings.prioritiesFull, readings.prioritiesLite} {
		t.Log(read.String())
		for _, work := range slices.Concat(read.custom, read.generic) {
			t.Log("  " + work.String())
		}
	}
	for _, held := range []struct {
		read       speedRequestWork
		statements int
		ratio      float64
	}{
		{readings.prioritiesFull, priorityBodyStatements, speedPrioritiesRatio},
		{readings.prioritiesLite, prioritySummaryStatements, speedSummaryRatio},
	} {
		read := held.read
		if len(read.custom) != held.statements || read.roundTrips != priorityReadRoundTrips {
			t.Errorf("%s over %d priorities and %d relations sends %d statements in %d round trips, want %d in %d",
				read.name, speedCorpusPriorities, speedCorpusRelations, len(read.custom), read.roundTrips, held.statements, priorityReadRoundTrips)
		}
		for _, generic := range []bool{false, true} {
			work, reference := read.total(generic), paged.total(generic)
			buffers := work.buffers / reference.buffers
			t.Logf("%s costs %.2fx the buffer accesses of %s, at most %.2fx, and %.2fx its rows",
				work.name, buffers, reference.name, held.ratio, work.rows/reference.rows)
			if buffers > held.ratio {
				t.Errorf("%s reads %.0f buffer accesses against %.0f for %s (%.2fx), want at most %.2fx",
					work.name, work.buffers, reference.buffers, reference.name, buffers, held.ratio)
			}
		}
	}
	growth := readings.priorityGrowth
	for _, held := range []struct {
		name    string
		summary bool
		bound   float64
	}{
		{speedPriorityFullName, false, speedBodyAllocsPerRelation},
		{"the priorities summary", true, speedSummaryAllocsPerRelation},
	} {
		fewer, more := growth.allocs(held.summary)
		perRelation := growth.perRelation(held.summary)
		t.Logf("%s allocates %.0f objects in Go over %d relations and %.0f over %d, %.2f for each relation more, at most %.2f",
			held.name, fewer, speedCorpusRelations, more, speedCorpusRelations+speedCorpusMoreRelations, perRelation, held.bound)
		if perRelation > held.bound {
			t.Errorf("%s allocates %.0f objects in Go over %d relations and %.0f over %d, %.2f for each relation more, want at most %.2f",
				held.name, fewer, speedCorpusRelations, more, speedCorpusRelations+speedCorpusMoreRelations, perRelation, held.bound)
		}
	}
}

// measureSpeedCorpus builds the corpus, holds the reads to what they answer over it, and counts
// the work of each: the two listing texts under their plans and the Go side of each, and the
// three requests the priorities read is held by.
func measureSpeedCorpus(t *testing.T) speedReadings {
	t.Helper()
	corpus := seedSpeedCorpus(t)
	withRelation := listIssuesPageQuery
	withoutRelation := issueSummaryQuery(false, true, false, issueListOrder)
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
		scanRead:        speedScanAllocs(t, corpus, withRelation, arguments),
		scanUnread:      speedScanAllocs(t, corpus, withoutRelation, arguments),
		pagedIssues:     countSpeedRequest(t, corpus, "the paged listing of every open issue", speedPagedListingTarget),
		prioritiesFull:  countSpeedRequest(t, corpus, speedPriorityFullName, "/api/v1/priorities"),
		prioritiesLite:  countSpeedRequest(t, corpus, "the priorities summary", "/api/v1/priorities?summary=true"),
	}
	for _, read := range []speedWork{readings.advancesCustom, readings.advancesGeneric} {
		if read.walks != readings.advancesUnread.walks+1 {
			t.Fatalf("%s holds %d walks and the page without the relation %d, want one more, the relation's own: the plan was not read whole, so nothing was measured",
				read.name, read.walks, readings.advancesUnread.walks)
		}
	}
	// Last, since it writes more relations into the corpus every reading above was taken over.
	readings.priorityGrowth = growSpeedPriorities(t, corpus)
	return readings
}

// speedPriorityAllocs reads the Go side of one priorities read, in the objects one read allocates
// (testing.AllocsPerRun over speedScanRuns reads): readPriorities, which the route calls for the
// whole body and for the summary (summary), with no project named, as the page asks. Its round
// trips are in it as well. The encoding of the body, which the route does after it, is not. An
// allocation count does not move with the box's load; a Go-side change that costs CPU and
// allocates nothing more is not in it.
func speedPriorityAllocs(t *testing.T, corpus speedCorpusShape, summary bool) float64 {
	t.Helper()
	ctx := context.Background()
	var failed error
	allocs := testing.AllocsPerRun(speedScanRuns, func() {
		if _, err := corpus.server.readPriorities(ctx, []string{}, summary); err != nil {
			failed = err
		}
	})
	if failed != nil {
		t.Fatalf("read the priorities: %v", failed)
	}
	if allocs < speedCorpusRelations {
		t.Fatalf("reading the priorities over %d relations allocated %.0f objects: fewer than one a relation, so the read was not counted", speedCorpusRelations, allocs)
	}
	return allocs
}

// speedPriorityGrowth is the Go side of the priorities read at the store's two sizes
// (growSpeedPriorities): the objects one read of the whole body and one of the summary allocate
// over the corpus's speedCorpusRelations relations, and over speedCorpusMoreRelations more.
type speedPriorityGrowth struct {
	body, summary [2]float64
}

// allocs answers one read's two readings, over the corpus's relations and over the more.
func (g speedPriorityGrowth) allocs(summary bool) (float64, float64) {
	if summary {
		return g.summary[0], g.summary[1]
	}
	return g.body[0], g.body[1]
}

// perRelation is the objects one read allocates for each relation the store holds more.
func (g speedPriorityGrowth) perRelation(summary bool) float64 {
	fewer, more := g.allocs(summary)
	return (more - fewer) / speedCorpusMoreRelations
}

// growSpeedPriorities reads the Go side of the priorities read at two sizes of the store
// (speedPriorityGrowth): over the corpus as it was built, then once corpus.moreRelations are
// written. Every added relation is on a leaf issue in `triage` (seedSpeedCorpus), so the second
// read answers the same faults, and each priority's counts in the same rows: the test stops on
// faults that differ, and on a relation read that does not answer every relation written, since
// then what is between the two readings is not the relations alone.
func growSpeedPriorities(t *testing.T, corpus speedCorpusShape) speedPriorityGrowth {
	t.Helper()
	ctx := context.Background()
	faults := func() string {
		body, err := corpus.server.readPriorities(ctx, []string{}, false)
		if err != nil {
			t.Fatalf("read the priorities: %v", err)
		}
		parts := []string{fmt.Sprintf("%d priorities, %d unplaced, %d parked:", len(body.Priorities),
			body.WorkUnderNoPriority.Count, body.PriorityWorkParked.Count)}
		for _, issue := range body.WorkUnderNoPriority.Issues {
			parts = append(parts, issue.Key)
		}
		for _, issue := range body.PriorityWorkParked.Issues {
			parts = append(parts, fmt.Sprintf("%s(%d)", issue.Key, len(issue.Advances)))
		}
		return strings.Join(parts, " ")
	}
	growth := speedPriorityGrowth{}
	before := faults()
	growth.body[0], growth.summary[0] = speedPriorityAllocs(t, corpus, false), speedPriorityAllocs(t, corpus, true)

	copySeededRows(t, corpus.database, "priority_tuples", seededTupleColumns, corpus.moreRelations)
	keys, _, err := corpus.server.readPriorityRelations(ctx, corpus.priorities)
	if err != nil {
		t.Fatalf("read the advances relations: %v", err)
	}
	if len(keys) != speedCorpusRelations+speedCorpusMoreRelations {
		t.Fatalf("the store answers %d advances relations once %d more are written, want %d",
			len(keys), speedCorpusMoreRelations, speedCorpusRelations+speedCorpusMoreRelations)
	}
	if after := faults(); after != before {
		t.Fatalf("the faults differ once %d relations more are written, so the readings are not of the relations alone:\nbefore %s\nafter  %s",
			speedCorpusMoreRelations, before, after)
	}
	growth.body[1], growth.summary[1] = speedPriorityAllocs(t, corpus, false), speedPriorityAllocs(t, corpus, true)
	return growth
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
		(*time.Time)(nil), (*time.Time)(nil), []string{}, false, false, false, false, "", "", limit, offset,
	}
}

// countSpeedPage counts the work of one page by one statement text under one of its two plans
// (countSpeedStatement), and stops on a count that did not answer the page's rows, which is a
// plan this read could not see into.
func countSpeedPage(t *testing.T, corpus speedCorpusShape, name, query string, arguments []any, generic bool) speedWork {
	t.Helper()
	connection, err := corpus.database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("take a connection to count %s: %v", name, err)
	}
	defer connection.Release()
	work := countSpeedStatement(t, connection.Conn(), name, query, arguments, generic)
	if work.answered != speedCorpusPage {
		t.Fatalf("%s answered %.0f rows over %d plan nodes, want the page's %d rows answered: the plan was not read, so nothing was measured",
			name, work.answered, work.nodes, speedCorpusPage)
	}
	return work
}

// countSpeedStatement counts the work of one statement under one of its two plans (speedWork), on
// connection, a connection of the untraced pool: once to warm the connection, whose first run of a
// statement can read catalog pages a later one does not, then speedSettleReads times to be
// counted. The generic plan is the one plan_cache_mode = force_generic_plan makes a prepared
// statement run from its first execution, the plan a connection's later runs get
// (speedCustomPlans), as analyzeGenericListingPlan reads it.
//
// The reads must agree on the plan's shape and its rows — the nodes, the walks, the rows the plan
// produces and reads and throws away, and the rows it answers — which is the statement's work and
// what the machine cannot move; two reads that answer different rows are a count of a different
// plan, and the test stops. The buffer accesses are the fewest any read made (speedWork.buffers):
// the first read of a heap page whose tuples a committed transaction wrote sets the page's hint
// bits and, where the visibility map does not yet mark the page all-visible, fetches the heap
// through an index-only scan, so a read before the pages settle accesses more buffers than the
// read's own work (3,135 against 3,069 for the body's unplaced fault on a loaded arm64 CI runner,
// 2026-11-23; a quiet amd64 node reads 3,069 every time of 40). The fewest is the settled floor,
// the pages the plan touches once the hint bits are set, which is one number on either
// architecture; the extra reads only ever add to it. A count of no rows or no buffer accesses is a
// plan this read could not see into, so the test stops on that too.
func countSpeedStatement(t *testing.T, connection *pgx.Conn, name, query string, arguments []any, generic bool) speedWork {
	t.Helper()
	ctx := context.Background()
	statement, bound := "explain (analyze, buffers, format json) "+query, arguments
	if generic {
		for _, setup := range []string{"set plan_cache_mode = force_generic_plan", "prepare speed_statement as " + query} {
			if _, err := connection.Exec(ctx, setup); err != nil {
				t.Fatalf("prepare %s: %v", name, err)
			}
		}
		defer func() {
			for _, teardown := range []string{"deallocate speed_statement", "reset plan_cache_mode"} {
				if _, err := connection.Exec(ctx, teardown); err != nil {
					t.Errorf("%s after counting %s: %v", teardown, name, err)
				}
			}
		}()
		statement, bound = "explain (analyze, buffers, format json) execute speed_statement"+speedLiterals(t, arguments), nil
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
	warm := explain()
	settled := warm
	for range speedSettleReads {
		read := explain()
		if read.rows != settled.rows || read.answered != settled.answered || read.nodes != settled.nodes || read.walks != settled.walks {
			t.Fatalf("two counts of %s answer different work, %.0f rows answering %.0f over %d nodes and %d walks and then %.0f rows answering %.0f over %d nodes and %d walks: one is a count of a different plan",
				name, settled.rows, settled.answered, settled.nodes, settled.walks, read.rows, read.answered, read.nodes, read.walks)
		}
		if read.buffers < settled.buffers {
			settled.buffers = read.buffers
		}
	}
	if settled.rows == 0 || settled.buffers == 0 {
		t.Fatalf("%s counted %.0f rows and %.0f buffer accesses over %d plan nodes: the plan was not read, so nothing was measured",
			name, settled.rows, settled.buffers, settled.nodes)
	}
	return settled
}

// speedRequestWork is the work of one request: the statements the server sent to answer it, in
// the order it sent them, and the round trips they went in, as the corpus's traced pool saw them
// (speedTrace); and the work of each of those statements under each of the two plans a
// connection runs it with (speedCustomPlans), one entry a statement in custom and in generic.
type speedRequestWork struct {
	name       string
	roundTrips int
	custom     []speedWork
	generic    []speedWork
}

func (w speedRequestWork) String() string {
	custom, generic := w.total(false), w.total(true)
	return fmt.Sprintf("%s: %d statements in %d round trips, %.0f rows and %.0f buffer accesses under the plans made for their arguments, %.0f rows and %.0f buffer accesses under the generic plans",
		w.name, len(w.custom), w.roundTrips, custom.rows, custom.buffers, generic.rows, generic.buffers)
}

// total is the work of every statement the request sent, summed, under the generic plans where
// generic is set and under the plans made for each statement's arguments where it is not.
func (w speedRequestWork) total(generic bool) speedWork {
	works, plans := w.custom, "the plans made for their arguments"
	if generic {
		works, plans = w.generic, "the generic plans"
	}
	total := speedWork{name: w.name + " under " + plans}
	for _, work := range works {
		total.nodes += work.nodes
		total.walks += work.walks
		total.answered += work.answered
		total.rows += work.rows
		total.buffers += work.buffers
	}
	return total
}

// countSpeedRequest makes one GET of target on a context the corpus's trace counts (countedRead)
// and counts what the server sent to answer it: the statements and the round trips, and the work
// of each statement under both plans (countSpeedStatement), run again with the arguments the
// request sent it, on a connection of the untraced pool so the counting sends nothing the trace
// sees.
func countSpeedRequest(t *testing.T, corpus speedCorpusShape, name, target string) speedRequestWork {
	t.Helper()
	ctx := context.Background()
	corpus.trace.take()
	speedRequest(t, context.WithValue(ctx, countedRead{}, true), corpus, target)
	sent, roundTrips := corpus.trace.take()
	if len(sent) == 0 {
		t.Fatalf("%s sent no statement the trace saw, so nothing was counted", name)
	}
	connection, err := corpus.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection to count %s: %v", name, err)
	}
	defer connection.Release()
	work := speedRequestWork{name: name, roundTrips: roundTrips}
	for index, statement := range sent {
		described := fmt.Sprintf("statement %d of %s (%.60s…)", index+1, name, strings.Join(strings.Fields(statement.sql), " "))
		work.custom = append(work.custom, countSpeedStatement(t, connection.Conn(),
			described+" under the plan made for its arguments", statement.sql, statement.arguments, false))
		work.generic = append(work.generic, countSpeedStatement(t, connection.Conn(),
			described+" under the generic plan", statement.sql, statement.arguments, true))
	}
	return work
}

// speedTrace is roundTrips (issue_read_batch_test.go), which counts the round trips a counted
// request sends, keeping as well every statement of that request, a batch's one by one, with the
// arguments it was sent with, so countSpeedRequest can count the work of each.
type speedTrace struct {
	roundTrips
	sent []speedStatement
}

// speedStatement is one statement a counted request sent: its text and its arguments.
type speedStatement struct {
	sql       string
	arguments []any
}

// take answers the statements and the number of round trips counted since the last take.
func (s *speedTrace) take() ([]speedStatement, int) {
	roundTrips := len(s.roundTrips.take())
	s.mu.Lock()
	defer s.mu.Unlock()
	sent := s.sent
	s.sent = nil
	return sent, roundTrips
}

func (s *speedTrace) keep(ctx context.Context, sql string, arguments []any) {
	if ctx.Value(countedRead{}) == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, speedStatement{sql: sql, arguments: arguments})
}

func (s *speedTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.keep(ctx, data.SQL, data.Args)
	return s.roundTrips.TraceQueryStart(ctx, connection, data)
}

func (s *speedTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	s.keep(ctx, data.SQL, data.Args)
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

// speedLiterals writes a statement's arguments as the list an EXECUTE names after the statement,
// since an EXECUTE under EXPLAIN takes no bound parameter: a string, a flag, a number, a list of
// strings, an empty list of numbers and a null time are every value the arguments of the reads
// counted here hold, and any other stops the test rather than be written wrong. A statement that
// takes no argument is named with no list.
func speedLiterals(t *testing.T, arguments []any) string {
	t.Helper()
	if len(arguments) == 0 {
		return ""
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	literals := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		switch value := argument.(type) {
		case string:
			literals = append(literals, quote(value))
		case bool:
			literals = append(literals, strconv.FormatBool(value))
		case int:
			literals = append(literals, strconv.Itoa(value))
		case []string:
			if len(value) == 0 {
				literals = append(literals, "'{}'")
				continue
			}
			elements := make([]string, 0, len(value))
			for _, element := range value {
				elements = append(elements, quote(element))
			}
			literals = append(literals, "array["+strings.Join(elements, ", ")+"]")
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
	return "(" + strings.Join(literals, ", ") + ")"
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

// assertSpeedPriorities holds the three counted requests to what they answer before any of them
// is counted: the page with every open issue counted, the whole body with every priority and the
// relations the corpus holds, and the summary with its two counts and no list.
func assertSpeedPriorities(t *testing.T, corpus speedCorpusShape) {
	t.Helper()
	ctx := context.Background()
	page := decodeBody[model.IssueSummaryPage](t, speedRequest(t, ctx, corpus, speedPagedListingTarget))
	if len(page.Issues) != speedCorpusPage || page.Total != speedCorpusIssues {
		t.Fatalf("the paged listing holds %d issues of %d, want %d of %d",
			len(page.Issues), page.Total, speedCorpusPage, speedCorpusIssues)
	}
	body := decodeBody[model.Priorities](t, speedRequest(t, ctx, corpus, "/api/v1/priorities"))
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
	summary := decodeBody[model.Priorities](t, speedRequest(t, ctx, corpus, "/api/v1/priorities?summary=true"))
	if len(summary.Priorities) != 0 || summary.WorkUnderNoPriority.Count != body.WorkUnderNoPriority.Count {
		t.Fatalf("the summary draws %d priorities and counts %d work under none, want none and the body's %d",
			len(summary.Priorities), summary.WorkUnderNoPriority.Count, body.WorkUnderNoPriority.Count)
	}
}

// speedRequest is one GET against the corpus's server on ctx, as a signed-in person makes it:
// everything the server does to answer, and the recorder's write of it.
func speedRequest(t *testing.T, ctx context.Context, corpus speedCorpusShape, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, target, bytes.NewReader(nil))
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	corpus.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return response
}
