// project_timeline.go serves GET /api/v1/projects/{key}/timeline and the live hub's
// project-timeline view: one project's rows of the delivery timeline, newest first, a page at a
// time (dispatch://INFRA-2918, D8). The rows are the base timeline's (deliveryTimelineOf), computed
// over the rows the project counts by root R27, which the eval registry's day rows count them by
// (evals.Attribution), and carry no measures: a page of numbers reads those from the eval routes.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

const (
	// projectTimelinePageDefault and projectTimelinePageMax bound the route's page: limit is 1 to
	// 500, 200 where the query names none.
	projectTimelinePageDefault = 200
	projectTimelinePageMax     = 500
	// projectTimelineViewMax bounds a project-timeline view's limit: the most rows a window can
	// hold, its pull requests and its runs each bounded as the base bounds the timeline's
	// population. A view grows by its limit up to the window's total, past the route's page.
	projectTimelineViewMax = delivery.MaxPullRequestsPerWindow + delivery.MaxRunsPerWindow
)

// The kinds of row, in the order the rows of one moment are listed.
const (
	projectTimelineMerge     = "merge"
	projectTimelineDeploy    = "deploy"
	projectTimelineFailedRun = "failed_run"
)

var projectTimelineKinds = []string{projectTimelineMerge, projectTimelineDeploy, projectTimelineFailedRun}

// projectTimelineOneValueKeys are the keys the route reads one value of (repeatedQueryKey): from,
// to, q, limit and offset; every facet stays repeatable.
var projectTimelineOneValueKeys = oneValueQueryKeys(projectTimelineQuery{})

// projectTimelineResponse is GET /api/v1/projects/{key}/timeline: one page of the project's
// timeline rows of the window, newest first, in the envelope every paged read answers (root
// R26), and beside it what labels and counts them. The facet and colour counts are the whole
// window's, on every page. Waiting, the project's pull requests waiting at the window's start, is
// on the page that reaches that start (every row from offset on fits in limit) and null on every
// other; the issue titles, session titles, session pull-request counts, components and session
// groups are the whole window's, on the first page (offset 0), and null on every other. A page is
// the base timeline's answer cut: its rows hold every field the base's prs[] and runs[] hold, and
// no measure.
type projectTimelineResponse struct {
	Items  []projectTimelineRow `json:"items"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
	// Window is the [from, to) the page is of.
	Window          delivery.DeliveryWindowView               `json:"window"`
	Waiting         []delivery.DeliveryWaitingPRView          `json:"waiting"`
	FacetCounts     map[string]map[string]int                 `json:"facet_counts"`
	ColorCounts     map[string]map[string]int                 `json:"color_counts"`
	Components      map[string]delivery.DeliveryComponentView `json:"components"`
	IssueTitles     map[string]string                         `json:"issue_titles"`
	SessionGroups   []delivery.SessionGroup                   `json:"session_groups"`
	SessionTitles   map[string]string                         `json:"session_titles"`
	SessionPRCounts map[string]int                            `json:"session_pr_counts"`
	Freshness       delivery.DeliveryFreshnessView            `json:"freshness"`
}

// projectTimelineMergeRow is a merge: one of the base timeline's prs[], by its pull request id.
type projectTimelineMergeRow struct {
	Kind string `json:"kind" enum:"merge"`
	delivery.DeliveryPRView
}

// projectTimelineRunRow is a deploy run started in the window: one of the base timeline's runs[],
// by its run id, with the repository the run is in, which a read of its jobs names, and when
// GitHub created it, which orders it among the rows. A failed_run is one the timeline draws as a
// pipeline failure, a job of it failed or its production job did; every other run is a deploy.
type projectTimelineRunRow struct {
	Kind string `json:"kind" enum:"deploy,failed_run"`
	delivery.DeliveryRunView
	Repo      string    `json:"repo"`
	CreatedAt time.Time `json:"created_at"`
}

// projectTimelineRow is one row of the project timeline, a merge or a run, written as the one it
// holds; its schema is one of the two.
type projectTimelineRow struct {
	merge *projectTimelineMergeRow
	run   *projectTimelineRunRow
}

// MarshalJSON writes the row it holds.
func (row projectTimelineRow) MarshalJSON() ([]byte, error) {
	if row.merge != nil {
		return json.Marshal(row.merge)
	}
	return json.Marshal(row.run)
}

// JSONSchemaOneOf is the two shapes a row is written as, for the OpenAPI document.
func (projectTimelineRow) JSONSchemaOneOf() []any {
	return []any{projectTimelineMergeRow{}, projectTimelineRunRow{}}
}

// projectTimelineEntry is one row with what orders it: its time (a merge's merged_at, a run's
// creation), its kind's place in projectTimelineKinds, and its id.
type projectTimelineEntry struct {
	at    time.Time
	kind  int
	merge string
	run   int64
	row   projectTimelineRow
}

// compareProjectTimelineEntries is the timeline's one total order: newest first, then by kind
// (merge, deploy, failed_run), then by id.
func compareProjectTimelineEntries(a, b projectTimelineEntry) int {
	return cmp.Or(b.at.Compare(a.at), cmp.Compare(a.kind, b.kind), strings.Compare(a.merge, b.merge), cmp.Compare(a.run, b.run))
}

// projectTimelineRead is what one read of the project timeline holds: the delivery rules, the
// project's population (projectTimelinePopulation), and the page computed from them.
type projectTimelineRead struct {
	rules *delivery.SourceRules
	pop   deliveryPopulation
	body  projectTimelineResponse
}

// parseProjectTimelinePage reads the page of a project timeline query: limit, 1 to maxLimit
// (projectTimelinePageDefault where absent), and offset, from 0, after refusing a query that names
// a key the route reads one value of more than once. A malformed one is 400 INVALID_QUERY.
func parseProjectTimelinePage(query url.Values, maxLimit int) (limit, offset int, err error) {
	if err := repeatedQueryKey(query, projectTimelineOneValueKeys); err != nil {
		return 0, 0, err
	}
	return parseOffsetPage(query, projectTimelinePageDefault, maxLimit)
}

// getProjectTimeline answers GET /api/v1/projects/{key}/timeline: one page of the timeline rows the
// project its path names counts, over [from, to) (default the last 7 days), under the facets and
// the search the delivery timeline takes (readProjectTimeline). A key that is no project key is
// 400 INVALID_PROJECT, a malformed query 400 INVALID_QUERY, a project no row holds 404
// PROJECT_NOT_FOUND, and a deployment with no project with delivery on 404
// DELIVERY_NOT_CONFIGURED.
func (s *server) getProjectTimeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	project := strings.TrimSpace(r.PathValue("key"))
	if !projectKeyPattern.MatchString(project) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("project", project))
		return
	}
	query := r.URL.Query()
	limit, offset, err := parseProjectTimelinePage(query, projectTimelinePageMax)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	from, to, err := parseDeliveryWindow(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	read, err := s.readProjectTimeline(r.Context(), project, from, to, query, limit, offset)
	switch {
	case errors.Is(err, delivery.ErrNoSettings):
		writeError(w, deliveryNotConfiguredCode, http.StatusNotFound, deliveryNotConfiguredMessage)
	case err != nil:
		s.writeHandlerError(w, err)
	default:
		WriteJSON(w, http.StatusOK, read.body)
	}
}

// readProjectTimeline is GET /api/v1/projects/{key}/timeline's body for project over [from, to),
// query's facets and search, and the page of limit rows from offset: the project found
// (loadProject, 404 PROJECT_NOT_FOUND for a project no row holds), the delivery rules of every
// source repository (readDeliveryRules, delivery.ErrNoSettings where no project has delivery on),
// the project's population of the window (projectTimelinePopulation), and the page computed from
// it (projectTimelineCompute). The live hub's project-timeline view reads its body through this
// too (readProjectTimelineView), so the two answer one query alike, and keeps the rules and the
// population it answers with.
func (s *server) readProjectTimeline(ctx context.Context, project string, from, to time.Time, query url.Values, limit, offset int) (projectTimelineRead, error) {
	pool := s.deps.Store.Pool
	if _, err := s.loadProject(ctx, pool, project); err != nil {
		return projectTimelineRead{}, err
	}
	rules, err := readDeliveryRules(ctx, pool)
	if err != nil {
		return projectTimelineRead{}, err
	}
	pop, err := s.projectTimelinePopulation(ctx, rules, project, from, to, query)
	if err != nil {
		return projectTimelineRead{}, err
	}
	return projectTimelineRead{rules: rules, pop: pop, body: projectTimelineCompute(rules, pop, from, to, limit, offset)}, nil
}

// projectTimelinePopulation is the project's population of [from, to): the delivery timeline's read
// of the window (readDeliveryPopulation, with its waiting line and the pull requests each of its
// deploys ships first), kept to the rows project counts by root R27, by the rule the eval
// registry's day rows count them by (evals.Attribution, read for every issue those rows name): a
// pull request merged in the window or waiting at its start that counts for project, through its
// issue or, naming none, through its repository as project's published default; and a deploy run
// started in the window that counts for it by the pull requests it ships first, or, shipping none
// (a failed run among them), as the project whose deploy source reads its repository. The runs
// started outside the window stay, since a pull request of the window can ship with one; a run of
// the window that counts for other projects alone ships none of project's first.
func (s *server) projectTimelinePopulation(ctx context.Context, rules *delivery.SourceRules, project string, from, to time.Time, query url.Values) (deliveryPopulation, error) {
	pop, err := s.readDeliveryPopulation(ctx, rules, from, to, query, true)
	if err != nil {
		return deliveryPopulation{}, err
	}
	prs := slices.Clone(pop.prs)
	for _, pr := range pop.waiting {
		prs = append(prs, pr.DeliveryPullRequest)
	}
	for _, shipped := range pop.windowShipped {
		for _, pr := range shipped {
			prs = append(prs, *pr)
		}
	}
	attribution, err := s.registry().ReadAttribution(ctx, prs)
	if err != nil {
		return deliveryPopulation{}, fmt.Errorf("read the projects the timeline's rows count for: %w", err)
	}

	kept := pop
	kept.prs = slices.DeleteFunc(slices.Clone(pop.prs), func(pr delivery.DeliveryPullRequest) bool {
		return !attribution.CountsPullRequest(pr, project)
	})
	kept.waiting = slices.DeleteFunc(slices.Clone(pop.waiting), func(pr delivery.WaitingPullRequest) bool {
		return !attribution.CountsPullRequest(pr.DeliveryPullRequest, project)
	})
	kept.applyRuns, kept.jobsByRun = runsWhere(pop.applyRuns, pop.jobsByRun, func(run delivery.DeliveryRun) bool {
		return run.StartedAt.Before(from) || !run.StartedAt.Before(to) || attribution.CountsDeploy(run.Repo, pop.windowShipped[run.RunID], project)
	})
	// The measures' runs are no row of the timeline, and would move its expiry (nextExpiry).
	kept.windowRuns, kept.windowJobs, kept.windowShipped = nil, nil, nil
	return kept, nil
}

// projectTimelineCompute is GET /api/v1/projects/{key}/timeline's body for [from, to) from the
// project's population (projectTimelinePopulation), read for that window or narrowed to it: the
// base timeline's answer over it (deliveryTimelineOf), cut to the page (projectTimelinePage). It
// reads nothing.
func projectTimelineCompute(rules *delivery.SourceRules, pop deliveryPopulation, from, to time.Time, limit, offset int) projectTimelineResponse {
	runs := make(map[int64]delivery.DeliveryRun, len(pop.applyRuns))
	for _, run := range pop.applyRuns {
		runs[run.RunID] = run
	}
	return projectTimelinePage(deliveryTimelineOf(rules, pop, pop.derive(rules), from, to), runs, limit, offset)
}

// projectTimelinePage is a timeline's merges and runs listed as rows in the one total order
// (compareProjectTimelineEntries) and cut to [offset, offset+limit), with the members each page
// carries beside them (projectTimelineResponse). runs holds every run the timeline lists, by id,
// for the repository and the creation each run row names.
func projectTimelinePage(timeline delivery.DeliveryTimelineResponse, runs map[int64]delivery.DeliveryRun, limit, offset int) projectTimelineResponse {
	entries := make([]projectTimelineEntry, 0, len(timeline.PRs)+len(timeline.Runs))
	for _, pr := range timeline.PRs {
		entries = append(entries, projectTimelineEntry{
			at: *pr.MergedAt, kind: slices.Index(projectTimelineKinds, projectTimelineMerge), merge: pr.ID,
			row: projectTimelineRow{merge: &projectTimelineMergeRow{Kind: projectTimelineMerge, DeliveryPRView: pr}},
		})
	}
	for _, view := range timeline.Runs {
		run := runs[view.ID]
		kind := projectTimelineDeploy
		if len(view.FailedJobs) > 0 || (view.Production != nil && view.Production.Conclusion != nil && *view.Production.Conclusion == delivery.DeliveryJobConclusionFailure) {
			kind = projectTimelineFailedRun
		}
		entries = append(entries, projectTimelineEntry{
			at: run.Created(), kind: slices.Index(projectTimelineKinds, kind), run: view.ID,
			row: projectTimelineRow{run: &projectTimelineRunRow{Kind: kind, DeliveryRunView: view, Repo: run.Repo, CreatedAt: run.Created()}},
		})
	}
	slices.SortFunc(entries, compareProjectTimelineEntries)

	total := len(entries)
	start := min(offset, total)
	end := start + min(limit, total-start)
	items := make([]projectTimelineRow, 0, end-start)
	for _, entry := range entries[start:end] {
		items = append(items, entry.row)
	}
	answer := projectTimelineResponse{
		Items: items, Total: total, Limit: limit, Offset: offset,
		Window: timeline.Window, FacetCounts: timeline.FacetCounts, ColorCounts: timeline.ColorCounts,
		Freshness: timeline.Freshness,
	}
	if limit >= total-offset {
		answer.Waiting = timeline.Waiting
	}
	if offset == 0 {
		answer.IssueTitles, answer.SessionTitles, answer.SessionPRCounts = timeline.IssueTitles, timeline.SessionTitles, timeline.SessionPRCounts
		answer.Components, answer.SessionGroups = timeline.Components, timeline.SessionGroups
	}
	return answer
}

// parseProjectTimelineLiveView reads a project-timeline view: the project whose timeline it shows,
// a key that is no project key refused as the route refuses it (INVALID_PROJECT); its window, as a
// timeline view names one; and its query, the facets and the search as the route reads them and
// limit, the rows the view holds from the newest, 1 to projectTimelineViewMax (the route's page
// default where it names none). The view grows by its limit, so it names no offset: its page
// always starts at the newest row. A query the route refuses is refused here, by the route's own
// reading of it (parseProjectTimelinePage), before the view is held.
func parseProjectTimelineLiveView(spec *deliveryLiveSpec, view DeliveryLiveView) *LiveError {
	project, refusal := readLiveProject(view, "a project-timeline view", "INVALID_PROJECT")
	if refusal != nil {
		return refusal
	}
	named, refusal := spec.readWindowAndQuery(view, "a project-timeline view")
	if refusal != nil {
		return refusal
	}
	if _, _, err := parseProjectTimelinePage(spec.query, projectTimelineViewMax); err != nil {
		var refused *apiError
		if errors.As(err, &refused) {
			return invalidLiveView(refused.code, refused.message)
		}
		return invalidLiveView("INVALID_QUERY", err.Error())
	}
	if spec.query.Has("offset") {
		return invalidLiveView("INVALID_QUERY", "a project-timeline view holds its rows from the newest and grows by its limit, so it names no offset")
	}
	spec.project = project
	spec.key = deliveryLiveProjectTimeline + "|" + project + "|" + named + "|" + spec.query.Encode()
	return nil
}

// projectTimelineLimit is the limit a project-timeline view's query names, as its parse read it.
func (spec deliveryLiveSpec) projectTimelineLimit() int {
	limit, _, _ := parseProjectTimelinePage(spec.query, projectTimelineViewMax)
	return limit
}

// readProjectTimelineView is a project-timeline view's read: the route's body for the view's
// project, window, facets and search, its limit of rows from the newest, through the function the
// route answers with (readProjectTimeline), so the body is the route's for the same query, byte for
// byte. A project no row holds is refused as the route refuses it (viewReads.refusal), and so is a
// deployment with no project with delivery on. It holds the rules and the population the body was
// computed from, as a timeline view does: they judge the freshness, the issues and sessions its
// rows name mark it (deliveryHub.nameShown), and an expiry narrows the population (expire).
func (h *deliveryHub) readProjectTimelineView(ctx context.Context, spec deliveryLiveSpec, now time.Time, read *deliveryViewRead) (any, error) {
	from, to := spec.from, spec.to
	if spec.preset > 0 {
		from, to = now.Add(-spec.days()), now
	}
	answer, err := h.server.readProjectTimeline(ctx, spec.project, from, to, spec.query, spec.projectTimelineLimit(), 0)
	if err != nil {
		return nil, err
	}
	read.rules, read.pop = answer.rules, answer.pop
	if err := h.nameShown(ctx, read); err != nil {
		return nil, err
	}
	if spec.preset > 0 {
		read.expiry = nextExpiry(read.pop, spec.days(), now)
	}
	read.from = from
	read.prs, read.runs = len(read.pop.prs), len(read.pop.applyRuns)
	read.unfetchable = read.pop.unfetchable
	return answer.body, nil
}
