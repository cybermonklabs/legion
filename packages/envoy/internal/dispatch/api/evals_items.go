package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/evals/charts"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// GET /api/v1/projects/{key}/evals/{slug}/items: the rows behind an eval's numbers, every row in
// one shape whatever its kind (R7), so a page draws each row kind with one renderer and a client
// reads one type.
//
// A computed eval answers first the rows its numbers count, by its source (evalComputedItems):
// the issues a Legion eval counts, closed in the range, and before them the filings in it that
// legion-splitting counts (`issue`, from the stored filings and facts,
// legionfacts.FilingItemRows and legionfacts.ItemRows; an issue the range both filed and closed
// answers one row, its closed row, which states its filing too); the pull requests and deploy runs
// a delivery eval counts (`pull_request`, `deploy`, from the delivery reads the day rows are built
// with, delivery/measure_rows.go); the deploys and failures of the record a failure measure counts
// (`deploy`, `failure`, from production_events, as the failure rows count them,
// evals.FailureRecordIn), each with its relation to the measure's numbers; the runs a CI eval times
// (`run`) and, for pr-ci, each job's figures (`job`); and the open P0 issues with no owner
// (`issue`, delivery.ListUnownedP0). A delivery or CI row is answered where the project's day rows
// counted it (eval_day_members, evals.CountedIn), so the items are the rows the numbers fold, under
// the attribution the day rows made (evals' delivery_days.go, "Attribution"). Every eval then
// answers its readings (evals.ItemsIn, a reading logged under a computed eval's slug among them,
// which its value never folds, R3) and last its day rows (evals.EvalDaysIn, `day`). `source` keeps
// the rows of the sources it names alone: computed, the rows of a computed eval's source; readings;
// days, the day rows.
//
// A row of the compare range carries range `compare`. The page runs across the row kinds in the
// order above, the compare range's rows after the window's, and a row kind's rows in its own
// order. Before a range's rows the answer holds, whatever its page, the readings each step line of
// the eval's current charts starts that range at (stepBeforeRows, dispatch://INFRA-2918, D20 (a)),
// each marked before_window.
//
// One answer, one snapshot (dispatch://INFRA-2916, point 6): every read an answer makes, the
// project and the eval, the settings the rows stand built under, the day rows, the items they
// counted and the delivery, CI, Legion and readings rows behind them, runs in one read-only
// repeatable-read transaction (evals.Store.BeginSnapshot, readEvalItems), so a refresher pass
// that commits a day's rows, members and figures while the answer reads is in the whole answer
// or in none of it, and a page's items never mix the rows before it with those after it.

// evalItemRow is one row of an items answer, the same fields for every row kind (R7): its key and
// kind, its columns (cols, whose names are the row kind's own), when it was measured, the source
// it was read from, the address a reader follows to see it, the links it carries (a reading's
// own, [] for every other row), the range it is of, and whether it is a reading read before that
// range (before_window, absent for every other row).
//
// MeasuredAt is when the row's value was measured, the moment its eval dates it by and so the
// moment that puts it in its range, never when a row was last written: a reading's own
// measured_at; a pull request's merge, the day its delivery measures count it on; a deploy run's
// and a CI run's start, which the deploy and CI measures count it by (delivery.ReadDeploys and
// ReadCIRows read runs by start); a Legion issue's close and a filing's filing; a day row's day; a
// job's figures the newest day of the day rows they fold; a failing job its latest failure, which
// measures.FailingJobs counts by completion; and an open P0 issue with no owner the read itself,
// since the count is of the present. A reading before its range is measured before it.
type evalItemRow struct {
	RowKey       string         `json:"row_key"`
	RowKind      string         `json:"row_kind"`
	Cols         map[string]any `json:"cols"`
	MeasuredAt   time.Time      `json:"measured_at"`
	Source       string         `json:"source"`
	Href         string         `json:"href"`
	Links        []evals.Link   `json:"links"`
	Range        string         `json:"range" enum:"window,compare"`
	BeforeWindow bool           `json:"before_window,omitempty"`
}

// evalItemsResponse is GET /api/v1/projects/{key}/evals/{slug}/items: one page of the rows, with
// the window and the compare range they were read over.
type evalItemsResponse struct {
	Items   []evalItemRow `json:"items"`
	Total   int           `json:"total"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
	Window  evalRange     `json:"window"`
	Compare *evalRange    `json:"compare"`
}

// The row kinds the items route answers beside a reading's own (its row_kind, the caller's).
const (
	evalRowIssue       = legionfacts.ItemRowKind
	evalRowPullRequest = "pull_request"
	evalRowDeploy      = "deploy"
	evalRowFailure     = "failure"
	evalRowRun         = "run"
	evalRowJob         = "job"
	evalRowDay         = "day"
)

// The sources `source` names, each a part of the rows: a computed eval's rows its source counts
// (evalComputedItems), the eval's readings, and its day rows.
const (
	evalSourceComputed = "computed"
	evalSourceReadings = "readings"
	evalSourceDays     = "days"
)

// The two orders `sort` names, each of which answers pr-ci's job rows alone: the slowest jobs
// with the series' min_n timed results by their median over the range, slowest first, and the
// jobs that failed in the 24 hours before the range's end (measures.FailingJobs), most failures
// first.
const (
	evalSortMedian   = "median"
	evalSortFailures = "failures"
)

// evalItemsRequest is one items read: what every eval read reads of its query (the window and
// compare range, the repositories and the page), the filters, the sources it keeps (sources, nil
// for every source) and the order.
type evalItemsRequest struct {
	evalReadQuery
	series  string
	rowKind string
	row     string
	sources []string
	sort    string
}

// parseEvalItemsQuery reads the items route's query; a key the route does not take
// (undeclaredQueryKey), a malformed key and a source that is none of the three are 400
// INVALID_QUERY naming it.
func parseEvalItemsQuery(query url.Values) (evalItemsRequest, error) {
	read, err := parseEvalReadQuery(query, evalItemsQueryKeys)
	if err != nil {
		return evalItemsRequest{}, err
	}
	request := evalItemsRequest{
		evalReadQuery: read, series: query.Get("series"), rowKind: query.Get("row_kind"), row: query.Get("row"),
		sources: query["source"], sort: query.Get("sort"),
	}
	for _, source := range request.sources {
		if source != evalSourceComputed && source != evalSourceReadings && source != evalSourceDays {
			return evalItemsRequest{}, invalidEvalQuery(fmt.Errorf("source %q is none of %s, %s and %s",
				evals.BoundedField(source), evalSourceComputed, evalSourceReadings, evalSourceDays))
		}
	}
	if request.sort != "" && request.sort != evalSortMedian && request.sort != evalSortFailures {
		return evalItemsRequest{}, invalidEvalQuery(fmt.Errorf("sort %q is neither %s nor %s", request.sort, evalSortMedian, evalSortFailures))
	}
	return request, nil
}

// reads is whether request keeps the rows of source: every source where it names none.
func (request evalItemsRequest) reads(source string) bool {
	return len(request.sources) == 0 || slices.Contains(request.sources, source)
}

// keeps is whether request keeps the rows of kind: the kind row_kind names, every kind where it
// names none, and pr-ci's job rows alone where it names a sort.
func (request evalItemsRequest) keeps(kind string) bool {
	if request.sort != "" {
		return kind == evalRowJob
	}
	return request.rowKind == "" || request.rowKind == kind
}

// evalItemsRange is one range an items read reads: its days, the instants it was asked for,
// [start, end), and its name in the answer (evals.RangeWindow, evals.RangeCompare).
type evalItemsRange struct {
	days       evals.Range
	start, end time.Time
	label      string
}

// evalItemsRead is what a reader of one row kind reads for one range: the eval, the request and
// the range, the deploy repositories the rows stand recorded under (evals.DeployReposIn), the
// reads every range of the request shares (evalRequestReads), the reads the range's readers share
// (evalRangeReads), and tx, the answer's one snapshot, which every read of the answer reads in.
type evalItemsRead struct {
	project     string
	entry       evals.Entry
	request     evalItemsRequest
	repos       []string // stored spellings for filters; request.repos is the spelling the caller named
	rng         evalItemsRange
	deployRepos *evals.DeployRepos
	once        *evalRequestReads
	shared      *evalRangeReads
	tx          pgx.Tx
}

// evalRequestReads is the reads of an items request its readers keep for every range, each made
// once a request, since no range moves them: the rules of the deploy repositories its eval reads
// (repoRules), whose delivery and CI rows each is read under, and the record behind a failure
// measure (failureItemsOf), which each range filters.
type evalRequestReads struct {
	rules        []delivery.DeliverySettings
	rulesRead    bool
	failures     *evals.FailureRecord
	failuresRead bool
}

// evalRangeReads is the reads of one range its readers keep, each made once a range: the items
// each family's day rows of the range counted (counted), the eval's day rows of the range, which
// pr-ci's jobs and its day rows both read, and the record's rows behind a failure measure, which
// its deploys and its failures both read. failing is the pull-request checks runs pr-ci's failures
// list of the range was read from (failingJobRows), which the items read answers beside its body
// for the window (evalItemsAnswer).
type evalRangeReads struct {
	counted      map[string]map[string]bool
	days         []evals.DayRow
	read         bool
	failures     []evals.FailureItem
	failuresRead bool
	failing      []delivery.DeliveryRun
}

// evalItemKind is one row kind a computed eval answers, and its reader: read answers the kind's
// rows whole, or source, set in its place, answers them a page at a time.
type evalItemKind struct {
	kind   string
	read   func(s *server, ctx context.Context, read evalItemsRead) ([]evalItemRow, error)
	source func(s *server, ctx context.Context, read evalItemsRead) (evalItemSource, error)
}

var (
	pullRequestItems    = evalItemKind{kind: evalRowPullRequest, read: (*server).pullRequestItemRows}
	deployItems         = evalItemKind{kind: evalRowDeploy, read: (*server).deployItemRows}
	legionItems         = evalItemKind{kind: evalRowIssue, source: (*server).legionItemSource}
	failureDeployItems  = evalItemKind{kind: evalRowDeploy, read: (*server).failureDeployItemRows}
	failureRecordItems  = evalItemKind{kind: evalRowFailure, read: (*server).failureItemRows}
	failureMeasureItems = []evalItemKind{failureDeployItems, failureRecordItems}
)

// evalComputedItems is the row kinds each computed eval answers beside its readings and its day
// rows, by the source it names: the rows its fold counts. A failure is a fact about its deploy
// (dispatch://INFRA-978, D7), so the three failure measures answer the deploys of the record they
// count and the failures on them.
var evalComputedItems = map[string][]evalItemKind{
	evals.SourceFoldDeployFrequency:  {deployItems},
	evals.SourceFoldDeployRunSuccess: {deployItems},
	evals.SourceFoldLeadTime:         {pullRequestItems},
	evals.SourceFoldReworkShare:      {pullRequestItems},
	evals.SourceFoldChangeFailRate:   failureMeasureItems,
	evals.SourceFoldRecovery:         failureMeasureItems,
	evals.SourceFoldReworkRate:       failureMeasureItems,
	evals.SourceFoldTiming:           {{kind: evalRowRun, read: (*server).runItemRows}, {kind: evalRowJob, read: (*server).jobItemRows}},
	evals.SourceListUnownedP0:        {{kind: evalRowIssue, read: (*server).unownedItemRows}},
	evals.SourceFoldWorkTime:         {legionItems},
	evals.SourceFoldPhaseTime:        {legionItems},
	evals.SourceFoldIdleTime:         {legionItems},
	evals.SourceFoldSlotWait:         {legionItems},
	evals.SourceFoldRework:           {legionItems},
	evals.SourceFoldSplitting:        {legionItems},
}

// evalItemSource is one row kind of one range as the page reads it: the rows from skip on, at
// most want of them (none when want is 0), and how many it holds in all.
type evalItemSource func(ctx context.Context, skip, want int) ([]evalItemRow, int, error)

// listedSource is rows, already read whole, as a source.
func listedSource(rows []evalItemRow) evalItemSource {
	return func(_ context.Context, skip, want int) ([]evalItemRow, int, error) {
		start := min(skip, len(rows))
		return rows[start:min(start+want, len(rows))], len(rows), nil
	}
}

// evalItemsPart is one part of an items answer, in the order the answer runs across them: a source
// the page reads (page), or, where page is nil, rows the answer holds whatever its page, outside
// its limit, its offset and its total (pinned): a range's readings before it (stepBeforeRows).
type evalItemsPart struct {
	page   evalItemSource
	pinned []evalItemRow
}

// listProjectEvalItems answers GET /api/v1/projects/{key}/evals/{slug}/items: one page of the rows
// behind the eval's numbers (readProjectEvalItems).
func (s *server) listProjectEvalItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	answer, err := s.readProjectEvalItems(r.Context(), r.URL.Query(), r.PathValue("key"), r.PathValue("slug"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer.body)
}

// evalItemsAnswer is one items read: the body of GET /api/v1/projects/{key}/evals/{slug}/items, and
// the pull-request checks runs its window's failures list was read from, none where it read no
// such list. The live hub's items view expires by those runs as the Pipeline's failing-jobs table
// does (readEvalItemsView, failingJobsNextExpiry).
type evalItemsAnswer struct {
	body    evalItemsResponse
	failing []delivery.DeliveryRun
}

// readProjectEvalItems is the body of GET /api/v1/projects/{key}/evals/{slug}/items for query: one
// page of the rows behind the eval's numbers over the window and the compare range, filtered by
// series, row_kind, row and repo, and for pr-ci's jobs ranked by sort (readEvalItems), after the
// query is read (parseEvalItemsQuery). 404 PROJECT_NOT_FOUND and EVAL_NOT_FOUND as the detail read
// answers them. The live hub's items view reads its body through this too, so the two answer one
// query alike.
func (s *server) readProjectEvalItems(ctx context.Context, query url.Values, project, slug string) (evalItemsAnswer, error) {
	request, err := parseEvalItemsQuery(query)
	if err != nil {
		return evalItemsAnswer{}, err
	}
	return s.readEvalItems(ctx, project, slug, request)
}

// readEvalItems is one items answer of slug's eval of project for request, read in one snapshot
// (evals.Store.BeginSnapshot): the project (404 PROJECT_NOT_FOUND where no row holds it), the eval
// (404 EVAL_NOT_FOUND), the settings the rows stand built under, and every source's page, in the
// order the page runs across them, with each range's readings before it first in the range. The
// transaction holds ctx's connection mark, so a read of the pool made under it is refused
// (store.ErrNestedAcquire). A source's rows are built as it reads them: the readings' page and the
// Legion issues' repositories and titles are read for the rows the page holds of them, which the
// totals of the sources before them place, so no read can wait for the builds. It commits before
// the answer is encoded.
func (s *server) readEvalItems(ctx context.Context, project, slug string, request evalItemsRequest) (evalItemsAnswer, error) {
	evalID := evals.EvalID(project, slug)
	tx, err := s.registry().BeginSnapshot(ctx)
	if err != nil {
		return evalItemsAnswer{}, fmt.Errorf("begin the read of the items of eval %s: %w", evalID, err)
	}
	defer tx.Rollback(ctx)
	if _, err := s.loadProject(ctx, tx, project); err != nil {
		return evalItemsAnswer{}, err
	}
	entry, found, err := evals.EvalIn(ctx, tx, evalID)
	if err != nil {
		return evalItemsAnswer{}, err
	}
	if !found {
		return evalItemsAnswer{}, evalNotFound(project, slug)
	}
	window := &evalRangeReads{}
	parts, err := s.evalItemSources(ctx, tx, project, entry, request, window)
	if err != nil {
		return evalItemsAnswer{}, err
	}
	answer := evalItemsResponse{
		Items: []evalItemRow{}, Limit: request.limit, Offset: request.offset,
		Window: newEvalRange(request.ranges.window), Compare: evalCompareRange(request.ranges.compare),
	}
	skip, want := request.offset, request.limit
	for _, part := range parts {
		if part.page == nil {
			answer.Items = append(answer.Items, part.pinned...)
			continue
		}
		rows, total, err := part.page(ctx, skip, want)
		if err != nil {
			return evalItemsAnswer{}, err
		}
		answer.Total += total
		skip = max(0, skip-total)
		rows = rows[:min(len(rows), want)]
		answer.Items, want = append(answer.Items, rows...), want-len(rows)
	}
	if err := tx.Commit(ctx); err != nil {
		return evalItemsAnswer{}, fmt.Errorf("end the read of the items of eval %s: %w", evalID, err)
	}
	return evalItemsAnswer{body: answer, failing: window.failing}, nil
}

// evalItemSources is the parts of one items read in tx, the answer's snapshot, in the order its
// page runs across them: for the window and then the compare range, first the readings each of
// the eval's step lines starts the range at (stepBeforeRows), then the computed rows of each kind
// the eval's source answers (evalComputedItems), its readings and its day rows, each kept where
// row_kind names it or names none and where source names its source or names none. A request
// naming sort reads pr-ci's job rows alone. The window's readers share window, the caller's, which
// holds what they read once they have read it.
func (s *server) evalItemSources(ctx context.Context, tx pgx.Tx, project string, entry evals.Entry, request evalItemsRequest, window *evalRangeReads) ([]evalItemsPart, error) {
	repos, err := evals.StoredRepos(ctx, tx, []string{project}, request.repos)
	if err != nil {
		return nil, err
	}
	ranges := []evalItemsRange{{days: request.ranges.window, start: request.ranges.from, end: request.ranges.to, label: evals.RangeWindow}}
	if request.ranges.compare != nil {
		ranges = append(ranges, evalItemsRange{
			days: *request.ranges.compare, start: request.ranges.compareFrom, end: request.ranges.compareTo, label: evals.RangeCompare,
		})
	}
	deployRepos, err := evals.DeployReposIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	readings := request.sort == "" && request.reads(evalSourceReadings)
	var before [][]evalItemRow
	if readings {
		if before, err = stepBeforeRows(ctx, tx, entry.Definition.EvalID, request, repos, ranges); err != nil {
			return nil, err
		}
	}
	definition := entry.Definition
	var parts []evalItemsPart
	once := &evalRequestReads{}
	for at, rng := range ranges {
		shared := window
		if rng.label != evals.RangeWindow {
			shared = &evalRangeReads{}
		}
		read := evalItemsRead{project: project, entry: entry, request: request, repos: repos, rng: rng, deployRepos: deployRepos, once: once, shared: shared, tx: tx}
		if readings && len(before[at]) > 0 {
			parts = append(parts, evalItemsPart{pinned: before[at]})
		}
		if definition.Computed() && request.reads(evalSourceComputed) {
			for _, kind := range evalComputedItems[definition.Source] {
				if !request.keeps(kind.kind) {
					continue
				}
				if kind.source != nil {
					source, err := kind.source(s, ctx, read)
					if err != nil {
						return nil, err
					}
					parts = append(parts, evalItemsPart{page: source})
					continue
				}
				rows, err := kind.read(s, ctx, read)
				if err != nil {
					return nil, err
				}
				if request.row != "" {
					rows = slices.DeleteFunc(rows, func(row evalItemRow) bool { return row.RowKey != request.row })
				}
				parts = append(parts, evalItemsPart{page: listedSource(rows)})
			}
		}
		if request.sort != "" {
			continue
		}
		// A reading's row kind is the one its writer gave it, so row_kind keeps the readings of that
		// kind, as the store reads them.
		if readings {
			parts = append(parts, evalItemsPart{page: func(ctx context.Context, skip, want int) ([]evalItemRow, int, error) {
				page, err := evals.ItemsIn(ctx, tx, definition.EvalID, request.series, request.rowKind, request.row, repos, rng.days, max(want, 1), skip)
				if err != nil {
					return nil, 0, err
				}
				rows := make([]evalItemRow, 0, len(page.Items))
				for _, reading := range page.Items[:min(len(page.Items), want)] {
					rows = append(rows, readingItemRow(reading, rng.label))
				}
				return rows, page.Total, nil
			}})
		}
		if request.keeps(evalRowDay) && request.reads(evalSourceDays) {
			days, err := s.evalDaysOf(ctx, read)
			if err != nil {
				return nil, err
			}
			rows := make([]evalItemRow, 0, len(days))
			for _, day := range days {
				row := dayItemRow(project, definition.Slug, day, rng.label)
				if request.row == "" || row.RowKey == request.row {
					rows = append(rows, row)
				}
			}
			parts = append(parts, evalItemsPart{page: listedSource(rows)})
		}
	}
	return parts, nil
}

// stepBeforeRows is, for each of ranges, the readings of evalID's step series that its step lines
// start the range at (evals.BeforeWindowIn): of each series a `curve: "step"` chart of the eval's
// current charts draws over measured time (evals.StepsIn), the one series request names where it
// names one, the newest standing reading of each group of the series before the range's first day,
// a group being the series and its col, and further its row key or its row group where the chart
// draws a line a row or a row group (dispatch://INFRA-2918, D20 (a)). Each is read through the
// filters the range's readings are (row kind, row and repositories), and answered as its range's
// row, marked before_window. A series no step chart draws has none.
func stepBeforeRows(ctx context.Context, tx pgx.Tx, evalID string, request evalItemsRequest, repos []string, ranges []evalItemsRange) ([][]evalItemRow, error) {
	rows := make([][]evalItemRow, len(ranges))
	drawn, err := evals.StepsIn(ctx, tx, []string{evalID})
	if err != nil {
		return nil, err
	}
	steps := drawn[evalID]
	if request.series != "" {
		steps = slices.DeleteFunc(steps, func(step charts.Step) bool { return step.Series != request.series })
	}
	if len(steps) == 0 {
		return rows, nil
	}
	befores := make([]time.Time, len(ranges))
	for at, rng := range ranges {
		befores[at] = rng.days.From
	}
	readings, err := evals.BeforeWindowIn(ctx, tx, evalID, steps, request.rowKind, request.row, repos, befores)
	if err != nil {
		return nil, err
	}
	for at, rng := range ranges {
		for _, reading := range readings[at] {
			row := readingItemRow(reading, rng.label)
			row.BeforeWindow = true
			rows[at] = append(rows[at], row)
		}
	}
	return rows, nil
}

// evalDaysOf is the eval's day rows of read's range (evals.EvalDaysIn, of the request's series
// and repositories, and of the deploy repositories recorded with the rows where the eval reads
// them), read once a range (evalRangeReads): the day-row source and pr-ci's job rows both read
// them.
func (s *server) evalDaysOf(ctx context.Context, read evalItemsRead) ([]evals.DayRow, error) {
	if !read.shared.read {
		days, err := evals.EvalDaysIn(ctx, read.tx, read.entry.Definition, read.deployRepos, read.request.series, read.repos, read.rng.days)
		if err != nil {
			return nil, err
		}
		read.shared.days, read.shared.read = days, true
	}
	return read.shared.days, nil
}

// repoRules is the rule of each deploy repository read's eval reads (evals.DeployRepoOf), as its
// rows stand built under it (evals.BuiltSettingsIn), its DeployRepo that repository, in their
// order: the rule the day builder counted that repository's rows by, whichever project's source
// reads it, so a project with no source of its own reads the rows its pull requests' issues count
// for it. An eval no deploy repository moves, and an events repository, whose posted deploys no
// rule judges, have none. They are read once a request (evalRequestReads), in one read of the
// settings and the recorded rules.
func (s *server) repoRules(ctx context.Context, read evalItemsRead) ([]delivery.DeliverySettings, error) {
	if read.once.rulesRead {
		return read.once.rules, nil
	}
	var repos []string
	for _, repo := range evals.DeployRepoOf(read.entry.Definition, read.deployRepos) {
		if repo != "" && !evals.IsEventsRepo(repo) {
			repos = append(repos, repo)
		}
	}
	built, err := evals.BuiltSettingsIn(ctx, read.tx, repos)
	if err != nil {
		return nil, err
	}
	var rules []delivery.DeliverySettings
	for i, rule := range built {
		if rule != nil {
			one := *rule
			one.DeployRepo = repos[i]
			rules = append(rules, one)
		}
	}
	read.once.rules, read.once.rulesRead = rules, true
	return rules, nil
}

// readingItemRow is a stored reading as an items row: its own row key and row kind, its columns
// the reading's every value, measured when it was, its own source and links, and as its address
// the first of its links, "" for a reading with none.
func readingItemRow(reading evals.Reading, label string) evalItemRow {
	href := ""
	if len(reading.Links) > 0 {
		href = reading.Links[0].Href
	}
	return evalItemRow{
		RowKey:  reading.RowKey,
		RowKind: reading.RowKind,
		Cols: map[string]any{
			"id": reading.ID, "series": reading.Series, "col": reading.Col, "value": reading.Value,
			"value_absent_reason": reading.ValueAbsentReason, "unit": reading.Unit, "n": reading.N, "n_unit": reading.NUnit,
			"ci_low": reading.CILow, "ci_high": reading.CIHigh, "ci_level": reading.CILevel, "attributes": reading.Attributes,
			"note": reading.Note, "replaces": reading.Replaces, "recorded_at": reading.RecordedAt, "recorded_by": reading.RecordedBy,
		},
		MeasuredAt: reading.MeasuredAt,
		Source:     reading.Source,
		Href:       href,
		Links:      reading.Links,
		Range:      label,
	}
}

// dayItemRow is a stored day row as an items row: keyed `<series>@<day>`, and `@<repo>` after
// that for a repository's row; its columns the row's repository, listing and build and its parts
// but its sample list and the write that stored its last value (last_recorded_at, last_reading);
// measured on its day, the day its parts are of; and as its address the items of that day of that
// series, of that repository for a repository's row.
func dayItemRow(project, slug string, day evals.DayRow, label string) evalItemRow {
	date := day.Day.Format(time.DateOnly)
	key := day.Series + "@" + date
	if day.Repo != "" {
		key += "@" + day.Repo
	}
	behind := url.Values{
		"from":   {date + "T00:00:00Z"},
		"to":     {day.Day.AddDate(0, 0, 1).Format(time.DateOnly) + "T00:00:00Z"},
		"series": {day.Series},
	}
	if day.Repo != "" {
		behind["repo"] = []string{day.Repo}
	}
	return evalItemRow{
		RowKey:  key,
		RowKind: evalRowDay,
		Cols: map[string]any{
			"series": day.Series, "repo": day.Repo, "day": date, "listed": day.Listed, "n": day.N, "sum": day.Sum,
			"num": day.Num, "den": day.Den, "max": day.Max, "last_value": day.LastValue, "last_at": day.LastAt,
			"counts": evals.NonNilCounts(day.Counts), "via_repository": day.ViaRepository, "built_by": day.BuiltBy,
		},
		MeasuredAt: day.Day,
		Source:     "eval_days",
		Href:       evalItemsPath(project, slug) + "?" + behind.Encode(),
		Links:      []evals.Link{},
		Range:      label,
	}
}

// evalItemsPath is the items route of one eval.
func evalItemsPath(project, slug string) string {
	return "/api/v1/projects/" + url.PathEscape(project) + "/evals/" + url.PathEscape(slug) + "/items"
}

// legionItemSource is the issues a Legion eval counts in the range: the issues of the project that
// closed in it (legionfacts.ReadClosedIn), and for legion-splitting the filings its sessions made
// in it (legionfacts.ReadFilingsIn), the filings first; a `filed:<class>` series reads the filings
// of that class alone, and any other series of legion-splitting the closed issues alone. A series'
// cut keeps the issues of that cut, by the stored root (metrics.InCut), and row keeps the issue it
// names. An issue the range both filed and closed answers one row (R24), its closed row, which
// states its filing beside its facts (legionfacts.ItemRows), so its filing makes no row of its
// own and each number the rows stand behind still finds its issue once. The repository each issue
// resolves to now (legionfacts.ReadRepos, the repository the day rows cut by) is what a closed
// issue's row shows and what repo keeps the issues by, so a row never shows one repository while
// it is kept by another. Each row carries its issue's title and priority
// (legionfacts.WithIssues), and its facts as of the request (legionfacts.Row.At).
//
// The source answers a page at a time: the stored rows of the range are read whole, since every
// one of them is counted in the total, but the repositories are resolved (a walk of each issue's
// parent chain) and the titles read for the page's issues alone, and the page's rows alone are
// built, where a read of every page made all three over every issue of the range. A repo filter
// resolves every issue's repository, which the total then counts by.
func (s *server) legionItemSource(ctx context.Context, read evalItemsRead) (evalItemSource, error) {
	base, cut := metrics.SplitCutSeries(read.request.series)
	class, filedSeries := strings.CutPrefix(base, metrics.FilingSeriesPrefix)
	splitting := read.entry.Definition.Slug == evals.SlugLegionSplitting
	var filings []legionfacts.Filing
	var closed []legionfacts.Row
	if splitting && (read.request.series == "" || filedSeries) {
		filed, err := legionfacts.ReadFilingsIn(ctx, read.tx, read.project, read.rng.days.From, read.rng.days.To)
		if err != nil {
			return nil, err
		}
		filings = slices.DeleteFunc(filed, func(filing legionfacts.Filing) bool {
			return (filedSeries && filing.Class != class) || !metrics.InCut(cut, filing.Root == filing.Key) ||
				(read.request.row != "" && filing.Key != read.request.row)
		})
	}
	if !filedSeries {
		stored, err := legionfacts.ReadClosedIn(ctx, read.tx, read.project, read.rng.days.From, read.rng.days.To, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		closed = slices.DeleteFunc(stored, func(row legionfacts.Row) bool {
			return !metrics.InCut(cut, row.Root) || (read.request.row != "" && row.Issue != read.request.row)
		})
	}
	// closedFilings is the filing of each issue the range both filed and closed, by issue: its
	// closed row states it, and the filings' rows leave it out. Only a read of every series of
	// legion-splitting reads both.
	var closedFilings map[string]legionfacts.Filing
	if len(filings) > 0 && len(closed) > 0 {
		closedFilings = make(map[string]legionfacts.Filing)
		closedIssues := make(map[string]bool, len(closed))
		for _, row := range closed {
			closedIssues[row.Issue] = true
		}
		filings = slices.DeleteFunc(filings, func(filing legionfacts.Filing) bool {
			if closedIssues[filing.Key] {
				closedFilings[filing.Key] = filing
			}
			return closedIssues[filing.Key]
		})
	}
	keysOf := func(filings []legionfacts.Filing, closed []legionfacts.Row) []string {
		keys := make([]string, 0, len(filings)+len(closed))
		for _, filing := range filings {
			keys = append(keys, filing.Key)
		}
		for _, row := range closed {
			keys = append(keys, row.Issue)
		}
		return keys
	}
	var repos map[string]string
	if len(read.repos) > 0 {
		resolved, err := issueReposIn(ctx, read.tx, read.project, keysOf(filings, closed))
		if err != nil {
			return nil, err
		}
		repos = resolved
		filings = slices.DeleteFunc(filings, func(filing legionfacts.Filing) bool { return !slices.Contains(read.repos, repos[filing.Key]) })
		closed = slices.DeleteFunc(closed, func(row legionfacts.Row) bool { return !slices.Contains(read.repos, repos[row.Issue]) })
	}
	total := len(filings) + len(closed)
	return func(ctx context.Context, skip, want int) ([]evalItemRow, int, error) {
		start := min(skip, total)
		end := start + min(want, total-start)
		pageFilings := filings[min(start, len(filings)):min(end, len(filings))]
		pageClosed := closed[max(start-len(filings), 0):max(end-len(filings), 0)]
		keys := keysOf(pageFilings, pageClosed)
		pageRepos := repos
		if pageRepos == nil {
			resolved, err := issueReposIn(ctx, read.tx, read.project, keys)
			if err != nil {
				return nil, 0, err
			}
			pageRepos = resolved
		}
		issues, err := legionfacts.ReadItemIssues(ctx, read.tx, keys)
		if err != nil {
			return nil, 0, err
		}
		items := append(legionfacts.FilingItemRows(pageFilings), legionfacts.ItemRows(pageClosed, pageRepos, closedFilings)...)
		rows := make([]evalItemRow, len(items))
		for index, item := range legionfacts.WithIssues(items, issues) {
			rows[index] = evalItemRow{
				RowKey: item.RowKey, RowKind: item.RowKind, Cols: item.Columns, MeasuredAt: item.MeasuredAt,
				Source: item.Source, Href: item.Href, Links: []evals.Link{}, Range: read.rng.label,
			}
		}
		return rows, total, nil
	}, nil
}

// issueReposIn is the repository each of keys, issues of project, resolves to now, read in tx: the
// one the day rows cut it by (legionfacts.ReadRepos), where legionfacts.DefaultRepo, which a row of
// an issue that owns no repository is stored under, stands for the project's published default
// repository (evals.PublishedDefaultIn), so the items read shows and filters that repository and a
// default move moves the cut and rewrites no row. An issue that resolves to none, or to the default
// while the project publishes none, has no entry.
func issueReposIn(ctx context.Context, tx pgx.Tx, project string, keys []string) (map[string]string, error) {
	repos, err := legionfacts.ReadRepos(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	published, err := evals.PublishedDefaultIn(ctx, tx, project)
	if err != nil {
		return nil, err
	}
	for key, repo := range repos {
		if repo == legionfacts.DefaultRepo {
			if published == "" {
				delete(repos, key)
				continue
			}
			repos[key] = published
		}
	}
	return repos, nil
}

// unownedItemRows is unowned-p0's issues: the project's open P0 issues with no owner now
// (delivery.ListUnownedP0), the count its day rows record, answered in the window alone since
// the count is of the present; where the request names repositories, those of the issues that
// resolve to one of them (issueReposIn), which those repositories' rows count
// (evals.Store.WriteUnownedP0Day).
func (s *server) unownedItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.rng.label != evals.RangeWindow {
		return nil, nil
	}
	issues, err := delivery.ListUnownedP0(ctx, read.tx, read.project)
	if err != nil {
		return nil, err
	}
	if len(read.repos) > 0 {
		keys := make([]string, len(issues))
		for index, issue := range issues {
			keys[index] = issue.Key
		}
		repos, err := issueReposIn(ctx, read.tx, read.project, keys)
		if err != nil {
			return nil, err
		}
		issues = slices.DeleteFunc(issues, func(issue delivery.UnownedIssue) bool { return !slices.Contains(read.repos, repos[issue.Key]) })
	}
	now := time.Now().UTC()
	rows := make([]evalItemRow, len(issues))
	for index, issue := range issues {
		rows[index] = evalItemRow{
			RowKey: issue.Key, RowKind: evalRowIssue, Cols: map[string]any{legionfacts.ColumnTitle: issue.Title},
			MeasuredAt: now, Source: "delivery.ListUnownedP0", Href: "/issues/" + issue.Key, Links: []evals.Link{},
			Range: read.rng.label,
		}
	}
	return rows, nil
}

// counted is the row keys of the items family's day rows of the project counted on the days of
// days, under the deploy repositories the eval's read reads (evals.DeployRepoOf), those of the
// repositories the request names where it names any. Those of the range's own days are read once
// a range (evalRangeReads), for every reader of the range that reads them.
func (s *server) counted(ctx context.Context, read evalItemsRead, family string, days evals.Range) (map[string]bool, error) {
	ranged := days == read.rng.days
	if keys, held := read.shared.counted[family]; ranged && held {
		return keys, nil
	}
	items, err := evals.CountedIn(ctx, read.tx, read.project, family, evals.DeployRepoOf(read.entry.Definition, read.deployRepos), read.repos, days)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(items))
	for _, item := range items {
		keys[item.RowKey] = true
	}
	if ranged {
		if read.shared.counted == nil {
			read.shared.counted = map[string]map[string]bool{}
		}
		read.shared.counted[family] = keys
	}
	return keys, nil
}

// pullRequestItemRows is the pull requests a delivery eval's day rows of the range counted: those
// merged in it (delivery.ListPullRequestsInWindow, as the day builder reads them), each with the
// first deploy that ships it among the deploy repositories the eval reads, each under its own rule
// (repoRules, the apply runs delivery.ReadApplyRows reads of the range, as delivery.ReadMergedRows
// reads them beside the same pull requests, and delivery.ContainingRun), none for an eval no
// deploy repository moves, and its four lead times, each built by its eval's day builder over the
// one pull request (measures.MergeToProductionDay and the rest), so a row's minutes are its part
// of that eval's day.
func (s *server) pullRequestItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	members, err := s.counted(ctx, read, evals.KindDelivery, read.rng.days)
	if err != nil {
		return nil, err
	}
	rules, err := s.repoRules(ctx, read)
	if err != nil {
		return nil, err
	}
	prs, err := delivery.ListPullRequestsInWindow(ctx, read.tx, read.rng.days.From, read.rng.days.To)
	if err != nil {
		return nil, err
	}
	shipping := map[string]*delivery.Apply{}
	for _, rule := range rules {
		applyRows, err := delivery.ReadApplyRows(ctx, read.tx, rule, read.rng.days.From)
		if err != nil {
			return nil, err
		}
		applies := delivery.ProductionApplies(applyRows.Runs, applyRows.Jobs, rule.Production()).Applies
		for _, pr := range prs {
			key := evals.PullRequestMember(pr.Repo, pr.Number)
			if apply := delivery.ContainingRun(pr, rule.DeployRepo, applies); apply != nil && (shipping[key] == nil || apply.CompletedAt.Before(shipping[key].CompletedAt)) {
				shipping[key] = apply
			}
		}
	}
	var rows []evalItemRow
	for _, pr := range prs {
		if pr.CreatedAt == nil || pr.MergedAt == nil || !members[evals.PullRequestMember(pr.Repo, pr.Number)] {
			continue
		}
		measured := measures.PROf(pr, shipping[evals.PullRequestMember(pr.Repo, pr.Number)])
		day, one := measures.Midnight(*pr.MergedAt), []measures.PR{measured}
		rows = append(rows, evalItemRow{
			RowKey:  measured.ID,
			RowKind: evalRowPullRequest,
			Cols: map[string]any{
				"repo": pr.Repo, "number": pr.Number, "title": pr.Title, "author": pr.Author, "issue": pr.IssueKey,
				"created_at": pr.CreatedAt, "merged_at": pr.MergedAt, "first_commit_at": pr.FirstCommitAt,
				"rework": pr.Rework, "deploy_run_id": measured.DeployRunID, "deployed_at": measured.DeployedAt,
				"merge_to_production_minutes":        sampleOf(measures.MergeToProductionDay(day, one)),
				"first_commit_to_production_minutes": sampleOf(measures.FirstCommitToProductionDay(day, one)),
				"open_to_production_minutes":         sampleOf(measures.OpenToProductionDay(day, one)),
				"open_to_merge_minutes":              sampleOf(measures.OpenToMergeDay(day, one)),
			},
			MeasuredAt: *pr.MergedAt,
			Source:     "delivery.ListPullRequestsInWindow",
			Href:       pr.URL,
			Links:      []evals.Link{},
			Range:      read.rng.label,
		})
	}
	return rows, nil
}

// sampleOf is the one value a day part built over one item holds, nil where the item added none.
func sampleOf(parts []measures.DayPart) *float64 {
	for _, part := range parts {
		if len(part.Samples) > 0 {
			return new(part.Samples[0])
		}
	}
	return nil
}

// deployItemRows is the deploy runs a delivery eval's day rows of the range counted: each deploy
// repository's deploy workflow runs on main started in it (repoRules, delivery.ReadDeploys under
// that repository's rule, as the day builder reads them), each with whether it reached production
// and how many pull requests it ships (delivery.Deploys.ShippedBy, the rule with_prs counts by,
// over delivery.ReadMergedRows' rows: the pull requests merged in the range, read once, and the
// repository's apply runs, delivery.ReadApplyRows).
func (s *server) deployItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	members, err := s.counted(ctx, read, evals.KindDelivery, read.rng.days)
	if err != nil {
		return nil, err
	}
	rules, err := s.repoRules(ctx, read)
	if err != nil || len(rules) == 0 {
		return nil, err
	}
	prs, err := delivery.ListPullRequestsInWindow(ctx, read.tx, read.rng.days.From, read.rng.days.To)
	if err != nil {
		return nil, err
	}
	var rows []evalItemRow
	for _, rule := range rules {
		deploys, err := delivery.ReadDeploys(ctx, read.tx, rule, read.rng.days.From, read.rng.days.To)
		if err != nil {
			return nil, err
		}
		applies, err := delivery.ReadApplyRows(ctx, read.tx, rule, read.rng.days.From)
		if err != nil {
			return nil, err
		}
		shipped := deploys.ShippedBy(delivery.MergedRows{PullRequests: prs, ApplyRuns: applies.Runs, ApplyJobs: applies.Jobs}, rule)
		reached := map[int64]bool{}
		for _, apply := range deploys.Applies {
			reached[apply.RunID] = true
		}
		for _, run := range deploys.Started.Runs {
			if !members[evals.RunMember(run.Repo, run.RunID)] {
				continue
			}
			rows = append(rows, runItemRow(run, evalRowDeploy, "delivery.ReadDeploys", read.rng.label, map[string]any{
				"reached_production": reached[run.RunID], "shipped_prs": len(shipped[run.RunID]),
			}))
		}
	}
	return rows, nil
}

// failureItemsOf is the rows of the record behind read's eval, a failure measure, on the range's
// days, as its day rows count them (evals.FailureRecord.Items over the record evals.FailureRecordIn
// reads once a request, evalRequestReads), kept once a range (evalRangeReads): its deploys and its
// failures both read them. Of a request naming repositories it keeps those counted through a
// deploy of one of them, the rows of those repositories' day rows.
func (s *server) failureItemsOf(ctx context.Context, read evalItemsRead) ([]evals.FailureItem, error) {
	if !read.once.failuresRead {
		record, err := evals.FailureRecordIn(ctx, read.tx, s.deps.Attributor, read.project, read.entry.Definition.Slug)
		if err != nil {
			return nil, err
		}
		read.once.failures, read.once.failuresRead = record, true
	}
	if !read.shared.failuresRead {
		items := read.once.failures.Items(read.rng.days)
		if len(read.repos) > 0 {
			items = slices.DeleteFunc(items, func(item evals.FailureItem) bool { return !slices.Contains(read.repos, item.Repo) })
		}
		read.shared.failures, read.shared.failuresRead = items, true
	}
	return read.shared.failures, nil
}

// failureDeployItemRows is the deploys of the record a failure measure's day rows of the range
// count (failureItemsOf), each with its relation to the measure.
func (s *server) failureDeployItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	return s.failureRowsOf(ctx, read, prodevents.KindDeploy)
}

// failureItemRows is the failures of the record a failure measure's day rows of the range count
// (failureItemsOf), each with its relation to the measure.
func (s *server) failureItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	return s.failureRowsOf(ctx, read, prodevents.KindFailure)
}

// failureRowsOf is the rows of kind (a deploy or a failure) of failureItemsOf, each as an items
// row: keyed by its id in the record, its columns its own and its relation, the repository of the
// deploy it is counted through, and a deploy's count of counted failures or a failure's minutes to
// restore; measured at the moment it counts on (evals.FailureItem.At), and addressed by its url.
func (s *server) failureRowsOf(ctx context.Context, read evalItemsRead, kind string) ([]evalItemRow, error) {
	items, err := s.failureItemsOf(ctx, read)
	if err != nil {
		return nil, err
	}
	var rows []evalItemRow
	for _, item := range items {
		event := item.Event
		if event.Kind != kind {
			continue
		}
		cols := map[string]any{
			"relation": item.Relation, "repo": item.Repo, "source": event.Source, "source_ref": event.SourceRef,
			"pull_requests": event.PullRequests, "note": event.Note,
		}
		rowKind := evalRowDeploy
		if kind == prodevents.KindDeploy {
			cols["status"], cols["started_at"], cols["finished_at"], cols["failures"] = event.Status, event.StartedAt, event.FinishedAt, item.Failures
		} else {
			rowKind = evalRowFailure
			cols["deploy_id"], cols["signal"], cols["detected_at"] = event.DeployID, event.Signal, event.DetectedAt
			cols["restored_at"], cols["restored_by_deploy"], cols["minutes"] = event.RestoredAt, event.RestoredByDeploy, item.Minutes
		}
		rows = append(rows, evalItemRow{
			RowKey: event.ID, RowKind: rowKind, Cols: cols, MeasuredAt: item.At, Source: "production_events",
			Href: event.URL, Links: []evals.Link{}, Range: read.rng.label,
		})
	}
	return rows, nil
}

// runItemRow is one run as an items row, keyed `<repo>/<run id>`, its columns the run's own and
// extra, measured when it started, which is what the deploy and CI measures count a run by, and
// addressed by its page on GitHub.
func runItemRow(run delivery.DeliveryRun, kind, source, label string, extra map[string]any) evalItemRow {
	cols := map[string]any{
		"repo": run.Repo, "run_id": run.RunID, "pr_number": run.PRNumber, "event": run.Event, "head_sha": run.HeadSHA,
		"started_at": run.StartedAt, "completed_at": run.CompletedAt, "conclusion": run.Conclusion,
	}
	maps.Copy(cols, extra)
	return evalItemRow{
		RowKey: run.Repo + "/" + strconv.FormatInt(run.RunID, 10), RowKind: kind, Cols: cols, MeasuredAt: run.StartedAt,
		Source: source, Href: run.URL, Links: []evals.Link{}, Range: label,
	}
}

// runItemRows is the runs a CI eval's day rows of the range counted: each deploy repository's
// (repoRules, delivery.ReadCIRows under that repository's rule, as the day builder reads them):
// pr-ci's pull-request checks runs, each with its wall time and outcome, and a deploy card's
// eval's push runs on main, each with the card's job's minutes and outcome; each built by its
// eval's day builder over the one run (measures.PRChecksDay, measures.DeployJobsDay), so a row's
// minutes are its part of that eval's day.
func (s *server) runItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	members, err := s.counted(ctx, read, evals.KindCI, read.rng.days)
	if err != nil {
		return nil, err
	}
	rules, err := s.repoRules(ctx, read)
	if err != nil {
		return nil, err
	}
	card, isCard := evals.CardOf(read.entry.Definition.Slug)
	var rows []evalItemRow
	for _, rule := range rules {
		cards := measures.DeployCards(rule)
		ci, err := delivery.ReadCIRows(ctx, read.tx, rule.DeployRepo, read.rng.days.From, read.rng.days.To, measures.DeployCardJobNames(cards))
		if err != nil {
			return nil, err
		}
		runs, jobs := ci.Checks.Runs, ci.Checks.Jobs
		cardIndex := -1
		if isCard {
			runs, jobs = ci.Deploys.Runs, ci.Deploys.Jobs
			cardIndex = slices.IndexFunc(cards, func(c measures.DeployJobCard) bool { return c.CardID == card })
		}
		for _, run := range runs {
			if !members[evals.RunMember(run.Repo, run.RunID)] {
				continue
			}
			day, one := measures.Midnight(run.StartedAt), []delivery.DeliveryRun{run}
			var parts []measures.DayPart
			extra := map[string]any{}
			if isCard {
				if cardIndex < 0 {
					continue
				}
				parts = measures.DeployJobsDay(day, one, jobs, cards)[card]
				extra["job"] = strings.Join(cards[cardIndex].JobNames, " or ")
			} else {
				parts = slices.DeleteFunc(measures.PRChecksDay(day, one, jobs), func(part measures.DayPart) bool {
					return part.Series != measures.SeriesMinutes
				})
				extra["jobs"] = len(jobs[run.RunID])
			}
			extra["minutes"], extra["outcome"] = sampleOf(parts), outcomeOf(parts)
			rows = append(rows, runItemRow(run, evalRowRun, "delivery.ReadCIRows", read.rng.label, extra))
		}
	}
	return rows, nil
}

// outcomeOf is the outcome a timing part built over one result counts, "" where it counts none (a
// result skipped, or still running).
func outcomeOf(parts []measures.DayPart) string {
	for _, part := range parts {
		for _, outcome := range []string{measures.CountSuccess, measures.CountFailure, measures.CountCancelled} {
			if part.Counts[outcome] > 0 {
				return outcome
			}
		}
	}
	return ""
}

// jobItemRows is pr-ci's jobs over the range: with sort failures, the jobs of each deploy
// repository that failed in the 24 hours before the range's end (failingJobRows, keyed by
// repository and name), most failures first; otherwise, keyed by the job's name, each job's
// figures, its unlisted `job:<name>` day rows of the range folded by measures.FoldTiming, by name,
// or with sort median the one ranking the Pipeline's slowest-jobs table shows the first
// measures.SlowestJobs of (measures.FoldSlowestJobs): every job with the job series' min_n timed
// results over the range's days (eval_series `job:*`, evals.SlowestJobsMinN), slowest first. A
// series filter names one job's series. An eval with no job series answers none. Each job links
// to its `job:<name>` day rows over the range as it was asked for (read.rng.start and end), which
// the items route reads as the same days: the range's whole days are a day longer where it starts
// inside one, and past the 84 days the route takes where it was 84 days long.
func (s *server) jobItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	index := slices.IndexFunc(read.entry.Series, func(series evals.Series) bool { return series.Series == evals.SeriesJobs })
	if index < 0 {
		return nil, nil
	}
	if read.request.sort == evalSortFailures {
		return s.failingJobRows(ctx, read)
	}
	days, err := s.evalDaysOf(ctx, read)
	if err != nil {
		return nil, err
	}
	var parts []measures.DayPart
	newest := map[string]time.Time{}
	for _, day := range days {
		name, job := strings.CutPrefix(day.Series, measures.JobSeriesPrefix)
		if !job {
			continue
		}
		parts = append(parts, day.DayPart)
		if day.Day.After(newest[name]) {
			newest[name] = day.Day
		}
	}
	type jobCell struct {
		name string
		cell measures.Cell
	}
	var cells []jobCell
	if read.request.sort == evalSortMedian {
		// The range's own days: its day rows are of no other.
		shown := evalRangeDays(read.rng.days)
		for _, job := range measures.FoldSlowestJobs(parts, shown, evals.SlowestJobsMinN(read.entry.Series[index].MinN)) {
			cells = append(cells, jobCell{job.Name, job.Total})
		}
	} else {
		byJob := map[string][]measures.DayPart{}
		for _, part := range parts {
			name := strings.TrimPrefix(part.Series, measures.JobSeriesPrefix)
			// The job's parts as the timing fold reads a series: its minutes.
			part.Series = measures.SeriesMinutes
			byJob[name] = append(byJob[name], part)
		}
		for _, name := range slices.Sorted(maps.Keys(byJob)) {
			cells = append(cells, jobCell{name, measures.FoldTiming(byJob[name])})
		}
	}
	rows := make([]evalItemRow, len(cells))
	for position, job := range cells {
		behind := url.Values{
			"from": {read.rng.start.Format(time.RFC3339Nano)}, "to": {read.rng.end.Format(time.RFC3339Nano)},
			"series": {measures.JobSeriesPrefix + job.name}, "row_kind": {evalRowDay},
		}
		if len(read.request.repos) > 0 {
			behind["repo"] = read.request.repos
		}
		rows[position] = evalItemRow{
			RowKey:  job.name,
			RowKind: evalRowJob,
			Cols: map[string]any{
				"name": job.name, "n": job.cell.N, "median_minutes": job.cell.MedianMinutes, "p90_minutes": job.cell.P90Minutes,
				"success": job.cell.Success, "failure": job.cell.Failure, "cancelled": job.cell.Cancelled,
				"success_rate": job.cell.SuccessRate,
			},
			MeasuredAt: newest[job.name],
			Source:     "measures.FoldTiming",
			Href:       evalItemsPath(read.project, read.entry.Definition.Slug) + "?" + behind.Encode(),
			Links:      []evals.Link{},
			Range:      read.rng.label,
		}
	}
	return rows, nil
}

// failingJobRows is pr-ci's jobs that failed in the measures.FailingWindow before the range's end:
// of each deploy repository the eval reads (repoRules), of the pull-request checks runs started on
// the range's days, the runs measures.FailingJobs picks, those that completed in the FailingWindow
// up to the end (delivery.ReadPRChecksCompletedIn, which reads them by their completion and every
// job of each, rather than every run of the range with every job), those the project's CI day rows
// counted, through measures.FailingJobs under that repository's roll-up job. The day rows count a
// run on the day it started, so the items they counted are read on the days the runs read started
// on alone. A job belongs to its repository's workflow, so each repository's jobs are rows of their
// own, keyed `<repo>/<name>` (failingJobKey) and naming their repository, each with its failures,
// its results and its latest failed run, which it links to; most failures first. Of one repository
// they are the list the Pipeline route's failing-jobs table answers for an `at` at the range's end
// when the range starts on the first of the seven days that at shows; the Pipeline tallies the
// jobs of one name of every repository as one. The runs it counts, of every repository, are kept
// for the range (evalRangeReads.failing), so the live view of the list expires as each run's
// failures leave the 24 hours (failingJobsNextExpiry).
func (s *server) failingJobRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	end := read.rng.end
	span := read.rng.days
	rules, err := s.repoRules(ctx, read)
	if err != nil {
		return nil, err
	}
	type repoChecks struct {
		rule   delivery.DeliverySettings
		checks delivery.RunsWithJobs
	}
	var repos []repoChecks
	first := span.To
	for _, rule := range rules {
		checks, err := delivery.ReadPRChecksCompletedIn(ctx, read.tx, rule.DeployRepo, span.From, span.To, end.Add(-measures.FailingWindow), end)
		if err != nil {
			return nil, err
		}
		if len(checks.Runs) == 0 {
			continue
		}
		// The runs come oldest start first, so the first started on the first day any of them did.
		if day := measures.Midnight(checks.Runs[0].StartedAt); day.Before(first) {
			first = day
		}
		repos = append(repos, repoChecks{rule: rule, checks: checks})
	}
	if len(repos) == 0 {
		return nil, nil
	}
	members, err := s.counted(ctx, read, evals.KindCI, evals.Range{From: first, To: span.To})
	if err != nil {
		return nil, err
	}
	type repoJob struct {
		repo string
		job  measures.FailingJob
	}
	var failing []repoJob
	var counted []delivery.DeliveryRun
	for _, one := range repos {
		runs := slices.DeleteFunc(one.checks.Runs, func(run delivery.DeliveryRun) bool { return !members[evals.RunMember(run.Repo, run.RunID)] })
		counted = append(counted, runs...)
		for _, job := range measures.FailingJobs(runs, one.checks.Jobs, end, deref(one.rule.PRChecksRollupJobName)) {
			failing = append(failing, repoJob{repo: one.rule.DeployRepo, job: job})
		}
	}
	read.shared.failing = counted
	slices.SortStableFunc(failing, func(a, b repoJob) int { return b.job.Failures - a.job.Failures })
	var rows []evalItemRow
	for _, one := range failing {
		job := one.job
		if read.request.series != "" && read.request.series != measures.JobSeriesPrefix+job.Name {
			continue
		}
		rows = append(rows, evalItemRow{
			RowKey:  failingJobKey(one.repo, job.Name),
			RowKind: evalRowJob,
			Cols: map[string]any{
				"name": job.Name, "repo": one.repo, "failures": job.Failures, "results": job.Results,
				"last_failed_at": job.LastFailedAt, "last_run_url": job.LastRunURL,
			},
			MeasuredAt: job.LastFailedAt,
			Source:     "measures.FailingJobs",
			Href:       job.LastRunURL,
			Links:      []evals.Link{},
			Range:      read.rng.label,
		})
	}
	return rows, nil
}

// failingJobKey is the row key of a failing job of repo's workflow named name: `<repo>/<name>`, as
// a run row is `<repo>/<run id>`, since two repositories' workflows can name a job alike.
func failingJobKey(repo, name string) string {
	return repo + "/" + name
}
