// priority_speed_test.go is what the `advances` relation and the priorities read cost at a
// deployment's size: a page of the issue listing with the relation read in place against the
// same page without it, and the whole priorities body and its summary against the paged listing
// they stand beside. The corpus every reading is taken over is one build
// (priority_speed_seed_test.go), and what each reading is held to is written at the test that
// holds it.
//
// # Work, and time
//
// The rows every run makes hold each read to the work it costs, which the code reports and no
// clock decides: the statements it sends and the round trips they go in, read off a traced pool,
// and the shared buffers those statements ask Postgres for, read from EXPLAIN (ANALYZE, BUFFERS)
// of each one as it was sent. Hit plus read is every page a statement asked for, cached or not,
// so the count is the read's work rather than the cache's state or the machine's load
// (indexingBuffers counts a save the same way). The issue's checks are held in those terms: the
// relation read at most twice the work of the same page without it, the whole priorities body at
// most five times the work of the paged listing, and the summary the navigation rail draws on
// every page at most the work of that listing, which a reader already waits for.
//
// Those checks were first written in time - the relation read within twice the page's time, the
// body under two seconds and under five times the listing's, the summary under 300 ms - and the
// times are a service level a person reads, so they are the measurement lane's
// (benchtime.TimingEnv): TestAdvancesListingSpeed and TestPrioritiesReadSpeed run only where
// DISPATCH_BENCH_TIMING is set, on a quiet machine, and CI never runs them, since a reading of a
// shared runner's clock is the runner's load as much as the read's work (INFRA-3149).
//
// # One corpus, four tests
//
// A database storetest.Open hands back lives as long as the test it was opened for, and these
// are four tests, so the later ones could not read the first's. The corpus is therefore built
// once, by whichever of them runs first, and every reading is taken over it in that run; the
// others read their own out of what that run left here. The times are taken in that run only
// when the timing lane is open, which it is for the whole process or not at all.
//
// # What the times are, and what they are not
//
// They are one box: the machine's Postgres, which carries whatever else runs beside it. The time
// of record is the server's own — the clock covers one handler.ServeHTTP, or one statement through
// the scan the handler uses, with everything else off it — and the least of speedRuns runs is
// reported, after a warm-up run that is not counted: the first run of a statement on a pooled
// connection prepares it, and a measurement of a read is not a measurement of a plan being made.
// A busy box only ever adds to a run's time, so each operation's least is the run the box reached
// least, and the runs interleave the operations, so every operation is read over the same stretch
// of the run's load. A median of each against the other's median did not survive that: on a
// loaded 288-CPU machine (2026-11-16) the relation read's runs were 15.5 to 33.3 ms and the page
// without it 9.6 to 14.7 ms, and the medians stood 2.16 times apart where the least of each stood
// 1.61 times apart.
package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// speedRuns is how many times each operation is run and timed, beside the warm-up run that is
// not counted: how many runs a load that slows one operation and spares another has to reach
// before the least of the first reads it as cost. Five runs of 61 passes each, at a load of 191
// to 266 on a 288-CPU machine (2026-11-16), read the relation at 1.47 to 1.64 times the page
// without it, least against least; groups of 5 passes drawn from them put that ratio over 2 in 31
// of 100,000, and groups of 11 or more in none of 100,000.
const speedRuns = 21

// The bounds the readings are held to, from the issue's own checks: the relation read at no more
// than twice the same page without it; the whole priorities body under two seconds and under five
// times the paged listing; the summary under 300 ms. The two ratios hold the work of the reads in
// every run and their times in the timing lane; the summary's work is held to the paged listing's
// (speedSummaryRatio), and the two absolute times are the timing lane's alone.
const (
	speedAdvancesRatio    = 2.0
	speedPrioritiesBound  = 2 * time.Second
	speedPrioritiesRatio  = 5.0
	speedSummaryRatio     = 1.0
	speedPrioritySummary  = 300 * time.Millisecond
	speedPriorityFullName = "the whole priorities body"
)

// The statements and round trips each priorities read is written as (readPriorities): the live
// priorities, their relations, then the counts, the newest event and the two faults in one batch
// for the whole body, and the two faults alone in it for the summary. A read that grew a
// statement for each priority, or for each relation, would show here at once, which is the cost
// a corpus of twenty priorities and 400 relations is built to show.
const (
	priorityBodyStatements    = 6
	prioritySummaryStatements = 4
	priorityReadRoundTrips    = 3
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

// speedReadings is every time the two timing-lane tests report, taken over the one corpus.
type speedReadings struct {
	advancesRead   speedTiming
	advancesUnread speedTiming
	pagedIssues    speedTiming
	prioritiesFull speedTiming
	prioritiesLite speedTiming
}

// speedWork is what one read cost the database: the statements it sent and the round trips they
// went in, read off the traced pool (speedTrace), and the shared buffers EXPLAIN (ANALYZE,
// BUFFERS) counts for those statements as they were sent, beside the plan of each statement
// that count was read from (speedPlan).
type speedWork struct {
	name       string
	statements int
	roundTrips int
	buffers    uint64
	plans      []speedPlanNode
}

func (w speedWork) String() string {
	return fmt.Sprintf("%s: %d statements in %d round trips, %d shared buffers", w.name, w.statements, w.roundTrips, w.buffers)
}

// speedWorks is the work of each of the five reads, taken over the one corpus.
type speedWorks struct {
	advancesRead   speedWork
	advancesUnread speedWork
	pagedIssues    speedWork
	prioritiesFull speedWork
	prioritiesLite speedWork
}

// speedCorpusRun is everything the four tests report, taken over the one corpus in the run that
// built it: the work of each read always, the rows the page's own chains give the relation
// read's walk (speedCorpusShape.walkRows), and the times only when the timing lane was open.
type speedCorpusRun struct {
	work     speedWorks
	pageWalk int
	timings  *speedReadings
}

// measuredSpeedCorpus is what the first of the four tests to run left behind. It is written and
// read by tests of one package, which the testing package runs one after another; none of them
// calls t.Parallel, so nothing reads it while it is being written.
var measuredSpeedCorpus *speedCorpusRun

// speedCorpusReadings answers the readings, taking them over a corpus of its own the first time
// it is asked. A build that fails leaves nothing behind, so the next test builds again rather
// than reporting readings nobody took.
func speedCorpusReadings(t *testing.T) speedCorpusRun {
	t.Helper()
	if measuredSpeedCorpus == nil {
		taken := measureSpeedCorpus(t)
		measuredSpeedCorpus = &taken
	}
	return *measuredSpeedCorpus
}

// TestAdvancesListingWork holds the `advances` relation's own cost on the listing: one page of
// 250 issues read as the server reads it, against the same page by the one statement text that
// leaves the relation out (issueSummaryQuery's readAdvances). Both run through the same scan on
// the same corpus with the same arguments, so what is between them is the relation read alone —
// the inheritance walk over the page's chains, the reading of the tuple log for every issue
// those chains name, and the three columns' join. It sends no statement and no round trip of
// its own; its walk climbs the page's own chains and nothing else, one row for each issue of the
// page and each issue above it, however many issues the corpus holds; and it costs no more than
// twice the shared buffers of the page without it.
//
// The walk is counted as well as the buffers because the buffers cannot see it at this size:
// Postgres climbs it by hashing the issues once, so a walk that climbed every issue's chain
// rather than the page's reads the same pages and does many times the work on them.
//
// The page is the heaviest the corpus holds: the chain as deep as the parent-depth cap, the
// issue that states every priority the store holds, and the issues below both.
func TestAdvancesListingWork(t *testing.T) {
	readings := speedCorpusReadings(t)
	work := readings.work
	t.Log(work.advancesRead.String())
	t.Log(work.advancesUnread.String())
	read, unread := work.advancesRead, work.advancesUnread
	if read.statements != unread.statements || read.roundTrips != unread.roundTrips {
		t.Errorf("a page of %d issues sends %d statements in %d round trips with its advances relations read and %d in %d without them, want the same: the relation is read inside the page's own statement",
			speedCorpusPage, read.statements, read.roundTrips, unread.statements, unread.roundTrips)
	}
	walked := speedWalkRows(t, read.plans)
	t.Logf("the relation read's walk climbed %d rows; the page's own chains are %d", walked, readings.pageWalk)
	if walked != readings.pageWalk {
		t.Errorf("the relation read's walk (issueAdvancesTerms' advances_chain) climbed %d rows for a page of %d issues, want the %d of the page's issues and the issues above them",
			walked, speedCorpusPage, readings.pageWalk)
	}
	ratio := float64(read.buffers) / float64(unread.buffers)
	t.Logf("the relation read costs %.2fx the shared buffers of the same page without it, at most %.2fx", ratio, speedAdvancesRatio)
	if ratio > speedAdvancesRatio {
		t.Errorf("a page of %d issues reads %d shared buffers with its advances relations and %d without them (%.2fx), want at most %.2fx",
			speedCorpusPage, read.buffers, unread.buffers, ratio, speedAdvancesRatio)
	}
}

// TestPrioritiesReadWork holds the priorities read to what the page that draws it can afford:
// the whole body in the six statements and three round trips it is written as, whatever the
// number of priorities and relations, and at most five times the shared buffers of the paged
// listing of every open issue, which is the read it stands beside and the one a reader already
// waits for; and the summary the navigation rail reads on every page in its four statements and
// three round trips, at no more than that listing's buffers.
func TestPrioritiesReadWork(t *testing.T) {
	work := speedCorpusReadings(t).work
	t.Log(work.pagedIssues.String())
	t.Log(work.prioritiesFull.String())
	t.Log(work.prioritiesLite.String())
	paged, full, summary := work.pagedIssues, work.prioritiesFull, work.prioritiesLite
	if full.statements != priorityBodyStatements || full.roundTrips != priorityReadRoundTrips {
		t.Errorf("%s over %d priorities and %d relations sends %d statements in %d round trips, want %d in %d",
			speedPriorityFullName, speedCorpusPriorities, speedCorpusRelations,
			full.statements, full.roundTrips, priorityBodyStatements, priorityReadRoundTrips)
	}
	if summary.statements != prioritySummaryStatements || summary.roundTrips != priorityReadRoundTrips {
		t.Errorf("the priorities summary sends %d statements in %d round trips, want %d in %d",
			summary.statements, summary.roundTrips, prioritySummaryStatements, priorityReadRoundTrips)
	}
	ratio := float64(full.buffers) / float64(paged.buffers)
	t.Logf("%s costs %.2fx the shared buffers of the paged listing of every open issue, at most %.2fx", speedPriorityFullName, ratio, speedPrioritiesRatio)
	if ratio > speedPrioritiesRatio {
		t.Errorf("%s reads %d shared buffers against the paged listing's %d (%.2fx), want at most %.2fx",
			speedPriorityFullName, full.buffers, paged.buffers, ratio, speedPrioritiesRatio)
	}
	summaryRatio := float64(summary.buffers) / float64(paged.buffers)
	t.Logf("the priorities summary costs %.2fx the shared buffers of the paged listing, at most %.2fx", summaryRatio, speedSummaryRatio)
	if summaryRatio > speedSummaryRatio {
		t.Errorf("the priorities summary reads %d shared buffers against the paged listing's %d (%.2fx), want at most %.2fx",
			summary.buffers, paged.buffers, summaryRatio, speedSummaryRatio)
	}
}

// TestAdvancesListingSpeed is TestAdvancesListingWork's check in time, the measurement lane's
// (benchtime.TimingEnv): the page with its advances relations read in at most twice the time of
// the same page without them.
func TestAdvancesListingSpeed(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	readings := speedTimings(t)
	t.Log(readings.advancesRead.String())
	t.Log(readings.advancesUnread.String())
	read, unread := readings.advancesRead.least(), readings.advancesUnread.least()
	ratio := float64(read) / float64(unread)
	t.Logf("the relation read costs %.2fx the same page without it, at most %.2fx", ratio, speedAdvancesRatio)
	if ratio > speedAdvancesRatio {
		t.Fatalf("a page of %d issues reads its advances relations in %.1f ms against %.1f ms without them, the least of %d runs each (%.2fx), want at most %.2fx",
			speedCorpusPage, msOf(read), msOf(unread), speedRuns, ratio, speedAdvancesRatio)
	}
}

// TestPrioritiesReadSpeed is TestPrioritiesReadWork's check in time, the measurement lane's
// (benchtime.TimingEnv): the whole body under two seconds, and under five times the paged
// listing of every open issue; and the summary the navigation rail reads under 300 ms, since it
// is drawn on every page.
func TestPrioritiesReadSpeed(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	readings := speedTimings(t)
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

// speedTimings is the times the run that built the corpus took. A timing-lane test asks only
// once the lane is open (benchtime.Timing), and the lane is open for the whole process, so the
// run that built the corpus took them.
func speedTimings(t *testing.T) speedReadings {
	t.Helper()
	readings := speedCorpusReadings(t).timings
	if readings == nil {
		t.Fatal("the run that built the corpus took no times, though the timing lane is open")
	}
	return *readings
}

// measureSpeedCorpus builds the corpus, holds the reads to what they answer over it, and takes
// every reading: each read's work, and, when the timing lane is open, one warm-up pass over the
// five operations, then speedRuns passes, interleaved so a box that slows in the middle of the
// run slows each of them alike, every other pass in the reverse order, so no operation always runs
// on what the one before it warmed.
func measureSpeedCorpus(t *testing.T) speedCorpusRun {
	t.Helper()
	corpus := seedSpeedCorpus(t)
	withRelation := listIssuesPageQuery
	withoutRelation := issueSummaryQuery(false, true, false)
	arguments := speedPageArguments(speedCorpusPage, 0)

	// What each read answers, read once before any of them is counted or timed: a reading of a
	// read that answered the wrong thing is worth nothing.
	page := assertSpeedPage(t, corpus, withRelation, arguments, true)
	assertSpeedPage(t, corpus, withoutRelation, arguments, false)
	assertSpeedPriorities(t, corpus)
	settleSpeedCorpusStatistics(t, corpus)

	run := speedCorpusRun{pageWalk: corpus.walkRows(page)}
	timings := speedReadings{
		advancesRead:   speedTiming{name: "a page of 250 issues with the advances relation read"},
		advancesUnread: speedTiming{name: "the same page with the relation read switched off"},
		pagedIssues:    speedTiming{name: "the paged listing of every open issue"},
		prioritiesFull: speedTiming{name: speedPriorityFullName},
		prioritiesLite: speedTiming{name: "the priorities summary"},
	}
	operations := []struct {
		work   *speedWork
		timing *speedTiming
		run    func(context.Context)
	}{
		{&run.work.advancesRead, &timings.advancesRead, func(ctx context.Context) { runSpeedPage(t, ctx, corpus, withRelation, arguments) }},
		{&run.work.advancesUnread, &timings.advancesUnread, func(ctx context.Context) { runSpeedPage(t, ctx, corpus, withoutRelation, arguments) }},
		{&run.work.pagedIssues, &timings.pagedIssues, func(ctx context.Context) { speedRequestOn(t, ctx, corpus, speedPagedListingTarget) }},
		{&run.work.prioritiesFull, &timings.prioritiesFull, func(ctx context.Context) { speedRequestOn(t, ctx, corpus, "/api/v1/priorities") }},
		{&run.work.prioritiesLite, &timings.prioritiesLite, func(ctx context.Context) { speedRequestOn(t, ctx, corpus, "/api/v1/priorities?summary=true") }},
	}

	connection, err := corpus.database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	for _, operation := range operations {
		*operation.work = countSpeedWork(t, connection.Conn(), corpus.trace, operation.timing.name, operation.run)
	}

	if os.Getenv(benchtime.TimingEnv) == "" {
		return run
	}
	for _, operation := range operations {
		operation.run(context.Background())
	}
	for pass := range speedRuns {
		order := slices.Clone(operations)
		if pass%2 == 1 {
			slices.Reverse(order)
		}
		for _, operation := range order {
			started := time.Now()
			operation.run(context.Background())
			operation.timing.runs = append(operation.timing.runs, time.Since(started))
		}
	}
	run.timings = &timings
	return run
}

// settleSpeedCorpusStatistics gives the corpus the statistics and the visibility a production
// table has, once every write the seed and the first reads make is in. The plans the counts are
// read off follow both, and autovacuum would otherwise take them at a moment of its own - some
// seconds after the seed, sooner or later as the machine is loaded - so a count would be of
// whichever plan the machine's load had picked. With nothing left for autovacuum to do, every run
// counts the one plan.
func settleSpeedCorpusStatistics(t *testing.T, corpus speedCorpusShape) {
	t.Helper()
	if _, err := corpus.database.Pool.Exec(context.Background(), "vacuum analyze"); err != nil {
		t.Fatalf("analyze the corpus: %v", err)
	}
}

// countSpeedWork runs one read on a context speedTrace counts (countedRead), and answers what it
// sent and the shared buffers its statements cost on connection, a connection of the untraced
// pool that every count of the run shares.
func countSpeedWork(t *testing.T, connection *pgx.Conn, trace *speedTrace, name string, read func(context.Context)) speedWork {
	t.Helper()
	trace.take()
	read(context.WithValue(context.Background(), countedRead{}, true))
	statements, roundTrips := trace.take()
	work := speedWork{name: name, statements: len(statements), roundTrips: roundTrips}
	for _, statement := range statements {
		plan := speedPlan(t, connection, statement)
		work.buffers += plan.SharedHit + plan.SharedRead
		work.plans = append(work.plans, plan)
	}
	return work
}

// speedPlanNode is what the counts read of one node of an EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
// plan: the common table expression it computes, if any, the rows it answered over all its
// loops, and the shared buffers it and every node below it asked for.
type speedPlanNode struct {
	SubplanName string          `json:"Subplan Name"`
	ActualRows  float64         `json:"Actual Rows"`
	ActualLoops float64         `json:"Actual Loops"`
	SharedHit   uint64          `json:"Shared Hit Blocks"`
	SharedRead  uint64          `json:"Shared Read Blocks"`
	Plans       []speedPlanNode `json:"Plans"`
}

// speedPlan is the top node of statement's plan under EXPLAIN (ANALYZE, BUFFERS) as the read
// sent it, its text and its arguments. Its shared buffers, hit plus read, are every page the
// statement asked for, whether the cache held it or not. It is run twice on the one connection
// and the second run is the one answered: the first fills the connection's catalog caches, and a
// miss there during execution would count pages of the catalog rather than of the read. Each run
// plans afresh with the read's own arguments (pgx.QueryExecModeDescribeExec), so neither is the
// generic plan a prepared statement turns to from its sixth execution, and every reading of a
// statement is of the one plan.
func speedPlan(t *testing.T, connection *pgx.Conn, statement speedStatement) speedPlanNode {
	t.Helper()
	arguments := append([]any{pgx.QueryExecModeDescribeExec}, statement.args...)
	var plan speedPlanNode
	for range 2 {
		var explained []struct {
			Plan speedPlanNode `json:"Plan"`
		}
		if err := connection.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement.sql, arguments...).Scan(&explained); err != nil {
			t.Fatalf("explain %.200s: %v", statement.sql, err)
		}
		if len(explained) != 1 {
			t.Fatalf("explain %.200s gave %d plans, want one", statement.sql, len(explained))
		}
		plan = explained[0].Plan
	}
	return plan
}

// speedWalkRows is the rows the relation read's inheritance walk answered in plans, the plans of
// a page read: the common table expression issueAdvancesTerms names advances_chain, one row for
// each subject and each issue above it. A recursive one is never inlined, so a plan of a text that
// reads the relation holds it exactly once.
func speedWalkRows(t *testing.T, plans []speedPlanNode) int {
	t.Helper()
	var walks []speedPlanNode
	var visit func(node speedPlanNode)
	visit = func(node speedPlanNode) {
		if node.SubplanName == "CTE advances_chain" {
			walks = append(walks, node)
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, plan := range plans {
		visit(plan)
	}
	if len(walks) != 1 {
		t.Fatalf("the page's plans hold %d advances_chain walks, want one", len(walks))
	}
	return int(walks[0].ActualRows * walks[0].ActualLoops)
}

// speedStatement is one statement a counted read sent, as it went: its text and its arguments,
// which is what speedPlan explains.
type speedStatement struct {
	sql  string
	args []any
}

// speedTrace records what the corpus's traced pool sends for a counted read (countedRead): every
// statement, a batch's one by one, and the round trips they went in - one for each statement
// sent on its own and one for each batch, whatever that batch holds (storetest.Traced).
type speedTrace struct {
	mu         sync.Mutex
	statements []speedStatement
	roundTrips int
}

// take returns what was recorded since the last take.
func (s *speedTrace) take() ([]speedStatement, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	statements, roundTrips := s.statements, s.roundTrips
	s.statements, s.roundTrips = nil, 0
	return statements, roundTrips
}

// record adds what a counted read sent: a round trip when roundTrip is set, and the statement
// when there is one. A statement sent on its own is both; a batch is one round trip, and each of
// its statements is reported apart.
func (s *speedTrace) record(ctx context.Context, roundTrip bool, statement *speedStatement) {
	if ctx.Value(countedRead{}) == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if roundTrip {
		s.roundTrips++
	}
	if statement != nil {
		s.statements = append(s.statements, *statement)
	}
}

func (s *speedTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.record(ctx, true, &speedStatement{sql: data.SQL, args: data.Args})
	return ctx
}

func (s *speedTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *speedTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	s.record(ctx, true, nil)
	return ctx
}

func (s *speedTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	s.record(ctx, false, &speedStatement{sql: data.SQL, args: data.Args})
}

func (s *speedTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// speedPagedListingTarget is the paged read of every open issue: one page of the most issues a
// page holds, whose total is every open issue the corpus carries.
var speedPagedListingTarget = fmt.Sprintf("/api/v1/issues?open=true&limit=%d", speedCorpusPage)

// speedPageArguments are the arguments one page of the open listing takes, in the order
// listIssues builds them: no filter but `open`, and the page's limit and offset last. Both
// statement texts take the same list, which is what lets one reading stand against the other
// (issueSummaryQuery).
func speedPageArguments(limit, offset int) []any {
	return []any{
		"", []string{}, "", (*time.Time)(nil), []string{}, true, []int16{}, false,
		(*time.Time)(nil), (*time.Time)(nil), []string{}, false, false, false, limit, offset,
	}
}

// runSpeedPage reads one page on ctx through the scan the listing route uses, which is everything
// of the read but the JSON encoding of its answer.
func runSpeedPage(t *testing.T, ctx context.Context, corpus speedCorpusShape, query string, arguments []any) {
	t.Helper()
	if _, _, err := corpus.server.scanIssuePage(ctx, query, arguments); err != nil {
		t.Fatalf("read a page of the listing: %v", err)
	}
}

// assertSpeedPage holds one of the two texts to what it answers: the page and the whole count
// either way, and the relations the corpus's heaviest issue carries only where the text reads
// them. The issue below it inherits all twenty, which is the walk the readings are of. It answers
// the keys of the issues the page holds.
func assertSpeedPage(t *testing.T, corpus speedCorpusShape, query string, arguments []any, readsRelation bool) []string {
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
	keys := make([]string, 0, len(issues))
	for _, issue := range issues {
		rows[issue.Key] = issue
		keys = append(keys, issue.Key)
	}
	heaviest, drawn := rows[corpus.heaviest]
	if !drawn {
		t.Fatalf("the page does not hold %s, the issue the readings are of", corpus.heaviest)
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
	return keys
}

// assertSpeedPriorities holds the three requests to what they answer before any of them is
// counted or timed: the page with every open issue counted, the whole body with every priority
// and the relations the corpus holds, and the summary with its two counts and no list.
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
		t.Fatal("the priorities body counts no work at all; a reading of it is of an empty read")
	}
	summary := decodeBody[model.Priorities](t, speedRequest(t, corpus, "/api/v1/priorities?summary=true"))
	if len(summary.Priorities) != 0 || summary.WorkUnderNoPriority.Count != body.WorkUnderNoPriority.Count {
		t.Fatalf("the summary draws %d priorities and counts %d work under none, want none and the body's %d",
			len(summary.Priorities), summary.WorkUnderNoPriority.Count, body.WorkUnderNoPriority.Count)
	}
}

// speedRequest is one GET against the corpus's server, as a signed-in person makes it.
func speedRequest(t *testing.T, corpus speedCorpusShape, target string) *httptest.ResponseRecorder {
	t.Helper()
	return speedRequestOn(t, context.Background(), corpus, target)
}

// speedRequestOn is speedRequest on ctx, which the three request readings count or time:
// everything the server does to answer, and the recorder's write of it.
func speedRequestOn(t *testing.T, ctx context.Context, corpus speedCorpusShape, target string) *httptest.ResponseRecorder {
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
