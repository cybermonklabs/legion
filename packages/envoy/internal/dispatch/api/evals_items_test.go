package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

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
// an at on a midnight.
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
	}
	t.Logf("the straddling run %d started %s, completed %s; its at is %s", straddling.RunID,
		straddling.StartedAt.Format(time.RFC3339), straddling.CompletedAt.Format(time.RFC3339), straddleTo.Format(time.RFC3339))
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
// over its first golden window, and each other computed eval of the delivery history over seven of
// its days, answers at least one row of a kind other than `day`. The folds' own table is held to
// the catalogue when the evals package starts.
func TestEveryCatalogueSourceAnswersItsRows(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistoriesByIssue(t, database, deps, 14)
	legion := windows[0].Spec.Project
	ranges := map[string][2]time.Time{
		fixture.Project: {fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)},
		legion:          {windows[0].Window.From, windows[0].Window.To},
	}
	computed := 0
	for _, project := range []string{fixture.Project, legion} {
		list := evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?limit=200")
		for _, item := range list.Items {
			definition := item.Definition
			if !definition.Computed() || (definition.Kind == evals.KindLegion) != (project == legion) {
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

// TestEveryItemsSourceNarrowsToTheNamedSeries: `series` narrows every source of the items route to
// the series it names, as `row_kind` narrows them to a row kind (evalItemKind.series,
// legionItemSource). The table is the evals the fixture serves over the histories
// TestEveryCatalogueSourceAnswersItsRows reads, so it holds every source kind: each computed eval
// (a Legion eval's issues and legion-splitting's filings, a delivery eval's pull requests and
// deploy runs, a CI eval's runs and pr-ci's jobs, unowned-p0's issues, and each eval's day rows),
// pr-ci sorted by failures and by median (the failing jobs and the slowest jobs), and a logged eval
// of two series (its readings and their day rows), with a reading logged under pr-ci's slug. Each
// read answers rows with no series named, and none (total 0) for a series no row holds, alone and
// with a cut; and for each series its rows name (a day row's or a reading's, a job's job series),
// at most four of them, the read answers those rows alone: every day row and reading of that
// series and no other, and of the computed rows those of a kind that stands behind the series:
// every one of the kind where the kind names its series (evalComputedItems), a part of them where
// it narrows to the series itself, and of pr-ci's jobs the job the series names.
func TestEveryItemsSourceNarrowsToTheNamedSeries(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture, windows := seedEvalHistoriesByIssue(t, database, deps, 14)
	legion := windows[0].Spec.Project
	ranges := map[string][2]time.Time{
		fixture.Project: {fixture.From.AddDate(0, 0, 4), fixture.From.AddDate(0, 0, 11)},
		legion:          {windows[0].Window.From, windows[0].Window.To},
	}
	const logged = "items-series"
	at := ranges[fixture.Project][0].Add(time.Hour)
	for slug, rows := range map[string][]any{
		logged:         {reading("alpha", 1, at), reading("alpha", 2, at.Add(time.Hour)), reading("beta", 3, at)},
		evals.SlugPRCI: {reading(measures.SeriesMinutes, 4, at)},
	} {
		written := evalRequest(t, handler, http.MethodPost, "/api/v1/projects/"+fixture.Project+"/evals/"+slug+"/readings",
			map[string]any{"readings": rows}, "series-readings-"+slug)
		if written.Code != http.StatusCreated {
			t.Fatalf("log the readings of %s: status %d, body %s", slug, written.Code, written.Body.String())
		}
	}

	type itemsRead struct {
		project    string
		definition evals.Definition
		sort       string
	}
	var reads []itemsRead
	for _, project := range []string{fixture.Project, legion} {
		for _, item := range evalGet[evalListResponse](t, handler, "/api/v1/projects/"+project+"/evals?limit=200").Items {
			definition := item.Definition
			if definition.Computed() {
				if (definition.Kind == evals.KindLegion) != (project == legion) {
					continue
				}
			} else if definition.Slug != logged {
				continue
			}
			// Each series a kind stands behind is one its eval holds, so no kind stands behind a
			// series none of its rows is counted under.
			for _, kind := range evalComputedItems[definition.Source] {
				for _, name := range kind.series {
					if !slices.ContainsFunc(item.Series, func(series evals.SeriesValues) bool { return series.Series.Series == name }) {
						t.Errorf("%s's %s rows stand behind the series %s, which the eval does not list", definition.EvalID, kind.kind, name)
					}
				}
			}
			reads = append(reads, itemsRead{project: project, definition: definition})
			if definition.Slug == evals.SlugPRCI {
				reads = append(reads, itemsRead{project, definition, evalSortFailures}, itemsRead{project, definition, evalSortMedian})
			}
		}
	}
	if len(reads) < 3 {
		t.Fatalf("the fixture serves %d evals' reads, so the table holds no source kind of them", len(reads))
	}

	// seriesOf is the series a row is of where it names one: a day row's and a reading's own, and
	// a job's job series.
	seriesOf := func(row evalItemRow) string {
		switch row.RowKind {
		case evalRowDay, "sample":
			series, _ := row.Cols["series"].(string)
			return series
		case evalRowJob:
			return measures.JobSeriesPrefix + row.RowKey
		}
		return ""
	}
	keyOf := func(row evalItemRow) string { return row.RowKind + " " + row.RowKey + " " + row.Range }
	narrowed := 0
	for _, read := range reads {
		name := read.definition.EvalID
		query := url.Values{"from": {ranges[read.project][0].Format(time.RFC3339)}, "to": {ranges[read.project][1].Format(time.RFC3339)}}
		if read.sort != "" {
			name += " sort=" + read.sort
			query.Set("sort", read.sort)
		}
		path := evalItemsPath(read.project, read.definition.Slug)
		every := everyItem(t, handler, path, query)
		if len(every) == 0 {
			t.Errorf("%s answers no row over %s, so no series filter of it narrows anything", name, query.Encode())
			continue
		}
		for _, series := range []string{"no-such-series", "no-such-series:roots"} {
			filtered := maps.Clone(query)
			filtered.Set("series", series)
			if answer := evalGet[evalItemsResponse](t, handler, path+"?"+filtered.Encode()); answer.Total != 0 || len(answer.Items) != 0 {
				t.Errorf("%s with series %s, which no row holds, answers %d rows of a total %d (%s); want none",
					name, series, len(answer.Items), answer.Total, mustJSON(t, answer.Items[:min(len(answer.Items), 1)]))
			}
		}
		// held is each series a row of the read is of: the series a day row, a reading or a job
		// names, and those a kind of the read's rows stands behind.
		kinds := evalComputedItems[read.definition.Source]
		held := map[string]bool{}
		for _, row := range every {
			if series := seriesOf(row); series != "" {
				held[series] = true
			}
			if kind := slices.IndexFunc(kinds, func(kind evalItemKind) bool { return kind.kind == row.RowKind }); kind >= 0 {
				for _, series := range kinds[kind].series {
					held[series] = true
				}
			}
		}
		names := slices.Sorted(maps.Keys(held))
		if len(names) > 4 {
			names = append(names[:2:2], names[len(names)-2:]...)
		}
		if len(names) == 0 {
			t.Errorf("%s answers %d rows, none of which is of a series, so no series that holds rows is read", name, len(every))
		}
		for _, series := range names {
			// want is each row of the unfiltered read the series answers whole: its day rows and
			// readings, the job it names, and the rows of each kind that names the series.
			want := map[string]bool{}
			unfiltered := map[string]bool{}
			for _, row := range every {
				unfiltered[keyOf(row)] = true
				kind := slices.IndexFunc(kinds, func(kind evalItemKind) bool { return kind.kind == row.RowKind })
				if seriesOf(row) == series || (kind >= 0 && slices.Contains(kinds[kind].series, series)) {
					want[keyOf(row)] = true
				}
			}
			filtered := maps.Clone(query)
			filtered.Set("series", series)
			got := everyItem(t, handler, path, filtered)
			for _, row := range got {
				kind := slices.IndexFunc(kinds, func(kind evalItemKind) bool { return kind.kind == row.RowKind })
				switch {
				case !unfiltered[keyOf(row)]:
					t.Errorf("%s with series %s answers %s, which its read with no series does not", name, series, keyOf(row))
				case seriesOf(row) != "" && seriesOf(row) != series:
					t.Errorf("%s with series %s answers %s, a row of %s", name, series, keyOf(row), seriesOf(row))
				case seriesOf(row) == "" && (kind < 0 || !kinds[kind].standsBehind(series)):
					t.Errorf("%s with series %s answers %s, a %s row, which stands behind no such series", name, series, keyOf(row), row.RowKind)
				}
				delete(want, keyOf(row))
			}
			if len(want) > 0 {
				t.Errorf("%s with series %s leaves out %d rows of that series, among them %v", name, series, len(want), slices.Sorted(maps.Keys(want))[0])
			}
			narrowed++
			t.Logf("%s: %d rows with no series, %d with series %s", name, len(every), len(got), series)
		}
	}
	t.Logf("%d reads, each narrowed to series no row holds, and %d reads narrowed to a series their rows hold", len(reads), narrowed)
}

// everyItem is every row the items route answers at path for query, read a page of evalPageMax
// rows at a time.
func everyItem(t *testing.T, handler http.Handler, path string, query url.Values) []evalItemRow {
	t.Helper()
	var rows []evalItemRow
	for {
		page := maps.Clone(query)
		page.Set("limit", strconv.Itoa(evalPageMax))
		page.Set("offset", strconv.Itoa(len(rows)))
		answer := evalGet[evalItemsResponse](t, handler, path+"?"+page.Encode())
		rows = append(rows, answer.Items...)
		if len(answer.Items) == 0 || len(rows) >= answer.Total {
			return rows
		}
	}
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
		window := seriesValues(t, detail.Items[0], evals.SeriesUnownedP0).Window
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
