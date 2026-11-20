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
// the statement's plan produced or read and threw away, and the pages it touched, as Postgres
// counts them while it runs the statement. A busy box makes the same plan do the same work more
// slowly, and the plan is made from the corpus's own statistics (settleSpeedCorpus), not from
// the box, so the counts do not move with its load. It was held to a time before, which the
// box's load decided both ways: correct code read at 2.2 times the page without it in a
// whole-package run on a loaded box, over the bound, and a listing that walks each row's chain on
// its own, rather than one walk for the page, read at 1.69 to 1.85 times on a 288-CPU box at a
// load of about 300 (2026-11-20) and passed, where its rows stand 4.6 times apart and its pages
// 2.8 times.
//
// The priorities read is still held to times. They are one box: this pod's Postgres, which
// carries every other suite running beside it. The time of record is the server's own — the
// clock covers one handler.ServeHTTP, with everything else off it — and the least of speedRuns
// runs is reported, after a warm-up run that is not counted: the first run of a statement on a
// pooled connection prepares it, and a measurement of a read is not a measurement of a plan being
// made. A busy box only ever adds to a run's time, so each operation's least is the run the box
// reached least, and the runs interleave the operations, so every operation is read over the
// same stretch of the run's load. A median of each against the other's median did not survive
// that: on a loaded 288-CPU machine (2026-11-16) the relation read's runs, when it was timed,
// were 15.5 to 33.3 ms and the page without it 9.6 to 14.7 ms, and the medians stood 2.16 times
// apart where the least of each stood 1.61 times apart.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// speedRuns is how many times each timed operation is run and timed, beside the warm-up run that
// is not counted: how many runs a load that slows one operation and spares another has to reach
// before the least of the first reads it as cost. It was sized for the relation read when that
// was timed: five runs of 61 passes each, at a load of 191 to 266 on a 288-CPU machine
// (2026-11-16), read the relation at 1.47 to 1.64 times the page without it, least against
// least; groups of 5 passes drawn from them put that ratio over 2 in 31 of 100,000, and groups of
// 11 or more in none of 100,000.
const speedRuns = 21

// The bounds the two tests hold their readings to, from the issue's own checks: the relation
// read at no more than twice the same page without it, in rows and in pages; the whole
// priorities body under two seconds and under five times the paged listing; the summary under
// 300 ms.
const (
	speedAdvancesRatio    = 2.0
	speedPrioritiesBound  = 2 * time.Second
	speedPrioritiesRatio  = 5.0
	speedPrioritySummary  = 300 * time.Millisecond
	speedPriorityFullName = "the whole priorities body"
)

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

// speedWork is the work Postgres did to answer one statement, by its own count. EXPLAIN (ANALYZE,
// BUFFERS) runs the statement and reports, for each node of the plan it ran, how many times the
// node ran, the rows it produced each time and the rows its filters read and threw away; and, for
// the whole statement, the shared buffers it touched, each a page of a table or an index. rows is
// the first summed over every node and every run of it, and pages the second; neither is a time.
type speedWork struct {
	name  string
	nodes int
	rows  float64
	pages float64
}

func (w speedWork) String() string {
	return fmt.Sprintf("%s: %.0f rows and %.0f pages over %d plan nodes", w.name, w.rows, w.pages, w.nodes)
}

// speedPlanNode is the part of one node of EXPLAIN's JSON plan that speedWork counts. Postgres
// writes Actual Rows and each Rows Removed count as the average of one run of the node, so each
// is multiplied by Actual Loops; the buffer counts of a node cover the nodes below it, so only
// the plan's top node's are read.
type speedPlanNode struct {
	Loops            float64         `json:"Actual Loops"`
	Rows             float64         `json:"Actual Rows"`
	RemovedByFilter  float64         `json:"Rows Removed by Filter"`
	RemovedByJoin    float64         `json:"Rows Removed by Join Filter"`
	RemovedByRecheck float64         `json:"Rows Removed by Index Recheck"`
	SharedHitBlocks  float64         `json:"Shared Hit Blocks"`
	SharedReadBlocks float64         `json:"Shared Read Blocks"`
	Plans            []speedPlanNode `json:"Plans"`
}

// speedReadings is every reading the two tests report, taken over the one corpus.
type speedReadings struct {
	advancesRead   speedWork
	advancesUnread speedWork
	pagedIssues    speedTiming
	prioritiesFull speedTiming
	prioritiesLite speedTiming
}

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
// and the three columns' join — and it is held to at most twice the work of the page without it,
// in rows and in pages alike (speedWork).
//
// The page is the heaviest the corpus holds: the chain as deep as the parent-depth cap, the
// issue that states every priority the store holds, and the issues below both.
func TestAdvancesListingSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	read, unread := readings.advancesRead, readings.advancesUnread
	t.Log(read.String())
	t.Log(unread.String())
	rows, pages := read.rows/unread.rows, read.pages/unread.pages
	t.Logf("the relation read costs %.2fx the rows and %.2fx the pages of the same page without it, at most %.2fx each",
		rows, pages, speedAdvancesRatio)
	if rows > speedAdvancesRatio || pages > speedAdvancesRatio {
		t.Fatalf("a page of %d issues reads its advances relations in %.0f rows and %.0f pages against %.0f rows and %.0f pages without them (%.2fx and %.2fx), want each at most %.2fx",
			speedCorpusPage, read.rows, read.pages, unread.rows, unread.pages, rows, pages, speedAdvancesRatio)
	}
}

// TestPrioritiesReadSpeed holds the priorities read to what the page that draws it can wait for:
// the whole body under two seconds, and under five times the paged listing of every open issue,
// which is the read it stands beside and the one a reader already waits for; and the summary the
// navigation rail reads under 300 ms, since it is drawn on every page.
func TestPrioritiesReadSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	t.Log(readings.pagedIssues.String())
	t.Log(readings.prioritiesFull.String())
	t.Log(readings.prioritiesLite.String())
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
// work of the two listing texts, and takes every timing: one warm-up pass over the three timed
// operations, then speedRuns passes, interleaved so a box that slows in the middle of the run
// slows each of them alike, every other pass in the reverse order, so no operation always runs on
// what the one before it warmed.
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
		advancesRead:   countSpeedPage(t, corpus, "a page of 250 issues with the advances relation read", withRelation, arguments),
		advancesUnread: countSpeedPage(t, corpus, "the same page with the relation read switched off", withoutRelation, arguments),
		pagedIssues:    speedTiming{name: "the paged listing of every open issue"},
		prioritiesFull: speedTiming{name: speedPriorityFullName},
		prioritiesLite: speedTiming{name: "the priorities summary"},
	}
	operations := []struct {
		into *speedTiming
		run  func()
	}{
		{&readings.pagedIssues, func() { speedRequest(t, corpus, speedPagedListingTarget) }},
		{&readings.prioritiesFull, func() { speedRequest(t, corpus, "/api/v1/priorities") }},
		{&readings.prioritiesLite, func() { speedRequest(t, corpus, "/api/v1/priorities?summary=true") }},
	}
	for _, operation := range operations {
		operation.run()
	}
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

// countSpeedPage counts the work of one page by one statement text (speedWork), on one connection
// of the pool: once to warm the connection, whose first run of a statement can read catalog pages
// a second does not, then twice to be counted. Two counts that differ are a count of something
// beside the statement, so the test stops on them rather than hold either to the bound; and a
// count of nothing is a plan this read could not see into, so it stops on that too.
func countSpeedPage(t *testing.T, corpus speedCorpusShape, name, query string, arguments []any) speedWork {
	t.Helper()
	ctx := context.Background()
	connection, err := corpus.server.deps.Store.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take a connection to count %s: %v", name, err)
	}
	defer connection.Release()
	explain := func() speedWork {
		var raw []byte
		if err := connection.QueryRow(ctx, "explain (analyze, buffers, format json) "+query, arguments...).Scan(&raw); err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		var plans []struct {
			Plan speedPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("read the plan of %s: %d plans, %v", name, len(plans), err)
		}
		work := speedWork{name: name, pages: plans[0].Plan.SharedHitBlocks + plans[0].Plan.SharedReadBlocks}
		var count func(node speedPlanNode)
		count = func(node speedPlanNode) {
			work.nodes++
			work.rows += node.Loops * (node.Rows + node.RemovedByFilter + node.RemovedByJoin + node.RemovedByRecheck)
			for _, below := range node.Plans {
				count(below)
			}
		}
		count(plans[0].Plan)
		return work
	}
	explain()
	first, second := explain(), explain()
	if first != second {
		t.Fatalf("two counts of %s differ, %.0f rows and %.0f pages, then %.0f rows and %.0f pages: neither is the statement's work alone",
			name, first.rows, first.pages, second.rows, second.pages)
	}
	if first.rows == 0 || first.pages == 0 {
		t.Fatalf("%s counted %.0f rows and %.0f pages over %d plan nodes: the plan was not read, so nothing was measured",
			name, first.rows, first.pages, first.nodes)
	}
	return first
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
