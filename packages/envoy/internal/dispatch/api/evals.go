package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The eval routes (dispatch://INFRA-2916, R3 to R5, R7, R9, R11, R12 and R16): every number a
// project's pages show is an eval of the registry (internal/dispatch/evals), read here and
// written here.
//
//   - GET /api/v1/projects/{key}/evals and GET /api/v1/evals read the list: each eval's current
//     definition with each listed series' statistics over a window of days and a compare range,
//     folded from the stored day rows of those days (evals.FoldStatistics, the one fold each
//     source names), never from the population again (R12). The cross-project read takes
//     `project` repeatably and reads every project when it names none: no read here picks a
//     project for its caller (R11, R14).
//   - GET /api/v1/projects/{key}/evals/{slug} reads one eval's versions, newest first, the newest
//     with the window's values, and the method its section of the project's `evals-md` holds (R5).
//   - GET /api/v1/projects/{key}/evals/{slug}/items reads the rows behind the numbers, one shape
//     for every row kind (evals_items.go).
//   - POST /api/v1/projects/{key}/evals/{slug}/readings logs one batch of readings, which the
//     registry never refuses for an eval it does not hold (R3), under a required
//     Idempotency-Key (R4); POST /api/v1/projects/{key}/evals registers or changes a definition.
//     A reading is append-only: PUT and DELETE of one answer 405 READING_APPEND_ONLY.
//
// Every answer is the envelope {items, total, limit, offset}, and every refusal {code, error}
// with a stable code (R7).
//
// A window is the UTC days its [from, to) covers, a window ending inside a day covering that day:
// the day rows are whole days, so the answer states the days it folded rather than the instants
// it was asked for (evalDaysOf).

const (
	// evalPageDefault and evalPageMax are an eval read's page: limit is 1 to evalPageMax,
	// evalPageDefault where the caller names none.
	evalPageDefault = 50
	evalPageMax     = 200

	codeEvalNotFound           = "EVAL_NOT_FOUND"
	codeIdempotencyKeyRequired = "IDEMPOTENCY_KEY_REQUIRED"
	codeReadingAppendOnly      = "READING_APPEND_ONLY"

	// evalSessionQueryKey is the one query key every eval route takes beside those its shape
	// declares: the `session` parameter every operation carries (actingPerson, openapi.go's
	// openAPISessionParameter).
	evalSessionQueryKey = "session"
)

// evalKinds are the kinds an eval read keeps with `kind`: eval_versions' check.
var evalKinds = []string{evals.KindDelivery, evals.KindLegion, evals.KindCI, evals.KindExperiment}

// The query keys each eval read declares: the `query` tags of the query struct its route shape
// names (routeShapes' Params), read once when the package starts. A readings write declares none.
var (
	evalListQueryKeys   = queryKeysOf(evalListQuery{})
	evalsQueryKeys      = queryKeysOf(evalsQuery{})
	evalDetailQueryKeys = queryKeysOf(evalDetailQuery{})
	evalItemsQueryKeys  = queryKeysOf(evalItemsQuery{})
)

// queryKeysOf is the keys params, a route's query struct, declares: each field's `query` tag.
func queryKeysOf(params any) []string {
	structure := reflect.TypeOf(params)
	keys := make([]string, 0, structure.NumField())
	for index := range structure.NumField() {
		if key, tagged := structure.Field(index).Tag.Lookup("query"); tagged {
			keys = append(keys, key)
		}
	}
	return keys
}

// undeclaredQueryKey refuses a query carrying a key its eval route does not take: 400
// INVALID_QUERY naming the first such key in name order, where declared is the keys the route's
// shape declares and the global session parameter is taken by every route; nil where every key
// is one of those. A read that passed over a key it does not take would answer a question other
// than the one asked: a caller still sending the old `scope=` would be answered every issue as if
// it had been answered the roots.
func undeclaredQueryKey(query url.Values, declared []string) error {
	for _, key := range slices.Sorted(maps.Keys(query)) {
		if key == evalSessionQueryKey || slices.Contains(declared, key) {
			continue
		}
		if len(declared) == 0 {
			return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is no query key of this route, which takes none but %s", key, evalSessionQueryKey)
		}
		return errorf(http.StatusBadRequest, "INVALID_QUERY", "%q is no query key of this route, which takes %s and %s",
			key, strings.Join(declared, ", "), evalSessionQueryKey)
	}
	return nil
}

// evalRange is a range of whole UTC days an answer folded: [from, to), both UTC midnights, and how
// many days that is.
type evalRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	Days int       `json:"days"`
}

func newEvalRange(days evals.Range) evalRange {
	return evalRange{From: days.From, To: days.To, Days: int(days.To.Sub(days.From) / (24 * time.Hour))}
}

// evalCompareRange is the answer's compare range, null where the request named none.
func evalCompareRange(compare *evals.Range) *evalRange {
	if compare == nil {
		return nil
	}
	return new(newEvalRange(*compare))
}

// evalFreshness is how fresh the day rows an answer folded are: the oldest updated_at among them
// (null where it folded none), and how long ago the day-row refresher's last pass committed
// (evals.Store.RefresherAge), in seconds, null where no pass has run on this database. The
// refresher is the deployment's one, so its age says how fresh the computed evals' rows are and
// never whether a project has any.
type evalFreshness struct {
	RowsUpdatedAt       *time.Time `json:"rows_updated_at"`
	RefresherAgeSeconds *int       `json:"refresher_age_seconds"`
}

// evalOwner is who an eval answers to: the Legion role and the person its owner columns name,
// and the writer of its current version (written_by).
type evalOwner struct {
	Role      string      `json:"role"`
	Person    string      `json:"person"`
	WrittenBy model.Actor `json:"written_by"`
}

// evalDayPoint is one day of a headline series' per-day array: the day (YYYY-MM-DD, UTC) and the
// series' statistics over that day's rows alone, through the same fold as the window's.
type evalDayPoint struct {
	Day        string           `json:"day"`
	Statistics evals.Statistics `json:"statistics"`
}

// evalSeriesValues is one listed series of an eval as a read states it: its current definition,
// its statistics over the window, over the compare range (null with none) and the change from the
// compare range to the window (each statistic the window's less the compare range's, null where
// either has no value; null with no compare range); data_since, the first day the series holds a
// day row (YYYY-MM-DD, null for a series with none), before which a range reads "no data", every
// statistic null; via_repository, how many of the rows the window's day rows count reached the
// project through a repository rather than an issue; and days, the window's per-day array of a
// headline series, one point per day that holds a row of it ([] for a control series).
type evalSeriesValues struct {
	evals.Series
	Window        evals.Statistics `json:"window"`
	Compare       evals.Statistics `json:"compare"`
	Change        evals.Statistics `json:"change"`
	DataSince     *string          `json:"data_since"`
	ViaRepository int              `json:"via_repository"`
	Days          []evalDayPoint   `json:"days"`
}

// evalJudge is one statistic a target judges, `<series>.<stat>`, its value over the window and
// whether it meets the target (null where it has no value, or the eval names no direction).
type evalJudge struct {
	Judge string   `json:"judge"`
	Value *float64 `json:"value"`
	Met   *bool    `json:"met"`
}

// evalVerdict is an eval's target judged over the window: the target, its direction and
// inclusiveness, each statistic it judges (target_judges, or the primary series' headline
// statistic where it names none), and met, false where any judge misses, true where every judge
// meets, null where none misses and one cannot be judged.
type evalVerdict struct {
	Target    float64     `json:"target"`
	Direction string      `json:"direction"`
	Inclusive bool        `json:"inclusive"`
	Met       *bool       `json:"met"`
	Judges    []evalJudge `json:"judges"`
}

// evalItem is one eval as the list read states it, and one version of one as the detail read
// does: the definition (every field of eval_versions, R1), its owner, each listed series with its
// values, and the verdict of its target (null where the target is off). The detail read states
// the series and the verdict of the newest version alone; an older version's are null.
type evalItem struct {
	Definition evals.Definition   `json:"definition"`
	Owner      evalOwner          `json:"owner"`
	Series     []evalSeriesValues `json:"series"`
	Verdict    *evalVerdict       `json:"verdict"`
}

// evalListItem is one eval of the list read: its id, `<project_key>/<slug>` (the definition's
// eval_id), and the eval as evalItem states it. The id is what a page and the live hub's evals
// view (live_frames.go) tell the list's evals apart by, as every list a live view sends item by
// item carries one; the detail read's items, versions of one eval, carry none.
type evalListItem struct {
	ID string `json:"id"`
	evalItem
}

// evalListResponse is GET /api/v1/projects/{key}/evals and GET /api/v1/evals: one page of evals,
// by project and then by slug, with the window and the compare range they were folded over and
// how fresh the folded rows are.
type evalListResponse struct {
	Items     []evalListItem `json:"items"`
	Total     int            `json:"total"`
	Limit     int            `json:"limit"`
	Offset    int            `json:"offset"`
	Window    evalRange      `json:"window"`
	Compare   *evalRange     `json:"compare"`
	Freshness evalFreshness  `json:"freshness"`
}

// evalDetailResponse is GET /api/v1/projects/{key}/evals/{slug}: one page of the eval's versions,
// newest first, the newest carrying the window's values, and doc_text, the method its section of
// the project's `evals-md` holds (null where the document holds no section for it).
type evalDetailResponse struct {
	Items     []evalItem    `json:"items"`
	Total     int           `json:"total"`
	Limit     int           `json:"limit"`
	Offset    int           `json:"offset"`
	Window    evalRange     `json:"window"`
	Compare   *evalRange    `json:"compare"`
	Freshness evalFreshness `json:"freshness"`
	DocText   *string       `json:"doc_text"`
}

// evalLinkInput is one link a write gives: its relation, its absolute address and its label. The
// server sets when it was stored, and a link carrying recorded_at is refused.
type evalLinkInput struct {
	Rel   string `json:"rel" required:"true"`
	Href  string `json:"href" required:"true"`
	Label string `json:"label"`
}

// evalReadingInput is one reading of a readings write, the keys a reading's row may carry
// (evals.Batch). The server sets id, eval_id, recorded_by, recorded_at and superseded_at, and a
// row carrying any of them is refused, naming the row (READING_FIELD_SERVER_SET).
type evalReadingInput struct {
	Series            string          `json:"series" required:"true"`
	RowKey            string          `json:"row_key"`
	RowKind           string          `json:"row_kind" required:"true"`
	Col               string          `json:"col"`
	Value             *float64        `json:"value"`
	ValueAbsentReason string          `json:"value_absent_reason"`
	Unit              string          `json:"unit"`
	N                 *int64          `json:"n"`
	NUnit             string          `json:"n_unit"`
	CILow             *float64        `json:"ci_low"`
	CIHigh            *float64        `json:"ci_high"`
	CILevel           *float64        `json:"ci_level"`
	Attributes        map[string]any  `json:"attributes"`
	Links             []evalLinkInput `json:"links"`
	MeasuredAt        time.Time       `json:"measured_at" required:"true"`
	Source            string          `json:"source"`
	Note              string          `json:"note"`
	Replaces          string          `json:"replaces"`
}

// evalReadingsInput is POST /api/v1/projects/{key}/evals/{slug}/readings's body: the batch, and
// the acting session for a bearer caller.
type evalReadingsInput struct {
	Actor    *model.Actor       `json:"actor"`
	Readings []evalReadingInput `json:"readings" required:"true"`
}

// evalReadingsBody is the readings write's body as its handler decodes it: each reading as the
// caller sent it, so the registry reads every key a row carries, those the server sets included
// (evals.Batch). evalReadingsInput is the same body as a client writes it.
type evalReadingsBody struct {
	Actor    *model.Actor      `json:"actor"`
	Readings []json.RawMessage `json:"readings"`
}

// evalReadingsResponse is the readings write's answer: the readings stored, in the batch's order,
// the eval's definition, and whether this write registered it (R3).
type evalReadingsResponse struct {
	Items      []evals.Reading  `json:"items"`
	Total      int              `json:"total"`
	Limit      int              `json:"limit"`
	Offset     int              `json:"offset"`
	Definition evals.Definition `json:"definition"`
	Registered bool             `json:"registered"`
}

// evalSeriesInput is one series a register write registers or changes, by name; a field it leaves
// out keeps the newest version's value (evals.SeriesChange).
type evalSeriesInput struct {
	Series    string          `json:"series" required:"true"`
	Unit      string          `json:"unit"`
	Role      string          `json:"role"`
	IsPrimary bool            `json:"is_primary"`
	MinN      *int            `json:"min_n"`
	Listed    bool            `json:"listed"`
	Agg       string          `json:"agg"`
	Links     []evalLinkInput `json:"links"`
}

// evalRegisterInput is POST /api/v1/projects/{key}/evals's body: the eval by slug, and each field
// the write sets. A field the body leaves out keeps the newest version's value, or the default of
// an eval registered here first (evals.Registration); a field the body names sets it, `target:
// null` turning the target off. doc_text, when given, is the eval's new method, written into its
// section of the project's `evals-md`.
type evalRegisterInput struct {
	Actor           *model.Actor        `json:"actor"`
	Slug            string              `json:"slug" required:"true"`
	Name            string              `json:"name"`
	Question        string              `json:"question"`
	NumberRule      string              `json:"number_rule"`
	Decision        string              `json:"decision"`
	DocBlock        string              `json:"doc_block"`
	DocText         string              `json:"doc_text"`
	FailureModes    []evals.FailureMode `json:"failure_modes"`
	Unit            string              `json:"unit"`
	Direction       string              `json:"direction"`
	Target          *float64            `json:"target"`
	TargetInclusive bool                `json:"target_inclusive"`
	TargetJudges    []string            `json:"target_judges"`
	OwnerRole       string              `json:"owner_role"`
	Person          string              `json:"person"`
	IssueKey        string              `json:"issue_key"`
	Kind            string              `json:"kind"`
	Source          string              `json:"source"`
	State           string              `json:"state"`
	ReplacedBy      string              `json:"replaced_by"`
	Links           []evalLinkInput     `json:"links"`
	Series          []evalSeriesInput   `json:"series"`
}

// evalRegistered is what a register write left: the eval's newest definition, the newest version
// of each of its series, and whether the write stored a new version of the definition or of a
// series.
type evalRegistered struct {
	Definition evals.Definition `json:"definition"`
	Series     []evals.Series   `json:"series"`
	Wrote      bool             `json:"wrote"`
}

// evalRegisterResponse is the register write's answer: the one version it left.
type evalRegisterResponse struct {
	Items  []evalRegistered `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// registry is the eval registry over this server's database, writing `evals-md` through its
// document service and its events through its broker.
func (s *server) registry() *evals.Store {
	return evals.New(s.deps.Store, s.deps.Docs, s.deps.Events)
}

// evalRanges is the window and the compare range one eval read asked for: each as the whole UTC
// days it covers, and each as the instants it was asked for, [from, to), which the failing-jobs
// rows read as the Pipeline route reads the same two (pipelineWindowBetween).
type evalRanges struct {
	window      evals.Range
	compare     *evals.Range
	from, to    time.Time
	compareFrom time.Time
	compareTo   time.Time
}

// evalDaysOf is the UTC days [from, to) covers: from the midnight of from's day to the midnight
// after the day of to's last instant, so a window ending inside a day covers that day.
func evalDaysOf(from, to time.Time) evals.Range {
	first := utcDayOf(from)
	return evals.Range{From: first, To: utcDayOf(to.Add(-time.Nanosecond)).AddDate(0, 0, 1)}
}

// utcDayOf is the UTC midnight of the day t is on.
func utcDayOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// parseEvalRanges reads an eval read's window and compare range: from and to as every delivery
// route reads them (parseDeliveryWindow: RFC3339, the last 7 days by default, at most 84 days),
// and compare_from and compare_to, both or neither, read the same way and held to the same bound,
// though the two ranges need not be of one length.
func parseEvalRanges(query url.Values) (evalRanges, error) {
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		return evalRanges{}, err
	}
	ranges := evalRanges{window: evalDaysOf(from, to), from: from.UTC(), to: to.UTC()}
	if query.Has("compare_from") != query.Has("compare_to") {
		return evalRanges{}, errors.New("compare_from and compare_to come together: name both, or neither")
	}
	if !query.Has("compare_from") {
		return ranges, nil
	}
	compareFrom, err := parseOptionalRFC3339(query, "compare_from", time.Time{})
	if err != nil {
		return evalRanges{}, err
	}
	compareTo, err := parseOptionalRFC3339(query, "compare_to", time.Time{})
	if err != nil {
		return evalRanges{}, err
	}
	if !compareFrom.Before(compareTo) {
		return evalRanges{}, errors.New("compare_from must be before compare_to")
	}
	if compareTo.Sub(compareFrom) > maxDeliveryWindow {
		return evalRanges{}, errors.New("compare_from..compare_to must not span more than 84 days")
	}
	ranges.compare = new(evalDaysOf(compareFrom, compareTo))
	ranges.compareFrom, ranges.compareTo = compareFrom.UTC(), compareTo.UTC()
	return ranges, nil
}

// parseEvalPage reads an eval read's page: limit, 1 to evalPageMax (evalPageDefault where absent),
// and offset, from 0.
func parseEvalPage(query url.Values) (limit, offset int, err error) {
	limit = evalPageDefault
	if value, present, valid := parseQueryInt(query, "limit", 1, evalPageMax); present {
		if !valid {
			return 0, 0, fmt.Errorf("limit must be one integer from 1 to %d", evalPageMax)
		}
		limit = value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return 0, 0, errors.New("offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

// parseEvalRepos reads `repo`, repeatable: the repositories whose day rows a read folds in place
// of the project's own. An empty value names no repository and is refused.
func parseEvalRepos(query url.Values) ([]string, error) {
	repos := query["repo"]
	if slices.Contains(repos, "") {
		return nil, errors.New("repo names a repository, and an empty repo names none")
	}
	return repos, nil
}

// evalListRequest is one read of the eval list: the projects it reads, in order; the kind it keeps,
// "" for every kind; the repositories whose day rows it folds, none for the projects' own rows;
// the window and the compare range; and the page.
type evalListRequest struct {
	projects []string
	kind     string
	repos    []string
	ranges   evalRanges
	limit    int
	offset   int
}

// parseEvalListQuery reads the list read's query, every key but `project`, which only the
// cross-project read takes; declared is the keys the reading route takes (evalListQueryKeys,
// evalsQueryKeys), any other of which is refused (undeclaredQueryKey). A malformed key is 400
// INVALID_QUERY naming it.
func parseEvalListQuery(query url.Values, declared []string) (evalListRequest, error) {
	if err := undeclaredQueryKey(query, declared); err != nil {
		return evalListRequest{}, err
	}
	invalid := func(err error) (evalListRequest, error) {
		return evalListRequest{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s", err.Error())
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
	kind := query.Get("kind")
	if kind != "" && !slices.Contains(evalKinds, kind) {
		return invalid(fmt.Errorf("kind %q is not one of %s", kind, strings.Join(evalKinds, ", ")))
	}
	return evalListRequest{kind: kind, repos: repos, ranges: ranges, limit: limit, offset: offset}, nil
}

// listProjectEvals answers GET /api/v1/projects/{key}/evals: one page of the project's evals
// (readEvalList); 404 PROJECT_NOT_FOUND for a project no row holds.
func (s *server) listProjectEvals(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	request, err := parseEvalListQuery(r.URL.Query(), evalListQueryKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	project := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request.projects = []string{project}
	answer, err := s.readEvalList(r.Context(), request, time.Now().UTC())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// listEvals answers GET /api/v1/evals: the list read across projects, each item the shape the
// project read answers. `project`, repeatable, names the projects read, in the order named; with
// none named it reads every project the store holds, by key, and never picks one for its caller
// (R11). A named project no row holds is 404 PROJECT_NOT_FOUND naming it.
func (s *server) listEvals(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	request, err := parseEvalListQuery(query, evalsQueryKeys)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	named := query["project"]
	if slices.Contains(named, "") {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "project names a project, and an empty project names none")
		return
	}
	projects, err := s.evalProjects(r.Context(), named)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	request.projects = projects
	answer, err := s.readEvalList(r.Context(), request, time.Now().UTC())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// evalProjects is the projects a cross-project read reads: those named, each once in the order
// first named, or every project by key where none is named. A name no project holds is 404
// PROJECT_NOT_FOUND naming it.
func (s *server) evalProjects(ctx context.Context, named []string) ([]string, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	if len(named) == 0 {
		return keys, nil
	}
	var projects []string
	for _, key := range named {
		if !slices.Contains(keys, key) {
			return nil, errorf(http.StatusNotFound, "PROJECT_NOT_FOUND", "no project has the key %q", key)
		}
		if !slices.Contains(projects, key) {
			projects = append(projects, key)
		}
	}
	return projects, nil
}

// readEvalList is the list read: one page of the evals of request's projects, by project in the
// order given and then by slug (evals.Store.Current, through evals_current), each with its values
// over the window and the compare range (evalItemsOf). The page runs across the projects: total is
// every project's count, and offset counts from the first project's first eval. It is the body of
// both list routes, which the live hub's view of the evals reads too.
func (s *server) readEvalList(ctx context.Context, request evalListRequest, now time.Time) (evalListResponse, error) {
	registry := s.registry()
	answer := evalListResponse{
		Items: []evalListItem{}, Limit: request.limit, Offset: request.offset,
		Window: newEvalRange(request.ranges.window), Compare: evalCompareRange(request.ranges.compare),
	}
	var oldest *time.Time
	skip, want := request.offset, request.limit
	for _, project := range request.projects {
		// A page of the project's evals from skip on, of at most want; with want spent, a page of
		// one is read for the project's count alone.
		page, err := registry.Current(ctx, project, request.kind, max(want, 1), skip)
		if err != nil {
			return evalListResponse{}, err
		}
		answer.Total += page.Total
		skip = max(0, skip-page.Total)
		entries := page.Items[:min(len(page.Items), want)]
		want -= len(entries)
		if len(entries) == 0 {
			continue
		}
		items, folded, err := s.evalItemsOf(ctx, registry, project, entries, request.repos, request.ranges)
		if err != nil {
			return evalListResponse{}, err
		}
		for _, item := range items {
			answer.Items = append(answer.Items, evalListItem{ID: item.Definition.EvalID, evalItem: item})
		}
		oldest = olderOf(oldest, folded)
	}
	freshness, err := s.evalFreshness(ctx, registry, oldest, now)
	if err != nil {
		return evalListResponse{}, err
	}
	answer.Freshness = freshness
	return answer, nil
}

// olderOf is the older of two moments, either of which may be none.
func olderOf(a, b *time.Time) *time.Time {
	if a == nil || (b != nil && b.Before(*a)) {
		return b
	}
	return a
}

// evalFreshness is an answer's freshness: oldest, the oldest updated_at of the rows it folded,
// and the refresher's age at now.
func (s *server) evalFreshness(ctx context.Context, registry *evals.Store, oldest *time.Time, now time.Time) (evalFreshness, error) {
	freshness := evalFreshness{RowsUpdatedAt: oldest}
	age, found, err := registry.RefresherAge(ctx, now)
	if err != nil {
		return evalFreshness{}, err
	}
	if found {
		freshness.RefresherAgeSeconds = new(int(age / time.Second))
	}
	return freshness, nil
}

// evalItemsOf states each of entries, evals of project, with its values: one read of the day rows
// of the page's evals over the window and the compare range (evals.Store.DayWindow, the rows of
// repos where it names any), and one read of each series' first day (evals.Store.FirstDays). It
// answers the items, in entries' order, and the oldest updated_at of the rows it folded.
func (s *server) evalItemsOf(ctx context.Context, registry *evals.Store, project string, entries []evals.Entry, repos []string, ranges evalRanges) ([]evalItem, *time.Time, error) {
	ids := make([]string, len(entries))
	for index, entry := range entries {
		ids[index] = entry.Definition.EvalID
	}
	rows, err := registry.DayWindow(ctx, project, entries, repos, ranges.window, ranges.compare)
	if err != nil {
		return nil, nil, err
	}
	firsts, err := registry.FirstDays(ctx, project, ids)
	if err != nil {
		return nil, nil, err
	}
	byEval := map[string][]evals.WindowRow{}
	var oldest *time.Time
	if len(rows) > 0 {
		first := rows[0].UpdatedAt
		oldest = &first
	}
	for _, row := range rows {
		byEval[row.EvalID] = append(byEval[row.EvalID], row)
		if row.UpdatedAt.Before(*oldest) {
			*oldest = row.UpdatedAt
		}
	}
	items := make([]evalItem, len(entries))
	for index, entry := range entries {
		series := evalSeriesOf(entry, byEval[entry.Definition.EvalID], firsts[entry.Definition.EvalID], ranges)
		items[index] = evalItem{
			Definition: entry.Definition,
			Owner:      evalOwnerOf(entry.Definition),
			Series:     series,
			Verdict:    evalVerdictOf(entry, series),
		}
	}
	return items, oldest, nil
}

func evalOwnerOf(definition evals.Definition) evalOwner {
	return evalOwner{Role: definition.OwnerRole, Person: definition.Person, WrittenBy: definition.WrittenBy}
}

// evalSeriesOf is each listed series of entry with its values over rows, the eval's day rows of
// the window and the compare range, and firsts, each series' first day. A range that starts
// before a series' first day, or a series with no first day at all, reads "no data": every
// statistic of that range null, whatever rows the range holds; the per-day array still answers
// each day that holds a row (R12, D8c).
func evalSeriesOf(entry evals.Entry, rows []evals.WindowRow, firsts map[string]time.Time, ranges evalRanges) []evalSeriesValues {
	var listed, headline []evals.Series
	for _, series := range entry.Series {
		if !series.Listed {
			continue
		}
		listed = append(listed, series)
		if series.Role == evals.RoleHeadline {
			headline = append(headline, series)
		}
	}
	var windowParts, compareParts []measures.DayPart
	byDay := map[time.Time][]measures.DayPart{}
	via := map[string]int{}
	for _, row := range rows {
		part := row.Part
		if row.Range == evals.RangeCompare {
			compareParts = append(compareParts, part)
			continue
		}
		windowParts = append(windowParts, part)
		byDay[part.Day] = append(byDay[part.Day], part)
		via[part.Series] += part.ViaRepository
	}
	definition := entry.Definition
	window := evals.FoldStatistics(definition, listed, windowParts, ranges.window)
	var compare map[string]evals.Statistics
	if ranges.compare != nil {
		compare = evals.FoldStatistics(definition, listed, compareParts, *ranges.compare)
	}
	// Each day of the window folded alone, by the same fold, for the headline series' arrays.
	days := slices.SortedFunc(maps.Keys(byDay), func(a, b time.Time) int { return a.Compare(b) })
	perDay := make([]map[string]evals.Statistics, len(days))
	for index, day := range days {
		perDay[index] = evals.FoldStatistics(definition, headline, byDay[day], evals.Range{From: day, To: day.AddDate(0, 0, 1)})
	}
	values := make([]evalSeriesValues, len(listed))
	for index, series := range listed {
		first, held := firsts[series.Series]
		value := evalSeriesValues{Series: series, Window: window[series.Series], ViaRepository: via[series.Series], Days: []evalDayPoint{}}
		if held {
			value.DataSince = new(first.Format(time.DateOnly))
		}
		if !held || ranges.window.From.Before(first) {
			value.Window = noData(value.Window)
		}
		if ranges.compare != nil {
			value.Compare = compare[series.Series]
			if !held || ranges.compare.From.Before(first) {
				value.Compare = noData(value.Compare)
			}
			value.Change = evalChange(value.Window, value.Compare)
		}
		if series.Role == evals.RoleHeadline {
			for dayIndex, day := range days {
				if slices.ContainsFunc(byDay[day], func(part measures.DayPart) bool { return part.Series == series.Series }) {
					value.Days = append(value.Days, evalDayPoint{Day: day.Format(time.DateOnly), Statistics: perDay[dayIndex][series.Series]})
				}
			}
		}
		values[index] = value
	}
	return values
}

// noData is stats with every statistic null: a range the series has no data for.
func noData(stats evals.Statistics) evals.Statistics {
	cleared := make(evals.Statistics, len(stats))
	for name := range stats {
		cleared[name] = nil
	}
	return cleared
}

// evalChange is each statistic of window less compare's, null where either has no value.
func evalChange(window, compare evals.Statistics) evals.Statistics {
	change := make(evals.Statistics, len(window))
	for name, value := range window {
		change[name] = nil
		if against := compare[name]; value != nil && against != nil {
			change[name] = new(*value - *against)
		}
	}
	return change
}

// evalVerdictOf judges entry's target over its series' window statistics, null where the target
// is off. Each judge is `<series>.<stat>`, of target_judges or, where those are empty, of the
// primary series' headline statistic (evals.HeadlineStatistic); a judge naming a series or a
// statistic the read does not state has no value. A higher_better eval meets its target at or
// above it where it is inclusive and above it where it is strict, a lower_better eval at or below
// it and below it; an eval with no direction judges nothing.
func evalVerdictOf(entry evals.Entry, series []evalSeriesValues) *evalVerdict {
	definition := entry.Definition
	if definition.Target == nil {
		return nil
	}
	judges := definition.TargetJudges
	if len(judges) == 0 {
		if index := slices.IndexFunc(entry.Series, func(one evals.Series) bool { return one.IsPrimary }); index >= 0 {
			primary := entry.Series[index]
			judges = []string{primary.Series + "." + evals.HeadlineStatistic(definition, primary)}
		}
	}
	target := *definition.Target
	verdict := &evalVerdict{Target: target, Direction: definition.Direction, Inclusive: definition.TargetInclusive, Judges: []evalJudge{}}
	missed, unknown := false, len(judges) == 0
	for _, judge := range judges {
		name, stat, _ := strings.Cut(judge, ".")
		judged := evalJudge{Judge: judge}
		if index := slices.IndexFunc(series, func(one evalSeriesValues) bool { return one.Series.Series == name }); index >= 0 {
			judged.Value = series[index].Window[stat]
		}
		if judged.Value != nil {
			value := *judged.Value
			switch {
			case definition.Direction == evals.DirectionHigherBetter && definition.TargetInclusive:
				judged.Met = new(value >= target)
			case definition.Direction == evals.DirectionHigherBetter:
				judged.Met = new(value > target)
			case definition.Direction == evals.DirectionLowerBetter && definition.TargetInclusive:
				judged.Met = new(value <= target)
			case definition.Direction == evals.DirectionLowerBetter:
				judged.Met = new(value < target)
			}
		}
		switch {
		case judged.Met == nil:
			unknown = true
		case !*judged.Met:
			missed = true
		}
		verdict.Judges = append(verdict.Judges, judged)
	}
	switch {
	case missed:
		verdict.Met = new(false)
	case !unknown:
		verdict.Met = new(true)
	}
	return verdict
}

// getProjectEval answers GET /api/v1/projects/{key}/evals/{slug}: one page of the eval's versions,
// newest first, read with its current definition and series in one snapshot
// (evals.Store.EvalVersions), the newest stating the window's values as the list read states
// them, and the method its section of the project's `evals-md` holds (evals.Store.Method). A query
// key the route does not take is 400 INVALID_QUERY (undeclaredQueryKey), 404 PROJECT_NOT_FOUND for
// a project no row holds, 404 EVAL_NOT_FOUND for a slug no eval of it holds.
func (s *server) getProjectEval(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	if err := undeclaredQueryKey(query, evalDetailQueryKeys); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ranges, err := parseEvalRanges(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	repos, err := parseEvalRepos(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	limit, offset, err := parseEvalPage(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	project, slug := r.PathValue("key"), r.PathValue("slug")
	if _, err := s.loadProject(ctx, s.deps.Store.Pool, project); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	registry := s.registry()
	entry, versions, found, err := registry.EvalVersions(ctx, evals.EvalID(project, slug))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !found {
		s.writeHandlerError(w, evalNotFound(project, slug))
		return
	}
	answer := evalDetailResponse{
		Items: []evalItem{}, Total: len(versions), Limit: limit, Offset: offset,
		Window: newEvalRange(ranges.window), Compare: evalCompareRange(ranges.compare),
	}
	// The page's bounds, clamped before they are added: an offset up to math.MaxInt is valid, and
	// offset+limit would overflow.
	start := min(offset, len(versions))
	end := start + min(limit, len(versions)-start)
	var oldest *time.Time
	for _, version := range versions[start:end] {
		if version.Version != entry.Definition.Version {
			answer.Items = append(answer.Items, evalItem{Definition: version, Owner: evalOwnerOf(version)})
			continue
		}
		newest, folded, err := s.evalItemsOf(ctx, registry, project, []evals.Entry{entry}, repos, ranges)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		answer.Items, oldest = append(answer.Items, newest...), folded
	}
	if answer.Freshness, err = s.evalFreshness(ctx, registry, oldest, time.Now().UTC()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	method, found, err := registry.Method(ctx, project, entry.Definition.DocBlock)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if found {
		answer.DocText = &method
	}
	WriteJSON(w, http.StatusOK, answer)
}

// loadEval reads the eval slug names in project, current definition and series: 404
// PROJECT_NOT_FOUND for a project no row holds, 404 EVAL_NOT_FOUND for a slug no eval of it holds.
func (s *server) loadEval(ctx context.Context, project, slug string) (evals.Entry, error) {
	if _, err := s.loadProject(ctx, s.deps.Store.Pool, project); err != nil {
		return evals.Entry{}, err
	}
	entry, found, err := s.registry().Eval(ctx, evals.EvalID(project, slug))
	if err != nil {
		return evals.Entry{}, err
	}
	if !found {
		return evals.Entry{}, evalNotFound(project, slug)
	}
	return entry, nil
}

// evalNotFound is the 404 EVAL_NOT_FOUND of a slug no eval of project holds.
func evalNotFound(project, slug string) error {
	return errorf(http.StatusNotFound, codeEvalNotFound, "project %s holds no eval %q", project, slug)
}

// writeEvalError answers err: a registry refusal (*evals.Error) as its own body, {code, error}
// and for a readings write the refused rows, 404 for a project no row holds, 409 for a project
// whose `evals-md` slug names a file and 400 for every other; anything else as writeHandlerError
// does.
func (s *server) writeEvalError(w http.ResponseWriter, err error) {
	var refused *evals.Error
	if !errors.As(err, &refused) {
		s.writeHandlerError(w, err)
		return
	}
	status := http.StatusBadRequest
	switch refused.Code {
	case evals.CodeProjectNotFound:
		status = http.StatusNotFound
	case evals.CodeDocumentNotDocument:
		status = http.StatusConflict
	}
	WriteJSON(w, status, refused)
}

// logEvalReadings answers POST /api/v1/projects/{key}/evals/{slug}/readings: one batch of readings
// of one eval, stored in one transaction or not at all (evals.Store.WriteReadings), every refused
// row named by its index. A slug no eval holds registers its eval with the caller as owner (R3),
// and a reading under a computed eval's slug is stored and never folded into its value. The write
// needs an Idempotency-Key (400 IDEMPOTENCY_KEY_REQUIRED), so a retry is answered from the keyed
// layer (keyedWrites) rather than written twice (R4). It takes no query key but the `session`
// parameter every operation carries (actingPerson): a window means nothing to a write, so any
// other key is 400 INVALID_QUERY naming it (R7). The body's actor follows Dispatch's bearer rule
// (bearerSessionActor); the server sets recorded_by and recorded_at, and a row carrying either is
// refused (READING_FIELD_SERVER_SET). The day rows the write keeps record s.deps.Build as their
// writer.
func (s *server) logEvalReadings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	if err := undeclaredQueryKey(r.URL.Query(), nil); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, present, _ := requestKeyHeader(r); !present {
		writeError(w, codeIdempotencyKeyRequired, http.StatusBadRequest, fmt.Sprintf(
			"a readings write names its intent in an %s header, so a retry is answered with the first write's answer rather than stored twice",
			contracts.IdempotencyKeyHeader))
		return
	}
	var body evalReadingsBody
	if err := decodeJSON(r, &body); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, body.Actor)
	if !ok {
		return
	}
	written, err := s.registry().WriteReadings(r.Context(), evals.Batch{
		Project: r.PathValue("key"), Slug: r.PathValue("slug"), Rows: body.Readings, Caller: actor, BuiltBy: s.deps.Build,
	})
	if err != nil {
		s.writeEvalError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, evalReadingsResponse{
		Items: written.Readings, Total: len(written.Readings), Limit: len(written.Readings),
		Definition: written.Definition, Registered: written.Registered,
	})
}

// refuseReadingChange answers PUT and DELETE /api/v1/projects/{key}/evals/{slug}/readings/{id}
// with 405 READING_APPEND_ONLY, whatever the id names: a reading is never updated or deleted, and
// a correction is a new reading naming the one it replaces (R4). The resource allows no method,
// which an empty Allow header says.
func (s *server) refuseReadingChange(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	w.Header().Set("Allow", "")
	writeError(w, codeReadingAppendOnly, http.StatusMethodNotAllowed,
		"a reading is append-only: a correction is a new reading, POSTed to the eval's readings with replaces naming the reading it corrects")
}

// registerEval answers POST /api/v1/projects/{key}/evals: the next version of one eval's
// definition, and of the series the body names, written in one transaction with every field the
// body leaves out copied forward (evals.Store.Register). doc_text, when given and not the method
// the eval's section of the project's `evals-md` holds already, replaces that method. It answers
// 201 with what it wrote, a version of the definition or of a series, and 200 with the versions
// that stand where the body changed no field of the definition or of a series it names.
func (s *server) registerEval(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	change, supplied, err := decodeEvalRegistration(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, supplied)
	if !ok {
		return
	}
	change.Project, change.Caller = r.PathValue("key"), actor
	registered, err := s.registry().Register(r.Context(), change)
	if err != nil {
		s.writeEvalError(w, err)
		return
	}
	status := http.StatusOK
	if registered.Wrote {
		status = http.StatusCreated
	}
	WriteJSON(w, status, evalRegisterResponse{
		Items: []evalRegistered{{Definition: registered.Definition, Series: registered.Series, Wrote: registered.Wrote}},
		Total: 1, Limit: 1,
	})
}

// decodeEvalRegistration reads a register write's body into the registration it asks for: each
// field the body names is set, `null` included (a null target is off), and each it leaves out is
// left unset, so the registry copies it forward. The body is decoded twice, once into its type,
// which refuses a key no field has, and once by key, which says which keys it named.
func decodeEvalRegistration(r *http.Request) (evals.Registration, *model.Actor, error) {
	var raw json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		return evals.Registration{}, nil, err
	}
	var input evalRegisterInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return evals.Registration{}, nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(raw, &named); err != nil {
		return evals.Registration{}, nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	change := evals.Registration{
		Slug:            input.Slug,
		Name:            fieldOf(named, "name", input.Name),
		Question:        fieldOf(named, "question", input.Question),
		NumberRule:      fieldOf(named, "number_rule", input.NumberRule),
		Decision:        fieldOf(named, "decision", input.Decision),
		DocBlock:        fieldOf(named, "doc_block", input.DocBlock),
		DocText:         fieldOf(named, "doc_text", input.DocText),
		FailureModes:    fieldOf(named, "failure_modes", input.FailureModes),
		Unit:            fieldOf(named, "unit", input.Unit),
		Direction:       fieldOf(named, "direction", input.Direction),
		Target:          fieldOf(named, "target", input.Target),
		TargetInclusive: fieldOf(named, "target_inclusive", input.TargetInclusive),
		TargetJudges:    fieldOf(named, "target_judges", input.TargetJudges),
		OwnerRole:       fieldOf(named, "owner_role", input.OwnerRole),
		Person:          fieldOf(named, "person", input.Person),
		IssueKey:        fieldOf(named, "issue_key", input.IssueKey),
		Kind:            fieldOf(named, "kind", input.Kind),
		Source:          fieldOf(named, "source", input.Source),
		State:           fieldOf(named, "state", input.State),
		ReplacedBy:      fieldOf(named, "replaced_by", input.ReplacedBy),
		Links:           fieldOf(named, "links", evalLinks(input.Links)),
	}
	if len(input.Series) > 0 {
		var namedSeries []map[string]json.RawMessage
		if err := json.Unmarshal(named["series"], &namedSeries); err != nil {
			return evals.Registration{}, nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: series: %v", err)
		}
		for index, series := range input.Series {
			keys := namedSeries[index]
			change.Series = append(change.Series, evals.SeriesChange{
				Series:    series.Series,
				Unit:      fieldOf(keys, "unit", series.Unit),
				Role:      fieldOf(keys, "role", series.Role),
				IsPrimary: fieldOf(keys, "is_primary", series.IsPrimary),
				MinN:      fieldOf(keys, "min_n", series.MinN),
				Listed:    fieldOf(keys, "listed", series.Listed),
				Agg:       fieldOf(keys, "agg", series.Agg),
				Links:     fieldOf(keys, "links", evalLinks(series.Links)),
			})
		}
	}
	return change, input.Actor, nil
}

// fieldOf is value as a field of a register write: set where the body named key, unset where it
// left key out.
func fieldOf[T any](named map[string]json.RawMessage, key string, value T) evals.Field[T] {
	if _, set := named[key]; set {
		return evals.Set(value)
	}
	return evals.Field[T]{}
}

// evalLinks is links as the registry stores them, each recorded_at the registry's to set.
func evalLinks(links []evalLinkInput) []evals.Link {
	stored := make([]evals.Link, len(links))
	for index, link := range links {
		stored[index] = evals.Link{Rel: link.Rel, Href: link.Href, Label: link.Label}
	}
	return stored
}
