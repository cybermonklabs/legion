package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The tests of GET /api/v1/projects/{key}/evals/{slug}/items (evals_items.go). The tests of the
// other eval routes, and the helpers every eval route's test reads them through, are evals_test.go's.

// evalItemFields is the field set of every items row, whatever its kind (R7).
var evalItemFields = []string{"cols", "href", "links", "measured_at", "range", "row_key", "row_kind", "source"}

// itemMeasuredAt is when an items row of kind, of slug's eval of project, over days, was measured
// by the rule the items route states (evalItemRow.MeasuredAt), read from the row's own columns
// and, for a job, from its day rows; and the rule, in words. A kind no rule names fails the test.
func itemMeasuredAt(t *testing.T, database *store.Store, project, slug, kind string, days evals.Range, row map[string]json.RawMessage) (string, time.Time) {
	t.Helper()
	var cols map[string]json.RawMessage
	if err := json.Unmarshal(row["cols"], &cols); err != nil {
		t.Fatalf("decode the columns of a %s row of %s: %v", kind, slug, err)
	}
	column := func(name string) time.Time {
		var at time.Time
		if err := json.Unmarshal(cols[name], &at); err != nil {
			t.Fatalf("decode the column %s of a %s row of %s (%s): %v", name, kind, slug, cols[name], err)
		}
		return at
	}
	switch kind {
	case evalRowPullRequest:
		return "its merge", column("merged_at")
	case evalRowDeploy, evalRowRun:
		return "its run's start, the day its measure counts it on", column("started_at")
	case evalRowDay:
		var day string
		if err := json.Unmarshal(cols["day"], &day); err != nil {
			t.Fatalf("decode the day of a day row of %s: %v", slug, err)
		}
		at, err := time.Parse(time.DateOnly, day)
		if err != nil {
			t.Fatalf("parse the day %q of a day row of %s: %v", day, slug, err)
		}
		return "its day", at
	case evalRowJob:
		var name string
		if err := json.Unmarshal(row["row_key"], &name); err != nil {
			t.Fatalf("decode a job's name: %v", err)
		}
		var newest time.Time
		if err := database.Pool.QueryRow(t.Context(), `
			select max(day)::timestamp at time zone 'UTC' from eval_days
			where project_key = $1 and eval_id = $2 and series = $3 and repo = '' and day >= $4 and day < $5
		`, project, evals.EvalID(project, slug), measures.JobSeriesPrefix+name, days.From, days.To).Scan(&newest); err != nil {
			t.Fatalf("read the newest day of job %s: %v", name, err)
		}
		return "the newest day its day rows of the range are of", newest
	case evalRowIssue:
		// A closed issue's row states its close, and its filing too where the range filed it; a
		// filing's row its filing alone.
		if _, closed := cols[legionfacts.ColumnClosedAt]; closed {
			return "its close", column(legionfacts.ColumnClosedAt)
		}
		return "its filing", column(legionfacts.ColumnFiledAt)
	case "sample":
		var measured time.Time
		if err := database.Pool.QueryRow(t.Context(), `select measured_at from eval_readings where id = $1`, mustString(t, cols["id"])).Scan(&measured); err != nil {
			t.Fatalf("read the reading behind a row of %s: %v", slug, err)
		}
		return "the moment its reading was logged as measured", measured
	}
	t.Fatalf("no rule names when a %s row of %s is measured", kind, slug)
	return "", time.Time{}
}

// mustString is a JSON string's value.
func mustString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s as a string: %v", raw, err)
	}
	return value
}

// TestTheFailingJobsItemsAreThePipelinesFailingJobs: pr-ci's items with sort=failures over the
// range from the first of the seven days an `at` shows to that at answer the list
// GET /api/v1/delivery/pipeline answers in pr_checks.failing_jobs for that at, one list and two
// readers (measures.FailingJobs over the runs of those days): for an at whose 24 hours hold a
// run of the deliverytest history that started before them and failed a job inside them, and for
// an at on a midnight. Each row is keyed `<repo>/<name>`, which a row filter finds alone.
func TestTheFailingJobsItemsAreThePipelinesFailingJobs(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Seed: 2916, OneProject: true, Days: 14})
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))

	// A pull-request checks run that failed a job, ending the window one second past the 24 hours
	// after its start: it started before the 24 hours the table counts and completed inside them.
	var straddling *delivery.DeliveryRun
	for index, run := range fixture.Runs {
		outcome, timed := measures.OutcomeOf(run.Conclusion)
		failed := slices.ContainsFunc(fixture.Jobs[run.RunID], func(job delivery.DeliveryRunJob) bool {
			jobOutcome, ok := measures.OutcomeOf(job.Conclusion)
			return ok && jobOutcome == measures.OutcomeFailure
		})
		if run.Kind == delivery.DeliveryRunKindPRChecks && timed && outcome != measures.OutcomeCancelled && failed &&
			run.CompletedAt != nil && run.CompletedAt.Sub(run.StartedAt) > 2*time.Second &&
			run.StartedAt.Add(measures.FailingWindow+time.Second).Before(fixture.To) && run.StartedAt.After(fixture.From.AddDate(0, 0, 1)) {
			straddling = &fixture.Runs[index]
			break
		}
	}
	if straddling == nil {
		t.Fatal("the history holds no pull-request checks run that failed a job, so no window straddles one")
	}
	straddleTo := straddling.StartedAt.Add(measures.FailingWindow + time.Second)
	for _, pinned := range []struct {
		name string
		at   time.Time
	}{
		{"an at whose 24 hours hold a run started before them", straddleTo},
		{"an at on a midnight", fixture.From.AddDate(0, 0, 11)},
	} {
		at := url.Values{"at": {pinned.at.Format(time.RFC3339Nano)}}.Encode()
		query := url.Values{"from": {measures.WindowStart(pinned.at).Format(time.RFC3339Nano)}, "to": {pinned.at.Format(time.RFC3339Nano)}}.Encode()
		pipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+at).PRChecks.FailingJobs
		items := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals/pr-ci/items?sort=failures&limit=200&"+query)
		answered := make([]measures.FailingJob, 0, len(items.Items))
		for _, row := range items.Items {
			var job measures.FailingJob
			if err := json.Unmarshal([]byte(mustJSON(t, row.Cols)), &job); err != nil {
				t.Fatalf("decode the failing job row %s: %v", row.RowKey, err)
			}
			if repo, _ := row.Cols["repo"].(string); row.RowKey != repo+"/"+job.Name {
				t.Errorf("the failing job %s of %s is keyed %q, want %q", job.Name, repo, row.RowKey, repo+"/"+job.Name)
			}
			answered = append(answered, job)
		}
		if len(pipeline) == 0 {
			t.Fatalf("%s (%s): the Pipeline answers no failing job, so the comparison compares nothing", pinned.name, at)
		}
		if got, want := mustJSON(t, answered), mustJSON(t, pipeline); got != want {
			t.Errorf("%s (%s, %s): the items answer the failing jobs %s, the Pipeline %s", pinned.name, at, query, got, want)
			continue
		}
		t.Logf("%s (%s, %s): the items and the Pipeline answer the same %d failing jobs, the first %+v", pinned.name, at, query, len(pipeline), pipeline[0])
		first := items.Items[0].RowKey
		filtered := evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals/pr-ci/items?sort=failures&limit=200&"+query+"&"+url.Values{"row": {first}}.Encode())
		if len(filtered.Items) != 1 || filtered.Items[0].RowKey != first {
			t.Errorf("%s: the row filter %q answers %d rows, want the one row it keys", pinned.name, first, len(filtered.Items))
		}
	}
	t.Logf("the straddling run %d started %s, completed %s; its at is %s", straddling.RunID,
		straddling.StartedAt.Format(time.RFC3339), straddling.CompletedAt.Format(time.RFC3339), straddleTo.Format(time.RFC3339))
}

// Two repositories' failed jobs of one name are two items rows, one per repository. The row
// filter finds each alone, the job series finds both, and the Pipeline tallies them as one job.
func TestTwoRepositoriesFailingJobsOfOneNameAreTwoRows(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	const project, repoA, repoB = "TWOR", "acme/repo-a", "acme/repo-b"
	issue := createTestIssue(t, handler, project, "Two source repositories")
	saveProjectRules(t, database, project, deployRule(repoA, "a / release"), deployRule(repoB, "b / release"))
	seedSourceCheck(t, database, repoA, issue, 101, 8, true)
	seedSourceCheck(t, database, repoA, issue, 102, 9, true)
	seedSourceCheck(t, database, repoB, issue, 201, 10, true)
	leaveAttributionInputsUnread(t, database.Pool)
	at := sourcesDay.Add(12 * time.Hour)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, at)

	query := url.Values{
		"from": {measures.WindowStart(at).Format(time.RFC3339)}, "to": {at.Format(time.RFC3339)},
		"sort": {"failures"},
	}
	path := evalItemsPath(project, evals.SlugPRCI)
	items := evalGet[evalItemsResponse](t, handler, path+"?"+query.Encode())
	if len(items.Items) != 2 || items.Total != 2 {
		t.Fatalf("sort=failures answers %d rows of %d, want two build rows, one per repository", len(items.Items), items.Total)
	}
	var failures float64
	for i, repo := range []string{repoA, repoB} {
		row := items.Items[i]
		key := repo + "/build"
		n, _ := row.Cols["failures"].(float64)
		if row.RowKey != key || row.Cols["repo"] != repo || row.Cols["name"] != "build" || n != float64(2-i) {
			t.Errorf("row %d is %q with %v, want %q naming %s and %d failures", i, row.RowKey, row.Cols, key, repo, 2-i)
		}
		failures += n
		query.Set("row", key)
		filtered := evalGet[evalItemsResponse](t, handler, path+"?"+query.Encode())
		if len(filtered.Items) != 1 || filtered.Total != 1 || filtered.Items[0].RowKey != key {
			t.Errorf("row filter %q answers %d rows of %d, want that row alone", key, len(filtered.Items), filtered.Total)
		}
	}
	query.Del("row")
	query.Set("series", "job:build")
	series := evalGet[evalItemsResponse](t, handler, path+"?"+query.Encode())
	if len(series.Items) != 2 || series.Total != 2 || series.Items[0].RowKey != repoA+"/build" || series.Items[1].RowKey != repoB+"/build" {
		t.Errorf("series=job:build answers %v, want both repositories' build rows", series.Items)
	}
	pipeline := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?at="+at.Format(time.RFC3339)).PRChecks.FailingJobs
	if len(pipeline) != 1 || pipeline[0].Name != "build" || float64(pipeline[0].Failures) != failures {
		t.Errorf("Pipeline failing_jobs = %+v, want one build row with %v failures (the sum of the items)", pipeline, failures)
	}
}

// TestTheSlowestJobsItemsAreThePipelinesSlowestJobs: pr-ci's items with sort=median over the
// seven days an `at` shows rank the jobs GET /api/v1/delivery/pipeline ranks in
// pr_checks.slowest_jobs for that at: the Pipeline's table is the first measures.SlowestJobs of the
// items' ranking, as many as the ranking holds up to that count, one ranking and two readers (R7:
// no read both families serve gets two answers), and they stay one after a register write sets
// pr-ci's job series' min_n: here to one past the timed results of the Pipeline's busiest slowest
// job, so a ranking that reads the registry drops every job, where one that does not keeps them
// all.
func TestTheSlowestJobsItemsAreThePipelinesSlowestJobs(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Seed: 2916, OneProject: true, Days: 14})
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))
	// The Pipeline's seven days ending on the history's eleventh, and the items' range of the
	// same days.
	pinned := fixture.From.AddDate(0, 0, 11).Add(-time.Second)
	at := url.Values{"at": {pinned.Format(time.RFC3339)}}.Encode()
	query := url.Values{"from": {measures.WindowStart(pinned).Format(time.RFC3339)}, "to": {pinned.Format(time.RFC3339)}}.Encode()
	ranked := func() (pipeline, items []string) {
		t.Helper()
		for _, job := range evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+at).PRChecks.SlowestJobs {
			pipeline = append(pipeline, job.Name)
		}
		for _, row := range evalGet[evalItemsResponse](t, handler, "/api/v1/projects/"+fixture.Project+"/evals/pr-ci/items?sort=median&limit=200&"+query).Items {
			items = append(items, row.RowKey)
		}
		return pipeline, items
	}
	same := func(pipeline, items []string) bool {
		return len(pipeline) == min(len(items), measures.SlowestJobs) && slices.Equal(items[:len(pipeline)], pipeline)
	}
	slowest := evalGet[deliveryPipelineResponse](t, handler, "/api/v1/delivery/pipeline?"+at).PRChecks.SlowestJobs
	if len(slowest) < 2 {
		t.Fatalf("the Pipeline ranks %d slowest jobs (%s), so no min_n can tell its ranking from another", len(slowest), query)
	}
	pipeline, before := ranked()
	if !same(pipeline, before) {
		t.Fatalf("before any register write the items rank %v, the Pipeline %v", before, pipeline)
	}
	minN := 0
	for _, job := range slowest {
		minN = max(minN, job.Total.N+1)
	}
	registered := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+fixture.Project+"/evals", map[string]any{
		"slug": "pr-ci", "series": []any{map[string]any{"series": evals.SeriesJobs, "min_n": minN}},
	}, "pr-ci-min-n")
	if registered.Code != http.StatusCreated && registered.Code != http.StatusOK {
		t.Fatalf("set pr-ci's min_n to %d: status %d, body %s", minN, registered.Code, registered.Body.String())
	}
	pipeline, items := ranked()
	if !same(pipeline, items) || len(items) >= len(before) {
		t.Errorf("with pr-ci's job series' min_n at %d the items rank %v, the Pipeline %v, where before it the items ranked %d jobs; want one ranking, of fewer jobs",
			minN, items, pipeline, len(before))
	}
}

// TestEveryCatalogueSourceAnswersItsRows: every computed eval a project is seeded with answers,
// through the items route, rows its numbers count beside its day rows, so no computed eval's items
// are its readings and day rows alone by an omission (evalComputedItems): over the histories the
// eval reads are checked over (seedEvalHistoriesByIssue), each Legion eval of the Legion history
// over its first golden window, each failure measure of the failure history (seedFailureHistory:
// its rows are the record's deploys and failures, dispatch://INFRA-978 D7, which the delivery
// history's project, with no failure source, holds none of) over its two days, and each other
// computed eval of the delivery history over seven of its days, answers at least one row of a kind
// other than `day`. The folds' own table is held to the catalogue when the evals package starts.
func TestEveryCatalogueSourceAnswersItsRows(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	// The record is written before the histories' one refresh, which builds every project's rows.
	seedFailureHistory(t, database)
	fixture, windows := seedEvalHistoriesByIssue(t, database, deps, 14)
	legion, failures := windows[0].Spec.Project, "FAIL"
	ranges := map[string][2]time.Time{
		fixture.Project: {fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)},
		legion:          {windows[0].Window.From, windows[0].Window.To},
		failures:        {failuresDay, failuresDay.AddDate(0, 0, 2)},
	}
	failureSources := []string{evals.SourceFoldChangeFailRate, evals.SourceFoldRecovery, evals.SourceFoldReworkRate}
	computed := 0
	for _, project := range []string{fixture.Project, legion, failures} {
		list := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?limit=200")
		for _, item := range list.Items {
			definition := item.Definition
			// Each eval is checked over the history that holds its input.
			history := fixture.Project
			switch {
			case definition.Kind == evals.KindLegion:
				history = legion
			case slices.Contains(failureSources, definition.Source):
				history = failures
			}
			if !definition.Computed() || history != project {
				continue
			}
			computed++
			query := url.Values{"from": {ranges[project][0].Format(time.RFC3339)}, "to": {ranges[project][1].Format(time.RFC3339)}, "limit": {"200"}}
			items := evalGet[evalItemsResponse](t, handler, evalItemsPath(project, definition.Slug)+"?"+query.Encode())
			counted := map[string]int{}
			for _, row := range items.Items {
				if row.RowKind != evalRowDay {
					counted[row.RowKind]++
				}
			}
			if len(counted) == 0 {
				t.Errorf("%s (%s) answers %d rows over %s, every one a day row: no row its numbers count", definition.EvalID, definition.Source, items.Total, query.Encode())
				continue
			}
			t.Logf("%s answers %v beside its day rows", definition.EvalID, counted)
		}
	}
	if computed == 0 {
		t.Fatal("the seeded projects hold no computed eval, so the check checked nothing")
	}
	t.Logf("%d seeded computed evals, each with rows its numbers count", computed)
}

// TestOneEvalItemsAnswerReadsItsRowsFromOneSnapshot (point 6 of dispatch://INFRA-2916, one answer,
// one snapshot): an items answer of deploy-frequency, its deploy runs, beside the items its day
// rows counted (eval_day_members), and its day rows, is one database moment. A deploy's production
// job fails and the refresher's pass rewrites the day rows of its day, their members and the
// stored figures they fold, all of it committed, on another connection, the moment the answer's
// first statement of the counted items ends. The answer the commit landed in is the answer before
// it, byte for byte, and the next answer is the new state, in which the deploy no longer reached
// production. A read whose statements read several moments answers, in the answer the commit lands
// in, the counted items before it beside the deploy runs and the day rows after it.
func TestOneEvalItemsAnswerReadsItsRowsFromOneSnapshot(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	commits := storetest.NewCommitBetweenReads("eval_day_members")
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: storetest.TracedStore(t, database, commits)})
	fixture, windows := seedEvalHistories(t, database, deps, 30)
	end := windows[0].Spec.Now
	if fixture.To.After(end) {
		end = fixture.To
	}
	now := end.Add(-time.Minute)
	registry := evals.New(database, deps.Docs, deps.Events, deps.Attributor)
	window := evalDaysEnding(7, measures.Midnight(now))
	target := evalItemsPath(fixture.Project, evals.SlugDeployFrequency) + "?" + url.Values{
		"from": {window.From.Format(time.RFC3339)}, "to": {window.To.Format(time.RFC3339)}, "limit": {"200"},
	}.Encode()
	var runID int64
	for _, apply := range fixture.Applies {
		if !apply.CompletedAt.Before(window.From) && apply.CompletedAt.Before(window.To) {
			runID = apply.RunID
		}
	}
	repo := ""
	for _, run := range fixture.Runs {
		if run.RunID == runID {
			repo = run.Repo
		}
	}
	if repo == "" {
		t.Fatalf("no production apply of the history completed in %s..%s", window.From, window.To)
	}
	before := getBody(t, handler, target)
	reached := func(body json.RawMessage) (bool, bool) {
		t.Helper()
		var answer evalItemsResponse
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode the items answer: %v", err)
		}
		for _, row := range answer.Items {
			if row.RowKind == evalRowDeploy && row.RowKey == repo+"/"+strconv.FormatInt(runID, 10) {
				value, _ := row.Cols["reached_production"].(bool)
				return value, true
			}
		}
		return false, false
	}
	if was, listed := reached(before); !listed || !was {
		t.Fatalf("before the commit the items list run %d as a deploy that reached production %t (listed %t), and the check needs it listed as one", runID, was, listed)
	}

	commits.Arm(func() {
		if _, err := database.Pool.Exec(t.Context(), `update delivery_run_jobs set conclusion = 'failure' where repo = $1 and run_id = $2 and name = any($3)`,
			repo, runID, fixture.Settings.ProductionJobNames); err != nil {
			t.Errorf("fail the production job of run %d: %v", runID, err)
		}
		refreshEvalDays(t, registry, database, now)
	})
	during := getBody(t, handler, target)
	if !commits.Fired() {
		t.Fatal("no statement of the items read read eval_day_members, so no commit landed during it")
	}
	if !bytes.Equal(during, before) {
		was, _ := reached(during)
		t.Errorf("the answer the commit landed in differs from the answer before it at byte %d of %d: run %d reached production %t in it",
			firstDifference(during, before), len(before), runID, was)
	}

	after := getBody(t, handler, target)
	if bytes.Equal(after, before) {
		t.Errorf("after the commit the items answer what they answered before it: the failed production job of run %d moved nothing", runID)
	}
	if was, listed := reached(after); !listed || was {
		t.Errorf("after the commit the items list run %d %t, as a deploy that reached production %t; want it listed, as one that did not", runID, listed, was)
	}
	t.Logf("the answer the commit landed in is the %d bytes of the answer before it; the next answer moved run %d", len(before), runID)
}

// failingRuns is a deliverytest history beside which two pull-request checks runs of one of its
// pull requests are written, each counted for the history's project by its pull request's issue,
// and the end of a seven-day range whose failing jobs they test: straddle started 25 hours before
// end and failed 23 hours before it, and early started in the range and failed before the 24 hours
// before end. Each failed a job of a name no other run holds.
type failingRuns struct {
	fixture         deliveryseed.Fixture
	from, end       time.Time
	straddle, early delivery.DeliveryRun
}

// The names of the jobs failingRuns' two runs fail.
const (
	straddleJob = "items / straddle"
	earlyJob    = "items / early"
)

// query is the items route's query of pr-ci's failing jobs over the range.
func (f failingRuns) query() string {
	return url.Values{
		"sort": {evalSortFailures}, "limit": {"200"}, "from": {f.from.Format(time.RFC3339Nano)}, "to": {f.end.Format(time.RFC3339Nano)},
	}.Encode()
}

// seedFailingRuns writes failingRuns into database: a deliverytest history of 14 days, the two runs
// beside it, and the day rows the refresher builds of them all.
func seedFailingRuns(t *testing.T, database *store.Store, deps Deps) failingRuns {
	t.Helper()
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Seed: 2918, OneProject: true, Days: 14})
	end := measures.Midnight(fixture.To).AddDate(0, 0, -2).Add(10 * time.Hour)
	f := failingRuns{fixture: fixture, from: end.AddDate(0, 0, -7), end: end}
	number := projectPullRequest(t, fixture, fixture.DeployRepo)
	f.straddle = writeFailedChecksRun(t, database, fixture.DeployRepo, number, 991_000_001, end.Add(-25*time.Hour), end.Add(-23*time.Hour), straddleJob)
	f.early = writeFailedChecksRun(t, database, fixture.DeployRepo, number, 991_000_002, end.AddDate(0, 0, -5), end.AddDate(0, 0, -5).Add(time.Hour), earlyJob)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))
	return f
}

// projectPullRequest is the number of a pull request of repo in fixture that names an issue of
// fixture's project, so a run of its checks counts for that project.
func projectPullRequest(t *testing.T, fixture deliveryseed.Fixture, repo string) int {
	t.Helper()
	index := slices.IndexFunc(fixture.PullRequests, func(pr delivery.DeliveryPullRequest) bool {
		return pr.Repo == repo && pr.IssueKey != nil && slices.Contains(fixture.Issues[fixture.Project], *pr.IssueKey)
	})
	if index < 0 {
		t.Fatalf("no pull request of %s names an issue of %s, so no run of its checks counts for it", repo, fixture.Project)
	}
	return fixture.PullRequests[index].Number
}

// writeFailedChecksRun writes run id of repo, a pull-request checks run of pull request number
// that started at started and failed at completed, with the job failed failing and "items / steady"
// passing.
func writeFailedChecksRun(t *testing.T, database *store.Store, repo string, number int, id int64, started, completed time.Time, failed string) delivery.DeliveryRun {
	t.Helper()
	ctx := t.Context()
	failure, jobFailure, jobSuccess := delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure, delivery.DeliveryJobConclusionSuccess
	run := delivery.DeliveryRun{
		Repo: repo, RunID: id, Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(number),
		HeadSHA: fmt.Sprintf("%040d", id), HeadCommitAt: started.Add(-time.Minute), StartedAt: started, CreatedAt: &started,
		CompletedAt: &completed, Conclusion: &failure, URL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id),
		HeadBranch: new("feature"), Event: new("pull_request"),
	}
	if err := delivery.UpsertRun(ctx, database.Pool, run); err != nil {
		t.Fatalf("write run %d: %v", id, err)
	}
	if err := delivery.UpsertRunJobs(ctx, database.Pool, repo, id, []delivery.DeliveryRunJob{
		{Repo: repo, RunID: id, Name: failed, StartedAt: &started, CompletedAt: &completed, Conclusion: &jobFailure},
		{Repo: repo, RunID: id, Name: "items / steady", StartedAt: &started, CompletedAt: &completed, Conclusion: &jobSuccess},
	}, true); err != nil {
		t.Fatalf("write run %d's jobs: %v", id, err)
	}
	return run
}

// TestTheFailedJobsListEqualsTheBasesFailingJobs (dispatch://INFRA-2918, D9): pr-ci's items with
// sort=failures read only the pull-request checks runs that completed in the 24 hours before the
// range's end, and answer the list the base answered from every run of the range:
// measures.FailingJobs over every pull-request checks run started on the range's days, with every
// job of each, of those the project's CI day rows counted. The range holds a run that started 25
// hours before its end and failed 23 hours before it, which both lists count, and one that started
// in it and failed before the 24 hours, which neither does, though both are among the runs the base
// reads.
func TestTheFailedJobsListEqualsTheBasesFailingJobs(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	f := seedFailingRuns(t, database, deps)
	ctx := t.Context()
	project := f.fixture.Project

	items := evalGet[evalItemsResponse](t, handler, evalItemsPath(project, evals.SlugPRCI)+"?"+f.query())
	answered := make([]measures.FailingJob, 0, len(items.Items))
	for _, row := range items.Items {
		var job measures.FailingJob
		if err := json.Unmarshal([]byte(mustJSON(t, row.Cols)), &job); err != nil {
			t.Fatalf("decode the failing job row %s: %v", row.RowKey, err)
		}
		answered = append(answered, job)
	}

	// The base: every pull-request checks run started on the range's days, with every job of each
	// (delivery.ReadCIRows, the read the route made before), those the project's CI day rows of the
	// range counted, through the same measures.FailingJobs, all read in one snapshot.
	tx, err := evals.New(database, deps.Docs, deps.Events, deps.Attributor).BeginSnapshot(ctx)
	if err != nil {
		t.Fatalf("begin the base's read: %v", err)
	}
	defer tx.Rollback(ctx)
	entry, found, err := evals.EvalIn(ctx, tx, evals.EvalID(project, evals.SlugPRCI))
	if err != nil || !found {
		t.Fatalf("read %s's pr-ci: found %t, %v", project, found, err)
	}
	deployRepos, err := evals.DeployReposIn(ctx, tx)
	if err != nil {
		t.Fatalf("read the deploy repositories the rows stand recorded under: %v", err)
	}
	var owned []string
	for _, repo := range deployRepos.Of(project) {
		if repo != "" && !evals.IsEventsRepo(repo) {
			owned = append(owned, repo)
		}
	}
	if len(owned) == 0 {
		t.Fatalf("%s's sources read no repository the rows stand recorded under", project)
	}
	built, err := evals.BuiltSettingsIn(ctx, tx, owned[:1])
	if err != nil || len(built) != 1 || built[0] == nil {
		t.Fatalf("read the settings the rows of %s stand built under: %+v, %v", owned[0], built, err)
	}
	settings := built[0]
	days := evalDaysOf(f.from, f.end)
	ci, err := delivery.ReadCIRows(ctx, tx, owned[0], days.From, days.To, nil)
	if err != nil {
		t.Fatalf("read every run of the range: %v", err)
	}
	counted, err := evals.CountedIn(ctx, tx, project, evals.KindCI, evals.DeployRepoOf(entry.Definition, deployRepos), nil, days)
	if err != nil {
		t.Fatalf("read the runs the range's CI day rows counted: %v", err)
	}
	members := map[string]bool{}
	for _, item := range counted {
		members[item.RowKey] = true
	}
	runs := slices.DeleteFunc(slices.Clone(ci.Checks.Runs), func(run delivery.DeliveryRun) bool { return !members[evals.RunMember(run.Repo, run.RunID)] })
	for _, held := range []delivery.DeliveryRun{f.straddle, f.early} {
		if !slices.ContainsFunc(runs, func(run delivery.DeliveryRun) bool { return run.RunID == held.RunID }) {
			t.Fatalf("the base's runs of the range hold no run %d (started %s), which the check needs counted among them", held.RunID, held.StartedAt.Format(time.RFC3339))
		}
	}
	base := measures.FailingJobs(runs, ci.Checks.Jobs, f.end, deref(settings.PRChecksRollupJobName))

	if got, want := mustJSON(t, answered), mustJSON(t, base); got != want {
		t.Fatalf("the items answer the failing jobs %s; the base's list from every run of the range is %s", got, want)
	}
	straddled := slices.IndexFunc(base, func(job measures.FailingJob) bool { return job.Name == straddleJob })
	if straddled < 0 || !base[straddled].LastFailedAt.Equal(*f.straddle.CompletedAt) || base[straddled].LastRunURL != f.straddle.URL {
		t.Errorf("the list %s does not name %s failed by run %d at %s", mustJSON(t, base), straddleJob, f.straddle.RunID, f.straddle.CompletedAt.Format(time.RFC3339))
	}
	if slices.ContainsFunc(base, func(job measures.FailingJob) bool { return job.Name == earlyJob }) {
		t.Errorf("the list %s names %s, which run %d failed before the 24 hours", mustJSON(t, base), earlyJob, f.early.RunID)
	}
	t.Logf("of the %d runs of the range the project's day rows counted, the items and the base list the same %d failing jobs: %s",
		len(runs), len(base), mustJSON(t, base))
}

// TestAJobNameTwoRepositoriesFailAnswersARowOfEach: pr-ci's failing jobs are each deploy
// repository's workflow's, so where a second deploy source of the project reads a repository whose
// pull-request checks fail a job of the name the first repository's fail in the same 24 hours, the
// failures items answer a row of each, both named that job, each naming its repository and keyed
// by it (failingJobKey): no two rows share a key.
func TestAJobNameTwoRepositoriesFailAnswersARowOfEach(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	f := seedFailingRuns(t, database, deps)
	fixture := f.fixture
	second := fixture.Settings
	second.DeployRepo = fixture.Repos[1]
	if _, err := deliveryseed.SaveRules(t.Context(), database.Pool, fixture.Project, fixture.Settings, second); err != nil {
		t.Fatalf("save %s's second deploy source, %s: %v", fixture.Project, second.DeployRepo, err)
	}
	// INFRA-2916's attribution rule (attributionListOf) counts a pull request for its issue's project
	// where the issue names that pull request's repository, or where the project's published default
	// repository is it; the second repository is no default, so the run's pull request's issue, one
	// that names no pull request of another repository, names the second repository.
	number := 0
	for _, pr := range fixture.PullRequests {
		if pr.Repo != second.DeployRepo || pr.IssueKey == nil || !slices.Contains(fixture.Issues[fixture.Project], *pr.IssueKey) ||
			slices.ContainsFunc(fixture.PullRequests, func(other delivery.DeliveryPullRequest) bool {
				return other.Repo != pr.Repo && other.IssueKey != nil && *other.IssueKey == *pr.IssueKey
			}) {
			continue
		}
		if _, err := database.Pool.Exec(t.Context(), `update issues set repo = $1 where key = $2`, second.DeployRepo, *pr.IssueKey); err != nil {
			t.Fatalf("name %s on issue %s: %v", second.DeployRepo, *pr.IssueKey, err)
		}
		number = pr.Number
		break
	}
	if number == 0 {
		t.Fatalf("no pull request of %s names an issue of %s that no pull request of another repository names", second.DeployRepo, fixture.Project)
	}
	writeFailedChecksRun(t, database, second.DeployRepo, number, 991_000_003, f.straddle.StartedAt, *f.straddle.CompletedAt, straddleJob)
	refreshEvalDays(t, evals.New(database, deps.Docs, deps.Events, deps.Attributor), database, fixture.To.Add(-time.Minute))

	items := evalGet[evalItemsResponse](t, handler, evalItemsPath(fixture.Project, evals.SlugPRCI)+"?"+f.query())
	keys := map[string]string{}
	for _, row := range items.Items {
		if _, twice := keys[row.RowKey]; twice {
			t.Errorf("two failing job rows share the key %s: %s", row.RowKey, mustJSON(t, items.Items))
		}
		repo, _ := row.Cols["repo"].(string)
		name, _ := row.Cols["name"].(string)
		if row.RowKey != failingJobKey(repo, name) {
			t.Errorf("the failing job %s of %s is keyed %s, want %s", name, repo, row.RowKey, failingJobKey(repo, name))
		}
		if name == straddleJob {
			keys[row.RowKey] = repo
		}
	}
	want := map[string]string{
		failingJobKey(fixture.DeployRepo, straddleJob): fixture.DeployRepo,
		failingJobKey(second.DeployRepo, straddleJob):  second.DeployRepo,
	}
	if !maps.Equal(keys, want) {
		t.Fatalf("the failing rows of %s are %v, want one of each repository, %v: %s", straddleJob, keys, want, mustJSON(t, items.Items))
	}
	t.Logf("%s failed in %s and %s answers the rows %v", straddleJob, fixture.DeployRepo, second.DeployRepo, slices.Sorted(maps.Keys(keys)))
}

// TestTheFailedJobsReadReadsRunsByCompletion (dispatch://INFRA-2918, D9; R12): the statements
// pr-ci's items with sort=failures send read the runs by their completion: the one read of
// delivery_runs bounds the runs' completed_at by the 24 hours before the range's end, sent as its
// two ends, and the read of their jobs names those runs alone, every job of each, each of which
// completed in those 24 hours; the run that failed before them is named by no read.
func TestTheFailedJobsReadReadsRunsByCompletion(t *testing.T) {
	database := storetest.Open(t)
	log := &statementLog{}
	handler, _, deps, _ := newRegisteredTestServer(t, testServerOptions{store: storetest.TracedStore(t, database, log)})
	f := seedFailingRuns(t, database, deps)
	horizon := f.end.Add(-measures.FailingWindow)

	log.reset()
	items := evalGet[evalItemsResponse](t, handler, evalItemsPath(f.fixture.Project, evals.SlugPRCI)+"?"+f.query())
	if !slices.ContainsFunc(items.Items, func(row evalItemRow) bool { return row.RowKey == failingJobKey(f.fixture.DeployRepo, straddleJob) }) {
		t.Fatalf("the failing jobs %s do not name %s, so the read read no run of the 24 hours", mustJSON(t, items.Items), straddleJob)
	}
	var runReads, jobReads []ranStatement
	for _, statement := range log.statements() {
		switch {
		case strings.Contains(statement.sql, "from delivery_runs"):
			runReads = append(runReads, statement)
		case strings.Contains(statement.sql, "from delivery_run_jobs"):
			jobReads = append(jobReads, statement)
		}
	}
	if len(runReads) != 1 || len(jobReads) != 1 {
		t.Fatalf("the failures read sent %d reads of delivery_runs and %d of delivery_run_jobs, want one of each", len(runReads), len(jobReads))
	}
	runRead := runReads[0]
	sql := strings.Join(strings.Fields(runRead.sql), " ")
	bounds := map[string]time.Time{}
	for _, operator := range []string{">=", "<="} {
		match := regexp.MustCompile(`completed_at ` + operator + ` \$(\d+)`).FindStringSubmatch(sql)
		if match == nil {
			t.Fatalf("the read of the runs bounds no completed_at by %s: %s", operator, sql)
		}
		position, _ := strconv.Atoi(match[1])
		at, ok := runRead.args[position-1].(time.Time)
		if !ok {
			t.Fatalf("the read of the runs sends $%d as %T, want a time", position, runRead.args[position-1])
		}
		bounds[operator] = at
	}
	if !bounds[">="].Equal(horizon) || !bounds["<="].Equal(f.end) {
		t.Errorf("the read of the runs bounds their completion by %s..%s, want the 24 hours %s..%s", bounds[">="].Format(time.RFC3339),
			bounds["<="].Format(time.RFC3339), horizon.Format(time.RFC3339), f.end.Format(time.RFC3339))
	}
	jobRead := jobReads[0]
	if strings.Contains(jobRead.sql, "name =") || strings.Contains(jobRead.sql, "name <>") {
		t.Errorf("the read of the runs' jobs names jobs, want every job of each run: %s", jobRead.sql)
	}
	var named []int64
	for _, arg := range jobRead.args {
		if ids, ok := arg.([]int64); ok {
			named = append(named, ids...)
		}
	}
	if !slices.Contains(named, f.straddle.RunID) || slices.Contains(named, f.early.RunID) {
		t.Errorf("the read of the jobs names the runs %v, want run %d and not run %d", named, f.straddle.RunID, f.early.RunID)
	}
	rows, err := database.Pool.Query(t.Context(), `select run_id, completed_at from delivery_runs where repo = $1 and run_id = any($2)`, f.fixture.DeployRepo, named)
	if err != nil {
		t.Fatalf("read the runs the jobs' read names: %v", err)
	}
	var id int64
	var completed *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &completed}, func() error {
		if completed == nil || completed.Before(horizon) || completed.After(f.end) {
			t.Errorf("the read of the jobs names run %d, completed %v, outside the 24 hours %s..%s", id, completed, horizon.Format(time.RFC3339), f.end.Format(time.RFC3339))
		}
		return nil
	}); err != nil {
		t.Fatalf("read the runs the jobs' read names: %v", err)
	}
	t.Logf("the failures read sent %s with completion %s..%s, and read the jobs of %d runs", sql, horizon.Format(time.RFC3339), f.end.Format(time.RFC3339), len(named))
}

// TestTheUnownedP0CountAndItsIssuesFollowTheRepositoryFilter: unowned-p0's rows of a day are
// written for each repository its open P0 issues with no owner resolve to
// (evals.Store.WriteUnownedP0Day), beside the project's, so the eval's read and its items read
// filtered by repositories answer their count and their issues: the published default
// repository's, its own issues and those whose chain names no repository (stored under
// legionfacts.DefaultRepo), as every count's cut of it does; another's, its own issues and those
// below an issue of it; two repositories, both; one no issue resolves to, no data and no issue; and
// no filter, the project's. The repositories' rows partition the project's (checkDayRowPartition).
func TestTheUnownedP0CountAndItsIssuesFollowTheRepositoryFilter(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createTestProject(t, handler, "EVL")
	ctx := t.Context()
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("give EVL a default repository", `update projects set default_repo = 'acme/web' where key = 'EVL'`)
	exec("publish it", `insert into eval_days_defaults (project_key, repo) values ('EVL', 'acme/web')`)
	actor := `{"kind":"user","id":"alice"}`
	for _, issue := range []struct {
		number, priority int
		repo, parent     string
		claimed          bool
	}{
		{1, 0, "acme/web", "", false},
		{2, 0, "acme/api", "", false},
		{3, 2, "acme/api", "", false}, // no P0
		{4, 0, "", "EVL-3", false},    // acme/api, its parent's
		{5, 0, "", "", false},         // no repository: the published default's
		{6, 0, "", "", false},
		{7, 0, "acme/api", "", true}, // claimed
	} {
		var claimedBy any
		if issue.claimed {
			claimedBy = actor
		}
		exec(fmt.Sprintf("write EVL-%d", issue.number), `
			insert into issues (key, project_key, number, title, status, rank, priority, repo, parent_key, created_by, claimed_by, claimed_at)
			values ($1, 'EVL', $2, 'Incident', 'triage', $3, $4, nullif($5, ''), nullif($6, ''), $7, $8, case when $8::jsonb is null then null else now() end)
		`, fmt.Sprintf("EVL-%d", issue.number), issue.number, fmt.Sprintf("U%03d", issue.number), issue.priority, issue.repo, issue.parent, actor, claimedBy)
	}
	today := measures.Midnight(time.Now())
	if err := evals.New(database, deps.Docs, deps.Events, deps.Attributor).WriteUnownedP0Day(ctx, "EVL", today, testServerBuild); err != nil {
		t.Fatalf("write EVL's unowned-p0 rows of today: %v", err)
	}
	stated := func(value *float64) string {
		if value == nil {
			return "no data"
		}
		return fmt.Sprint(*value)
	}
	for _, read := range []struct {
		repos  []string
		want   string
		issues []string
	}{
		{nil, "value 5, n 5", []string{"EVL-1", "EVL-2", "EVL-4", "EVL-5", "EVL-6"}},
		{[]string{"acme/web"}, "value 3, n 3", []string{"EVL-1", "EVL-5", "EVL-6"}},
		{[]string{"acme/api"}, "value 2, n 2", []string{"EVL-2", "EVL-4"}},
		{[]string{"acme/web", "acme/api"}, "value 5, n 5", []string{"EVL-1", "EVL-2", "EVL-4", "EVL-5", "EVL-6"}},
		{[]string{"acme/other"}, "value no data, n no data", []string{}},
	} {
		query := url.Values{"from": {today.Format(time.RFC3339)}, "to": {today.AddDate(0, 0, 1).Format(time.RFC3339)}, "repo": read.repos}
		detail := evalGet[evalDetailResponse](t, handler, "/api/v1/projects/EVL/evals/"+evals.SlugUnownedP0+"?"+query.Encode())
		window := seriesValues(t, detail.Items[0].evalItem, evals.SeriesUnownedP0).Window
		if got := "value " + stated(window["value"]) + ", n " + stated(window["n"]); got != read.want {
			t.Errorf("the read with repo %v answers %s, want %s", read.repos, got, read.want)
		}
		query.Set("row_kind", evalRowIssue)
		items := evalGet[evalItemsResponse](t, handler, evalItemsPath("EVL", evals.SlugUnownedP0)+"?"+query.Encode())
		keys := []string{}
		for _, row := range items.Items {
			keys = append(keys, row.RowKey)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, read.issues) || items.Total != len(read.issues) {
			t.Errorf("the items read with repo %v answers %v of %d, want %v", read.repos, keys, items.Total, read.issues)
		}
	}
	seriesDays, repositoryRows := checkDayRowPartition(t, storedDayRows(t, database.Pool, "EVL"))
	if repositoryRows == 0 {
		t.Errorf("EVL holds no repository's day row over %d series days, so the partition checked none", seriesDays)
	}
	t.Logf("the partition holds over %d series days of EVL, %d repository rows", seriesDays, repositoryRows)
}
