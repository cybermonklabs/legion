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
// They are one box: this pod's Postgres, which carries every other suite running beside it. The
// ratios are the part that survives a busy box, since each pair is measured in the same run,
// interleaved run by run, so a machine that slows moves both. The time of record is the
// server's own — the clock covers one handler.ServeHTTP, or one statement through the scan the
// handler uses, with everything else off it — and the median of five runs is reported, after a
// warm-up run that is not counted: the first run of a statement on a pooled connection prepares
// it, and a measurement of a read is not a measurement of a plan being made.
package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// speedRuns is how many times each operation is run and timed, beside the warm-up run that is
// not counted. Five readings is what the median below is drawn from.
const speedRuns = 5

// The bounds the two tests hold their readings to, from the issue's own checks: the relation
// read at no more than twice the same page without it; the whole priorities body under two
// seconds and under five times the paged listing; the summary under 300 ms.
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

// median is the reading of record: the middle of the sorted runs, which is a run that happened
// rather than an average of a slow one and a fast one.
func (s speedTiming) median() time.Duration {
	sorted := slices.Clone(s.runs)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

func (s speedTiming) String() string {
	readings := make([]string, 0, len(s.runs))
	for _, run := range s.runs {
		readings = append(readings, fmt.Sprintf("%.1f", msOf(run)))
	}
	return fmt.Sprintf("%s: %.1f ms (median of %v ms)", s.name, msOf(s.median()), readings)
}

// speedReadings is every reading the two tests report, taken over the one corpus.
type speedReadings struct {
	advancesRead   speedTiming
	advancesUnread speedTiming
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
// 250 issues read as the server reads it, against the same page by the one statement text that
// leaves the relation out (issueSummaryQuery's readAdvances). Both run through the same scan on
// the same corpus with the same arguments, so what is between them is the relation read alone —
// the inheritance walk over the page's chains, the reading of the tuple log for every issue
// those chains name, and the three columns' join.
//
// The page is the heaviest the corpus holds: the chain as deep as the parent-depth cap, the
// issue that states every priority the store holds, and the issues below both.
func TestAdvancesListingSpeed(t *testing.T) {
	readings := speedCorpusReadings(t)
	t.Log(readings.advancesRead.String())
	t.Log(readings.advancesUnread.String())
	read, unread := readings.advancesRead.median(), readings.advancesUnread.median()
	ratio := float64(read) / float64(unread)
	t.Logf("the relation read costs %.2fx the same page without it, at most %.2fx", ratio, speedAdvancesRatio)
	if ratio > speedAdvancesRatio {
		t.Fatalf("a page of %d issues reads its advances relations in %.1f ms against %.1f ms without them (%.2fx), want at most %.2fx",
			speedCorpusPage, msOf(read), msOf(unread), ratio, speedAdvancesRatio)
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
	paged, full, summary := readings.pagedIssues.median(), readings.prioritiesFull.median(), readings.prioritiesLite.median()
	ratio := float64(full) / float64(paged)
	t.Logf("%s costs %.2fx the paged listing of every open issue, at most %.2fx", speedPriorityFullName, ratio, speedPrioritiesRatio)
	if full > speedPrioritiesBound {
		t.Fatalf("%s took %.1f ms over %d issues and %d priorities, want under %v",
			speedPriorityFullName, msOf(full), speedCorpusIssues, speedCorpusPriorities, speedPrioritiesBound)
	}
	if ratio > speedPrioritiesRatio {
		t.Fatalf("%s took %.1f ms against the paged listing's %.1f ms (%.2fx), want at most %.2fx",
			speedPriorityFullName, msOf(full), msOf(paged), ratio, speedPrioritiesRatio)
	}
	if summary > speedPrioritySummary {
		t.Fatalf("the priorities summary took %.1f ms, want under %v", msOf(summary), speedPrioritySummary)
	}
}

// measureSpeedCorpus builds the corpus, holds the two reads to what they answer over it, and
// takes every reading: one warm-up pass over the five operations, then speedRuns passes,
// interleaved so a box that slows in the middle of the run slows each of them alike.
func measureSpeedCorpus(t *testing.T) speedReadings {
	t.Helper()
	corpus := seedSpeedCorpus(t)
	withRelation := listIssuesPageQuery
	withoutRelation := issueSummaryQuery(false, true, false)
	arguments := speedPageArguments(speedCorpusPage, 0)

	// What each measured read answers, read once before any of them is timed: a reading of a
	// read that answered the wrong thing is worth nothing.
	assertSpeedPage(t, corpus, withRelation, arguments, true)
	assertSpeedPage(t, corpus, withoutRelation, arguments, false)
	assertSpeedPriorities(t, corpus)

	readings := speedReadings{
		advancesRead:   speedTiming{name: "a page of 250 issues with the advances relation read"},
		advancesUnread: speedTiming{name: "the same page with the relation read switched off"},
		pagedIssues:    speedTiming{name: "the paged listing of every open issue"},
		prioritiesFull: speedTiming{name: speedPriorityFullName},
		prioritiesLite: speedTiming{name: "the priorities summary"},
	}
	operations := []struct {
		into *speedTiming
		run  func()
	}{
		{&readings.advancesRead, func() { runSpeedPage(t, corpus, withRelation, arguments) }},
		{&readings.advancesUnread, func() { runSpeedPage(t, corpus, withoutRelation, arguments) }},
		{&readings.pagedIssues, func() { speedRequest(t, corpus, speedPagedListingTarget) }},
		{&readings.prioritiesFull, func() { speedRequest(t, corpus, "/api/v1/priorities") }},
		{&readings.prioritiesLite, func() { speedRequest(t, corpus, "/api/v1/priorities?summary=true") }},
	}
	for _, operation := range operations {
		operation.run()
	}
	for range speedRuns {
		for _, operation := range operations {
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
		"", "", "", (*time.Time)(nil), []string{}, true, []int16{}, false, []string{}, false, limit, offset,
	}
}

// runSpeedPage reads one page through the scan the listing route uses, which is everything of
// the read but the JSON encoding of its answer.
func runSpeedPage(t *testing.T, corpus speedCorpusShape, query string, arguments []any) {
	t.Helper()
	if _, _, err := corpus.server.scanIssuePage(context.Background(), query, arguments); err != nil {
		t.Fatalf("read a page of the listing: %v", err)
	}
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
