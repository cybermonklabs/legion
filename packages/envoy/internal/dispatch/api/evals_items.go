package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// GET /api/v1/projects/{key}/evals/{slug}/items: the rows behind an eval's numbers, every row in
// one shape whatever its kind (R7), so a page draws each row kind with one renderer and a client
// reads one type.
//
// Every eval answers its readings (evals.Store.Items, a reading logged under a computed eval's
// slug among them, which its value never folds, R3) and its day rows (evals.Store.EvalDays,
// `day`). A computed eval answers besides the rows its numbers count, by its source
// (evalComputedItems): the issues a Legion eval counts, closed in the range, and the filings
// legion-splitting counts (`issue`, from the stored facts and filings, legionfacts.ItemRows);
// the pull requests and deploy runs a delivery eval counts (`pull_request`, `deploy`, from the
// delivery reads the day rows are built with, delivery/measure_rows.go); the runs a CI eval times
// (`run`) and, for pr-ci, each job's figures (`job`); and the open P0 issues with no owner (`issue`,
// delivery.ListUnownedP0). A delivery or CI row is answered where the project's day rows counted
// it (eval_day_members, evals.Store.CountedIn), so the items are the rows the numbers fold, under
// the attribution the day rows made (D1).
//
// A row of the compare range carries range `compare`. The page runs across the row kinds in the
// order above, each range's after the window's, and a row kind's rows in its own order.

// evalItemRow is one row of an items answer, the same fields for every row kind (R7): its key and
// kind, its columns (cols, whose names are the row kind's own), when it was measured, the source
// it was read from, the address a reader follows to see it, the links it carries (a reading's
// own, [] for every other row) and the range it is of.
//
// MeasuredAt is when the row's value was measured, the moment its eval dates it by and so the
// moment that puts it in its range, never when a row was last written: a reading's own
// measured_at; a pull request's merge, the day its delivery measures count it on; a deploy run's
// and a CI run's start, which the deploy and CI measures count it by (delivery.ReadDeploys and
// ReadCIRows read runs by start); a Legion issue's close and a filing's filing; a day row's day; a
// job's figures the newest day of the day rows they fold; a failing job its latest failure, which
// measures.FailingJobs counts by completion; and an open P0 issue with no owner the read itself,
// since the count is of the present.
type evalItemRow struct {
	RowKey     string         `json:"row_key"`
	RowKind    string         `json:"row_kind"`
	Cols       map[string]any `json:"cols"`
	MeasuredAt time.Time      `json:"measured_at"`
	Source     string         `json:"source"`
	Href       string         `json:"href"`
	Links      []evals.Link   `json:"links"`
	Range      string         `json:"range" enum:"window,compare"`
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
	evalRowRun         = "run"
	evalRowJob         = "job"
	evalRowDay         = "day"
)

// The two orders `sort` names, each of which answers pr-ci's job rows alone: the slowest jobs
// with the series' min_n timed results by their median over the range, slowest first, and the
// jobs that failed in the 24 hours before the range's end (measures.FailingJobs), most failures
// first.
const (
	evalSortMedian   = "median"
	evalSortFailures = "failures"
)

// evalItemsRequest is one items read: the window and compare range, the filters and the page.
type evalItemsRequest struct {
	ranges  evalRanges
	series  string
	rowKind string
	row     string
	sort    string
	repos   []string
	limit   int
	offset  int
}

// parseEvalItemsQuery reads the items route's query; a key the route does not take
// (undeclaredQueryKey) and a malformed key are 400 INVALID_QUERY naming it.
func parseEvalItemsQuery(query url.Values) (evalItemsRequest, error) {
	if err := undeclaredQueryKey(query, evalItemsQueryKeys); err != nil {
		return evalItemsRequest{}, err
	}
	invalid := func(err error) (evalItemsRequest, error) {
		return evalItemsRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s", err.Error())
	}
	ranges, err := parseEvalRanges(query)
	if err != nil {
		return invalid(err)
	}
	repos, err := parseEvalRepos(query)
	if err != nil {
		return invalid(err)
	}
	limit, offset, err := parseEvalPage(query)
	if err != nil {
		return invalid(err)
	}
	request := evalItemsRequest{
		ranges: ranges, series: query.Get("series"), rowKind: query.Get("row_kind"), row: query.Get("row"),
		sort: query.Get("sort"), repos: repos, limit: limit, offset: offset,
	}
	if request.sort != "" && request.sort != evalSortMedian && request.sort != evalSortFailures {
		return invalid(fmt.Errorf("sort %q is neither %s nor %s", request.sort, evalSortMedian, evalSortFailures))
	}
	return request, nil
}

// evalItemsRange is one range an items read reads: its days, the instants it was asked for,
// [start, end), and its name in the answer (evals.RangeWindow, evals.RangeCompare).
type evalItemsRange struct {
	days       evals.Range
	start, end time.Time
	label      string
}

// evalItemsRead is what a reader of one row kind reads for one range: the eval, the request and
// the range, and the delivery settings the delivery and CI rows are read under, nil where the
// deployment has set none, which leaves no such row to read.
type evalItemsRead struct {
	project  string
	entry    evals.Entry
	request  evalItemsRequest
	rng      evalItemsRange
	settings *delivery.DeliverySettings
}

// evalItemKind is one row kind a computed eval answers, and its reader.
type evalItemKind struct {
	kind string
	read func(s *server, ctx context.Context, read evalItemsRead) ([]evalItemRow, error)
}

var (
	pullRequestItems = evalItemKind{evalRowPullRequest, (*server).pullRequestItemRows}
	deployItems      = evalItemKind{evalRowDeploy, (*server).deployItemRows}
	legionItems      = evalItemKind{evalRowIssue, (*server).legionItemRows}
)

// evalComputedItems is the row kinds each computed eval answers beside its readings and its day
// rows, by the source it names: the rows its fold counts.
var evalComputedItems = map[string][]evalItemKind{
	evals.SourceComputedPrefix + "delivery/measures.FoldDeployFrequency":   {deployItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldDeployRunSuccess":  {deployItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldLeadTime":          {pullRequestItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldReworkShare":       {pullRequestItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldTimeToRestore":     {pullRequestItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldChangeFailureRate": {pullRequestItems, deployItems},
	evals.SourceComputedPrefix + "delivery/measures.FoldTiming":            {{evalRowRun, (*server).runItemRows}, {evalRowJob, (*server).jobItemRows}},
	evals.SourceComputedPrefix + "delivery.ListUnownedP0":                  {{evalRowIssue, (*server).unownedItemRows}},
	evals.SourceComputedPrefix + "legion/metrics.FoldWorkTime":             {legionItems},
	evals.SourceComputedPrefix + "legion/metrics.FoldPhaseTime":            {legionItems},
	evals.SourceComputedPrefix + "legion/metrics.FoldIdleTime":             {legionItems},
	evals.SourceComputedPrefix + "legion/metrics.FoldSlotWait":             {legionItems},
	evals.SourceComputedPrefix + "legion/metrics.FoldRework":               {legionItems},
	evals.SourceComputedPrefix + "legion/metrics.FoldSplitting":            {legionItems},
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

// listProjectEvalItems answers GET /api/v1/projects/{key}/evals/{slug}/items: one page of the rows
// behind the eval's numbers over the window and the compare range, filtered by series, row_kind,
// row and repo, and for pr-ci's jobs ranked by sort. 404 PROJECT_NOT_FOUND and EVAL_NOT_FOUND as
// the detail read answers them.
func (s *server) listProjectEvalItems(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	request, err := parseEvalItemsQuery(r.URL.Query())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx := r.Context()
	project := r.PathValue("key")
	entry, err := s.loadEval(ctx, project, r.PathValue("slug"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	sources, err := s.evalItemSources(ctx, project, entry, request)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer := evalItemsResponse{
		Items: []evalItemRow{}, Limit: request.limit, Offset: request.offset,
		Window: newEvalRange(request.ranges.window), Compare: evalCompareRange(request.ranges.compare),
	}
	skip, want := request.offset, request.limit
	for _, source := range sources {
		rows, total, err := source(ctx, skip, want)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		answer.Total += total
		skip = max(0, skip-total)
		rows = rows[:min(len(rows), want)]
		answer.Items, want = append(answer.Items, rows...), want-len(rows)
	}
	WriteJSON(w, http.StatusOK, answer)
}

// evalItemSources is the sources of one items read, in the order its page runs across them: for
// the window and then the compare range, the computed rows of each kind the eval's source answers
// (evalComputedItems), its readings and its day rows, each kept where row_kind names it or names
// none. A request naming sort reads pr-ci's job rows alone.
func (s *server) evalItemSources(ctx context.Context, project string, entry evals.Entry, request evalItemsRequest) ([]evalItemSource, error) {
	ranges := []evalItemsRange{{days: request.ranges.window, start: request.ranges.from, end: request.ranges.to, label: evals.RangeWindow}}
	if request.ranges.compare != nil {
		ranges = append(ranges, evalItemsRange{
			days: *request.ranges.compare, start: request.ranges.compareFrom, end: request.ranges.compareTo, label: evals.RangeCompare,
		})
	}
	settings, err := s.evalDeliverySettings(ctx)
	if err != nil {
		return nil, err
	}
	kept := func(kind string) bool {
		if request.sort != "" {
			return kind == evalRowJob
		}
		return request.rowKind == "" || request.rowKind == kind
	}
	registry := s.registry()
	definition := entry.Definition
	var sources []evalItemSource
	for _, rng := range ranges {
		read := evalItemsRead{project: project, entry: entry, request: request, rng: rng, settings: settings}
		if definition.Computed() {
			for _, kind := range evalComputedItems[definition.Source] {
				if !kept(kind.kind) {
					continue
				}
				rows, err := kind.read(s, ctx, read)
				if err != nil {
					return nil, err
				}
				if request.row != "" {
					rows = slices.DeleteFunc(rows, func(row evalItemRow) bool { return row.RowKey != request.row })
				}
				sources = append(sources, listedSource(rows))
			}
		}
		if request.sort != "" {
			continue
		}
		// A reading's row kind is the one its writer gave it, so row_kind keeps the readings of that
		// kind, as the store reads them.
		sources = append(sources, func(ctx context.Context, skip, want int) ([]evalItemRow, int, error) {
			page, err := registry.Items(ctx, definition.EvalID, request.series, request.rowKind, request.row, request.repos, rng.days, max(want, 1), skip)
			if err != nil {
				return nil, 0, err
			}
			rows := make([]evalItemRow, 0, len(page.Items))
			for _, reading := range page.Items[:min(len(page.Items), want)] {
				rows = append(rows, readingItemRow(reading, rng.label))
			}
			return rows, page.Total, nil
		})
		if kept(evalRowDay) {
			days, err := registry.EvalDays(ctx, project, definition.EvalID, request.series, request.repos, rng.days)
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
			sources = append(sources, listedSource(rows))
		}
	}
	return sources, nil
}

// evalDeliverySettings is the deployment's delivery settings, nil where it has set none.
func (s *server) evalDeliverySettings(ctx context.Context) (*delivery.DeliverySettings, error) {
	settings, err := delivery.GetSettings(ctx, s.deps.Store.Pool)
	if errors.Is(err, delivery.ErrNoSettings) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &settings, nil
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
// that for a repository's row; its columns the row's parts but its sample list; measured on its
// day, the day its parts are of; and as its address the items of that day of that series, of that
// repository for a repository's row.
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
			"counts": nonNilCounts(day.Counts), "via_repository": day.ViaRepository, "built_by": day.BuiltBy,
		},
		MeasuredAt: day.Day,
		Source:     "eval_days",
		Href:       evalItemsPath(project, slug) + "?" + behind.Encode(),
		Links:      []evals.Link{},
		Range:      label,
	}
}

func nonNilCounts(counts map[string]float64) map[string]float64 {
	if counts == nil {
		return map[string]float64{}
	}
	return counts
}

// evalItemsPath is the items route of one eval.
func evalItemsPath(project, slug string) string {
	return "/api/v1/projects/" + url.PathEscape(project) + "/evals/" + url.PathEscape(slug) + "/items"
}

// legionItemRows is the issues a Legion eval counts in the range: the issues of the project that
// closed in it (legionfacts.ReadClosedIn), and for legion-splitting the filings its sessions made
// in it (legionfacts.ReadFilingsIn); a `filed:<class>` series reads the filings of that class
// alone, and any other series of legion-splitting the closed issues alone. A series' cut keeps
// the issues of that cut, by the stored root (metrics.InCut), and repo the issues that resolve to
// one of the repositories named (legionfacts.ReadRepos, the repository the day rows cut by). Each
// row carries its issue's title and priority (legionfacts.WithIssues).
func (s *server) legionItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	pool := s.deps.Store.Pool
	base, cut := evals.SplitCut(read.request.series)
	class, filedSeries := strings.CutPrefix(base, "filed:")
	splitting := read.entry.Definition.Slug == evals.SlugLegionSplitting
	var items []legionfacts.ItemRow
	if splitting && (read.request.series == "" || filedSeries) {
		filings, err := legionfacts.ReadFilingsIn(ctx, pool, read.project, read.rng.days.From, read.rng.days.To)
		if err != nil {
			return nil, err
		}
		filings = slices.DeleteFunc(filings, func(filing legionfacts.Filing) bool {
			return (filedSeries && filing.Class != class) || !metrics.InCut(cut, filing.RootKey == filing.Issue)
		})
		items = append(items, legionfacts.FilingItemRows(filings)...)
	}
	if !filedSeries {
		closed, err := legionfacts.ReadClosedIn(ctx, pool, read.project, read.rng.days.From, read.rng.days.To)
		if err != nil {
			return nil, err
		}
		closed = slices.DeleteFunc(closed, func(row legionfacts.Row) bool { return !metrics.InCut(cut, row.RootKey == row.Issue) })
		items = append(items, legionfacts.ItemRows(closed)...)
	}
	keys := make([]string, len(items))
	for index, item := range items {
		keys[index] = item.RowKey
	}
	if len(read.request.repos) > 0 {
		repos, err := legionfacts.ReadRepos(ctx, pool, keys)
		if err != nil {
			return nil, err
		}
		items = slices.DeleteFunc(items, func(item legionfacts.ItemRow) bool { return !slices.Contains(read.request.repos, repos[item.RowKey]) })
	}
	issues, err := legionfacts.ReadItemIssues(ctx, pool, keys)
	if err != nil {
		return nil, err
	}
	rows := make([]evalItemRow, len(items))
	for index, item := range legionfacts.WithIssues(items, issues) {
		rows[index] = evalItemRow{
			RowKey: item.RowKey, RowKind: item.RowKind, Cols: item.Columns, MeasuredAt: item.MeasuredAt,
			Source: item.Source, Href: item.Href, Links: []evals.Link{}, Range: read.rng.label,
		}
	}
	return rows, nil
}

// unownedItemRows is unowned-p0's issues: the project's open P0 issues with no owner now
// (delivery.ListUnownedP0), the count its day rows record, answered in the window alone since
// the count is of the present.
func (s *server) unownedItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.rng.label != evals.RangeWindow {
		return nil, nil
	}
	issues, err := delivery.ListUnownedP0(ctx, s.deps.Store.Pool, read.project)
	if err != nil {
		return nil, err
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
// days, those of the repositories the request names where it names any.
func (s *server) counted(ctx context.Context, read evalItemsRead, family string, days evals.Range) (map[string]bool, error) {
	items, err := s.registry().CountedIn(ctx, read.project, family, read.request.repos, days)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]bool, len(items))
	for _, item := range items {
		keys[item.RowKey] = true
	}
	return keys, nil
}

// pullRequestItemRows is the pull requests a delivery eval's day rows of the range counted: those
// merged in it (delivery.ReadMergedRows, as the day builder reads them), each with the deploy
// that ships it (delivery.ContainingRun) and its four lead times, each built by its eval's day
// builder over the one pull request (measures.MergeToProductionDay and the rest), so a row's
// minutes are its part of that eval's day.
func (s *server) pullRequestItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.settings == nil {
		return nil, nil
	}
	settings := *read.settings
	members, err := s.counted(ctx, read, evals.KindDelivery, read.rng.days)
	if err != nil {
		return nil, err
	}
	merged, err := delivery.ReadMergedRows(ctx, s.deps.Store.Pool, settings, read.rng.days.From, read.rng.days.To, false)
	if err != nil {
		return nil, err
	}
	applies := delivery.ProductionApplies(merged.ApplyRuns, merged.ApplyJobs, settings.Production()).Applies
	var rows []evalItemRow
	for _, pr := range merged.PullRequests {
		if pr.CreatedAt == nil || pr.MergedAt == nil || !members[evals.PullRequestMember(pr.Repo, pr.Number)] {
			continue
		}
		measured := measures.PR{
			ID: pr.Repo + "#" + strconv.Itoa(pr.Number), CreatedAt: *pr.CreatedAt, MergedAt: *pr.MergedAt,
			FirstCommitAt: pr.FirstCommitAt, Rework: pr.Rework,
		}
		if apply := delivery.ContainingRun(pr, settings.DeployRepo, applies); apply != nil {
			measured.DeployRunID, measured.DeployedAt = &apply.RunID, &apply.CompletedAt
		}
		day, one := utcDayOf(*pr.MergedAt), []measures.PR{measured}
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
			Source:     "delivery.ReadMergedRows",
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

// deployItemRows is the deploy runs a delivery eval's day rows of the range counted: the deploy
// workflow's runs on main started in it (delivery.ReadDeploys, as the day builder reads them),
// each with whether it reached production and how many pull requests it ships
// (delivery.Deploys.ShippedBy, the rule with_prs counts by).
func (s *server) deployItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.settings == nil {
		return nil, nil
	}
	settings, pool := *read.settings, s.deps.Store.Pool
	members, err := s.counted(ctx, read, evals.KindDelivery, read.rng.days)
	if err != nil {
		return nil, err
	}
	deploys, err := delivery.ReadDeploys(ctx, pool, settings, read.rng.days.From, read.rng.days.To)
	if err != nil {
		return nil, err
	}
	merged, err := delivery.ReadMergedRows(ctx, pool, settings, read.rng.days.From, read.rng.days.To, false)
	if err != nil {
		return nil, err
	}
	shipped := deploys.ShippedBy(merged, settings)
	reached := map[int64]bool{}
	for _, apply := range deploys.Applies {
		reached[apply.RunID] = true
	}
	var rows []evalItemRow
	for _, run := range deploys.Started.Runs {
		if !members[evals.RunMember(run.Repo, run.RunID)] {
			continue
		}
		rows = append(rows, runItemRow(run, evalRowDeploy, "delivery.ReadDeploys", read.rng.label, map[string]any{
			"reached_production": reached[run.RunID], "shipped_prs": len(shipped[run.RunID]),
		}))
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

// runItemRows is the runs a CI eval's day rows of the range counted (delivery.ReadCIRows, as the
// day builder reads them): pr-ci's pull-request checks runs, each with its wall time and outcome,
// and a deploy card's eval's push runs on main, each with the card's job's minutes and outcome;
// each built by its eval's day builder over the one run (measures.PRChecksDay,
// measures.DeployJobsDay), so a row's minutes are its part of that eval's day.
func (s *server) runItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.settings == nil {
		return nil, nil
	}
	settings := *read.settings
	members, err := s.counted(ctx, read, evals.KindCI, read.rng.days)
	if err != nil {
		return nil, err
	}
	cards := measures.DeployCards(settings)
	ci, err := delivery.ReadCIRows(ctx, s.deps.Store.Pool, settings.DeployRepo, read.rng.days.From, read.rng.days.To, measures.DeployCardJobNames(cards))
	if err != nil {
		return nil, err
	}
	card, isCard := evals.CardOf(read.entry.Definition.Slug)
	runs, jobs := ci.Checks.Runs, ci.Checks.Jobs
	if isCard {
		runs, jobs = ci.Deploys.Runs, ci.Deploys.Jobs
	}
	var rows []evalItemRow
	for _, run := range runs {
		if !members[evals.RunMember(run.Repo, run.RunID)] {
			continue
		}
		day, one := utcDayOf(run.StartedAt), []delivery.DeliveryRun{run}
		var parts []measures.DayPart
		extra := map[string]any{}
		if isCard {
			index := slices.IndexFunc(cards, func(c measures.DeployJobCard) bool { return c.CardID == card })
			if index < 0 {
				continue
			}
			parts = measures.DeployJobsDay(day, one, jobs, cards)[card]
			extra["job"] = strings.Join(cards[index].JobNames, " or ")
		} else {
			parts = slices.DeleteFunc(measures.PRChecksDay(day, one, jobs), func(part measures.DayPart) bool {
				return part.Series != measures.SeriesMinutes
			})
			extra["jobs"] = len(jobs[run.RunID])
		}
		extra["minutes"], extra["outcome"] = sampleOf(parts), outcomeOf(parts)
		rows = append(rows, runItemRow(run, evalRowRun, "delivery.ReadCIRows", read.rng.label, extra))
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

// jobItemRows is pr-ci's jobs over the range, keyed by the job's name: with sort failures, the
// jobs that failed in the 24 hours before the range's end, the Pipeline's own failing-jobs list
// for the same from and to (failingJobRows), most failures first; otherwise each job's figures,
// its unlisted `job:<name>` day rows of the range folded by measures.FoldTiming, by name, or with
// sort median the jobs with the job series' min_n timed results (eval_series `job:*`), slowest
// first, the ranking measures.FoldSlowestJobs makes. A series filter names one job's series. An
// eval with no job series answers none.
func (s *server) jobItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	index := slices.IndexFunc(read.entry.Series, func(series evals.Series) bool { return series.Series == evals.SeriesJobs })
	if index < 0 {
		return nil, nil
	}
	if read.request.sort == evalSortFailures {
		return s.failingJobRows(ctx, read)
	}
	days, err := s.registry().EvalDays(ctx, read.project, read.entry.Definition.EvalID, read.request.series, read.request.repos, read.rng.days)
	if err != nil {
		return nil, err
	}
	byJob := map[string][]measures.DayPart{}
	newest := map[string]time.Time{}
	for _, day := range days {
		name, job := strings.CutPrefix(day.Series, measures.JobSeriesPrefix)
		if !job {
			continue
		}
		// The job's parts as the timing fold reads a series: its minutes.
		part := day.Part()
		part.Series = measures.SeriesMinutes
		byJob[name] = append(byJob[name], part)
		if day.Day.After(newest[name]) {
			newest[name] = day.Day
		}
	}
	minN := measures.SlowestMinResults
	if read.entry.Series[index].MinN != nil {
		minN = *read.entry.Series[index].MinN
	}
	type jobCell struct {
		name string
		cell measures.Cell
	}
	var cells []jobCell
	for _, name := range slices.Sorted(maps.Keys(byJob)) {
		cell := measures.FoldTiming(byJob[name])
		// A ranked job has min_n timed results and at least one: N counts the timed results, so a
		// job with none, which a min_n of 0 would keep, has no median to rank by.
		if read.request.sort == evalSortMedian && (cell.N < minN || cell.MedianMinutes == nil) {
			continue
		}
		cells = append(cells, jobCell{name, cell})
	}
	if read.request.sort == evalSortMedian {
		slices.SortStableFunc(cells, func(a, b jobCell) int { return cmp.Compare(*b.cell.MedianMinutes, *a.cell.MedianMinutes) })
	}
	rows := make([]evalItemRow, len(cells))
	for position, job := range cells {
		behind := url.Values{
			"from": {read.rng.days.From.Format(time.RFC3339)}, "to": {read.rng.days.To.Format(time.RFC3339)},
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

// failingJobRows is pr-ci's jobs that failed in the measures.FailingWindow before the range's end,
// the list the Pipeline route's failing-jobs table answers for the same from and to: the
// pull-request checks runs the Pipeline reads for that window (delivery.ReadCIRows over
// pipelineWindowBetween's days, which hold every run that can finish a job in those 24 hours
// however long before them it started), those the project's CI day rows counted, through the
// same measures.FailingJobs; each job with its failures, its results and its latest failed run,
// which it links to.
func (s *server) failingJobRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	if read.settings == nil {
		return nil, nil
	}
	settings := *read.settings
	end := read.rng.end
	window := pipelineWindowBetween(read.rng.start, end)
	span := evals.Range{From: window.start(), To: window.readTo()}
	members, err := s.counted(ctx, read, evals.KindCI, span)
	if err != nil {
		return nil, err
	}
	ci, err := delivery.ReadCIRows(ctx, s.deps.Store.Pool, settings.DeployRepo, span.From, span.To,
		measures.DeployCardJobNames(measures.DeployCards(settings)))
	if err != nil {
		return nil, err
	}
	runs := slices.DeleteFunc(ci.Checks.Runs, func(run delivery.DeliveryRun) bool { return !members[evals.RunMember(run.Repo, run.RunID)] })
	rollup := ""
	if settings.PRChecksRollupJobName != nil {
		rollup = *settings.PRChecksRollupJobName
	}
	var rows []evalItemRow
	for _, job := range measures.FailingJobs(runs, ci.Checks.Jobs, end, rollup) {
		if read.request.series != "" && read.request.series != measures.JobSeriesPrefix+job.Name {
			continue
		}
		rows = append(rows, evalItemRow{
			RowKey:  job.Name,
			RowKind: evalRowJob,
			Cols: map[string]any{
				"name": job.Name, "failures": job.Failures, "results": job.Results, "last_failed_at": job.LastFailedAt,
				"last_run_url": job.LastRunURL,
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
