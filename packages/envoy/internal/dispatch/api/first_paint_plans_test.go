package api

// first_paint_plans_test.go holds R12 of dispatch://INFRA-2918 (check k12) to the plans of the
// statements the Evals tab's first paint reads with: no plan reads a session title out of the event
// log, the deploy run-jobs read names the jobs it reads, the failures list reads only the runs that
// completed in the 24 hours before its range's end, and no plan scans the issues table.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// firstPaintRead marks the context of one read of the first paint with the read's name, so the
// tracer records that read's statements alone, each under the read that sent it, and none of a
// settlement, a refresher pass or the test's own.
type firstPaintRead struct{}

// paintStatement is one statement a read of the first paint sent: the read, the statement and the
// arguments it was sent with, which is what lets its plan be taken of the statement the read made
// rather than of a copy of it.
type paintStatement struct {
	read string
	sql  string
	args []any
}

// paintStatements records every statement a marked read sends (firstPaintRead), each sent on its
// own (pgx.QueryTracer) or on a pipeline (pgx.BatchTracer) alike.
type paintStatements struct {
	mu   sync.Mutex
	sent []paintStatement
}

func (p *paintStatements) record(ctx context.Context, sql string, args []any) {
	read, marked := ctx.Value(firstPaintRead{}).(string)
	if !marked {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, paintStatement{read: read, sql: sql, args: slices.Clone(args)})
}

func (p *paintStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.record(ctx, data.SQL, data.Args)
	return ctx
}

func (p *paintStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *paintStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		p.record(ctx, queued.SQL, queued.Arguments)
	}
	return ctx
}

func (p *paintStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (p *paintStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// statements is every statement recorded, in the order they started.
func (p *paintStatements) statements() []paintStatement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.sent)
}

// explainable matches a statement Postgres can explain: a query, as against the transaction
// control (begin, commit, rollback) a snapshot read sends around its queries.
var explainable = regexp.MustCompile(`(?is)^\s*(select|with|values)\b`)

// jobNameCondition matches a condition that names the jobs a read of delivery_run_jobs reads.
var jobNameCondition = regexp.MustCompile(`\bname = `)

// The reads of the first paint the test sends, by name: the four evals views, one per kind, and
// pr-ci's two items views of the CI section.
const (
	paintSlowestJobs = "items pr-ci sort=median"
	paintFailedJobs  = "items pr-ci sort=failures"
)

// TestTheFirstPaintsReadsReadNoEventsAndScanNoIssues (dispatch://INFRA-2918, R12, check k12): the
// reads of one first paint of the Evals tab, at the 84-day preset with the compare range the header
// defaults to (the 84 whole UTC days before the window's first), as the tab subscribes them (the
// web's evalsViewQuery and itemsViewQuery): the four evals views, one of each kind, 200 evals a
// page, and the CI section's two items views of pr-ci, the slowest jobs (sort=median, 5 rows) and
// the jobs failed in the last 24 hours (sort=failures, 200 rows). The merge timeline is closed on
// the first paint (R13), so its view is no read of it. Each view is read through the function the
// live hub reads it with (deliveryLiveClasses.read), on a server whose pool a tracer records every
// statement of, with its arguments; each query is then explained with those arguments, with
// sequential scans off, since a fixture this small is read fastest whole however a statement is
// written, and a scan carrying no index condition is then what says no index serves a read. The
// project holds a logged eval whose `last` series a step chart draws, so the evals views read its
// value at each range's end from its readings (evals.StepLastIn, D20 (b)) among their statements.
//
// It fails when any plan reads the events table, the log a session title was once resolved out of
// (the stored title is session_titles'); when a read of delivery_run_jobs outside the failures read
// (the deploy run-jobs read) does not name its jobs; when the failures read reads a run by anything
// but its completion in the 24 hours before the range's end (its read of delivery_runs bounds
// completed_at by those 24 hours in its index condition, sent as the two ends of them, and its read
// of delivery_run_jobs names those runs alone, every job of each, as measures.FailingJobs tallies
// each job's results); when any plan has a Seq Scan on issues; when no plan of the evals views reads
// the step series' readings through eval_readings_series; and when no statement was captured.
// It prints what each read sent and read.
func TestTheFirstPaintsReadsReadNoEventsAndScanNoIssues(t *testing.T) {
	log := &paintStatements{}
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{tracer: log})
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Seed: 2918, OneProject: true, Days: 30})
	project := fixture.Project
	now := fixture.To.Add(-time.Minute)
	const failedJob = "paint / failed"
	failed := writeFailedChecksRun(t, database, fixture.DeployRepo, projectPullRequest(t, fixture, fixture.DeployRepo), 991_000_101,
		now.Add(-3*time.Hour), now.Add(-2*time.Hour), failedJob)
	registerLastSeries(t, handler, project, "admission", []string{"cap"}, chartList(stepChart("cap", "cap", "")))
	logReadings(t, handler, project, "admission", "paint-cap", stepReading("cap", "", "cap-1", new(4.0), measures.Midnight(now).AddDate(0, 0, -2)))
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, now)

	// The header's 84-day preset, and the compare range it defaults to (the web's compareRange):
	// the window covers the whole UTC days from the one 84 days back to today, and the compare range
	// the 84 days before its first.
	preset := 84
	window := DeliveryLiveWindow{Preset: &preset}
	first := measures.Midnight(now.AddDate(0, 0, -preset))
	compare := url.Values{"compare_from": {first.AddDate(0, 0, -preset).Format(time.RFC3339)}, "compare_to": {first.Format(time.RFC3339)}}
	type paintRead struct {
		name string
		view DeliveryLiveView
	}
	var reads []paintRead
	for _, kind := range evals.Kinds {
		query := maps.Clone(compare)
		query.Set("kind", kind)
		query.Set("limit", "200")
		reads = append(reads, paintRead{"evals kind=" + kind, evalsLiveView(project, window, query.Encode())})
	}
	reads = append(reads,
		paintRead{paintSlowestJobs, itemsLiveView(project, evals.SlugPRCI, window, url.Values{"sort": {evalSortMedian}, "limit": {"5"}}.Encode())},
		paintRead{paintFailedJobs, itemsLiveView(project, evals.SlugPRCI, window, url.Values{"sort": {evalSortFailures}, "limit": {"200"}}.Encode())},
	)

	hub := newDeliveryHub(served, t.Context(), newStillHubClock())
	for _, paint := range reads {
		spec, refusal := parseDeliveryLiveView(paint.view)
		if refusal != nil {
			t.Fatalf("%s: the hub refuses the view: %+v", paint.name, refusal)
		}
		var read deliveryViewRead
		body, err := spec.classes().read(hub, context.WithValue(t.Context(), firstPaintRead{}, paint.name), spec, now, &read)
		if err != nil {
			t.Fatalf("%s: read the view: %v", paint.name, err)
		}
		if paint.name == paintFailedJobs {
			items, _ := body.(evalItemsResponse)
			if !slices.ContainsFunc(items.Items, func(row evalItemRow) bool { return row.RowKey == failingJobKey(fixture.DeployRepo, failedJob) }) {
				t.Fatalf("the failures read answers %s, not %s, which run %d failed in the 24 hours, so it read no run of them",
					mustJSON(t, body), failedJob, failed.RunID)
			}
		}
	}

	ran := log.statements()
	if len(ran) == 0 {
		t.Fatal("no statement of the first paint's reads was captured, so no plan says anything of them")
	}
	end := now
	horizon := end.Add(-measures.FailingWindow)
	type tally struct{ sent, explained int }
	tallies := map[string]*tally{}
	for _, paint := range reads {
		tallies[paint.name] = &tally{}
	}
	relations := map[string]int{}
	var deployJobReads, failureRunReads, failureJobReads, stepLastReads int
	for _, statement := range ran {
		counts := tallies[statement.read]
		counts.sent++
		if !explainable.MatchString(statement.sql) {
			continue
		}
		counts.explained++
		plan := planWithoutSeqScans(t, database, statement.sql, statement.args...)
		sql := strings.Join(strings.Fields(statement.sql), " ")
		read := map[string]bool{}
		for _, scan := range explaintest.Scans(t, plan) {
			read[scan.Relation] = true
		}
		for relation := range read {
			relations[relation]++
		}
		if read["events"] {
			t.Errorf("%s: a plan reads the event log, out of which a session title was once resolved; the first paint reads the stored title:\n%s\n%s",
				statement.read, sql, explaintest.Outline(t, plan))
		}
		if explaintest.SeqScansRelation(t, plan, "issues") {
			t.Errorf("%s: a plan scans issues whole, where an index or a stored day row answers it:\n%s\n%s", statement.read, sql, explaintest.Outline(t, plan))
		}
		if strings.HasPrefix(statement.read, "evals ") && slices.ContainsFunc(explaintest.IndexScans(t, plan), func(scan explaintest.IndexScan) bool {
			return scan.Index == "eval_readings_series"
		}) {
			stepLastReads++
		}
		if statement.read == paintFailedJobs {
			if read["delivery_runs"] {
				failureRunReads++
				requireReadByCompletion(t, statement, plan, horizon, end)
			}
			if read["delivery_run_jobs"] {
				failureJobReads++
				requireJobsOfRunsCompletedIn(t, database, statement, fixture.DeployRepo, horizon, end, failed.RunID)
			}
			continue
		}
		if read["delivery_run_jobs"] {
			deployJobReads++
			for _, node := range explaintest.Nodes(t, plan) {
				if node.Relation == "delivery_run_jobs" && !jobNameCondition.MatchString(node.IndexCond+" "+node.Filter+" "+node.RecheckCond) {
					t.Errorf("%s: the run-jobs read reads delivery_run_jobs with no condition naming its jobs (%s):\n%s\n%s",
						statement.read, node, sql, explaintest.Outline(t, plan))
				}
			}
		}
	}
	if failureRunReads == 0 || failureJobReads == 0 {
		t.Errorf("the failures read sent %d reads of delivery_runs and %d of delivery_run_jobs, want its runs read and their jobs", failureRunReads, failureJobReads)
	}
	if stepLastReads == 0 {
		t.Error("no plan of the evals views reads eval_readings through eval_readings_series, so the step series' value at each range's end was read by no statement the test explained")
	}

	explained := 0
	for _, paint := range reads {
		counts := tallies[paint.name]
		if counts.sent == 0 {
			t.Errorf("%s: no statement of the read was captured", paint.name)
		}
		explained += counts.explained
		t.Logf("%s: %d statements sent, %d of them queries explained", paint.name, counts.sent, counts.explained)
	}
	var read []string
	for _, relation := range slices.Sorted(maps.Keys(relations)) {
		read = append(read, fmt.Sprintf("%s %d", relation, relations[relation]))
	}
	t.Logf("the first paint's %d reads sent %d statements, %d of them queries explained; the plans read %s; reads of events %d, of issues %d; "+
		"%d reads of delivery_run_jobs outside the failures read (a deploy run-jobs read), each naming its jobs; the failures read sent %d reads of delivery_runs, each by completion in %s..%s, and %d of delivery_run_jobs; "+
		"%d plans of the evals views read the step series' readings through eval_readings_series",
		len(reads), len(ran), explained, strings.Join(read, ", "), relations["events"], relations["issues"],
		deployJobReads, failureRunReads, horizon.Format(time.RFC3339), end.Format(time.RFC3339), failureJobReads, stepLastReads)
}

// requireReadByCompletion holds the failures read's statement of delivery_runs to reading runs by
// their completion in the 24 hours [horizon, end]: the statement bounds completed_at from below and
// above, sent as horizon and end, and its plan reads delivery_runs through an index condition (or
// the recheck of a bitmap read by one) that holds both bounds, so no run that completed outside
// them is read, whatever it started.
func requireReadByCompletion(t *testing.T, statement paintStatement, plan json.RawMessage, horizon, end time.Time) {
	t.Helper()
	sql := strings.Join(strings.Fields(statement.sql), " ")
	for operator, want := range map[string]time.Time{">=": horizon, "<=": end} {
		match := regexp.MustCompile(`completed_at ` + operator + ` \$(\d+)`).FindStringSubmatch(sql)
		if match == nil {
			t.Errorf("the failures read's read of delivery_runs bounds no completed_at by %s: %s", operator, sql)
			continue
		}
		position, _ := strconv.Atoi(match[1])
		if at, ok := statement.args[position-1].(time.Time); !ok || !at.Equal(want) {
			t.Errorf("the failures read's read of delivery_runs sends completed_at %s $%d = %v, want %s", operator, position, statement.args[position-1],
				want.Format(time.RFC3339))
		}
	}
	for _, node := range explaintest.Nodes(t, plan) {
		if node.Relation != "delivery_runs" {
			continue
		}
		keyed := node.IndexCond + " " + node.RecheckCond
		if !strings.Contains(keyed, "completed_at >=") || !strings.Contains(keyed, "completed_at <=") {
			t.Errorf("the failures read reads delivery_runs by %s, not by the runs' completion in the 24 hours before the range's end:\n%s\n%s",
				node, sql, explaintest.Outline(t, plan))
		}
	}
}

// requireJobsOfRunsCompletedIn holds the failures read's statement of delivery_run_jobs to the jobs
// of the runs of repo that completed in [horizon, end], every job of each: it names no job, it
// names the run that failed in those hours (want), and every run it names completed in them.
func requireJobsOfRunsCompletedIn(t *testing.T, database *store.Store, statement paintStatement, repo string, horizon, end time.Time, want int64) {
	t.Helper()
	if jobNameCondition.MatchString(statement.sql) {
		t.Errorf("the failures read's read of the runs' jobs names jobs, want every job of each run: %s", statement.sql)
	}
	var named []int64
	for _, arg := range statement.args {
		if ids, ok := arg.([]int64); ok {
			named = append(named, ids...)
		}
	}
	if !slices.Contains(named, want) {
		t.Errorf("the failures read's read of the jobs names the runs %v, not run %d, which completed in the 24 hours", named, want)
	}
	rows, err := database.Pool.Query(context.Background(), `select run_id, completed_at from delivery_runs where repo = $1 and run_id = any($2)`, repo, named)
	if err != nil {
		t.Fatalf("read the runs the jobs' read names: %v", err)
	}
	var id int64
	var completed *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &completed}, func() error {
		if completed == nil || completed.Before(horizon) || completed.After(end) {
			t.Errorf("the failures read reads the jobs of run %d, completed %v, outside the 24 hours %s..%s", id, completed,
				horizon.Format(time.RFC3339), end.Format(time.RFC3339))
		}
		return nil
	}); err != nil {
		t.Fatalf("read the runs the jobs' read names: %v", err)
	}
}
