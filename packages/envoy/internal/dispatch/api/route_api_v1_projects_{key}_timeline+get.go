package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/timeline",
	Auth:        authAny,
	Description: fmt.Sprintf("One page of the delivery timeline's rows the project its path names counts, newest first, {items, total, limit, offset} (limit 1-%d, default %d; offset from 0), within [from, to) (default the last 7 days), the search q and the facets the delivery timeline takes (repeatable repo, parent_agent, session, issue, priority, component, author, rework, deployed): each row a merge (kind merge, every field of the delivery timeline's prs[]) or a deploy run started in the window (kind deploy, or failed_run for a run with a failed job or a failed production job, every field of its runs[], with repo, the repository the run is in, and created_at), in one order: by the row's time (a merge's merged_at, a run's created_at), newest first, then by kind (merge, deploy, failed_run), then by id. The rows are those the project counts as the eval registry's day rows count them, under the published default repositories: a pull request for its issue's project, or, naming none, for each project whose default repository holds it; a deploy run for the projects the pull requests it ships first count for, or, shipping none (every failed run among them), for the project whose deploy source reads its repository. Beside them facet_counts and color_counts over the whole window on every page; waiting, the project's pull requests waiting at the window's start, on the page that reaches that start and null on every other; issue_titles, session_titles, session_pr_counts, components and session_groups over the whole window on the first page and null on every other; window and freshness. No measures: the eval routes answer those. The /ws/live project-timeline view carries the same body for its limit of rows from the newest. 400 INVALID_PROJECT for a key that is no project key, 400 INVALID_QUERY for a malformed query, 404 PROJECT_NOT_FOUND, 404 DELIVERY_NOT_CONFIGURED.", projectTimelinePageMax, projectTimelinePageDefault),
	Handler:     (*server).getProjectTimeline,
	// The GETs that write.
	Read: readEntry{Writes: readWritesState, WriteNote: "As the delivery timeline's, whose population it reads (readDeliveryPopulation): an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn, on conflict doing nothing)."},
	Shape: routeShape{
		Status: http.StatusOK, Response: projectTimelineResponse{}, Params: projectTimelineQuery{},
	},
})
