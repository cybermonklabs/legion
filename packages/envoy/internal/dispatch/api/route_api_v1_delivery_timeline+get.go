package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/timeline",
	Auth:        authAny,
	Description: "The delivery timeline within [from, to) (default the last 7 days), the search q (title or PR id) and the given facets (repeatable repo, parent_agent, session, issue, priority (P0-P3), component (a parent includes its children), author, rework, deployed; __no_issue__, __no_priority__, __no_component__ and __no_session__ select by cause): merges with each PR's issue title, priority and effective components, successful production deploys with every window PR they shipped first, pipeline failures with each run's production job and failing step, and merged work waiting to deploy, each PR's deployed_status (deployed, waiting, not_tracked) computed from stored facts; facet_counts (each facet counted with every other facet applied and its own ignored), color_counts (with none applied), the components and issue titles the window names, data freshness, and measures (what GET /api/v1/delivery/measures answers for the same window, search and facets). The runs are every deploy source's repository's of every project with delivery on, each judged by the rule its repository's stored eval day rows stand built under, as the Evals tab does, so a save of a source's rule moves it once the eval refresher has rewritten the rows the save moves. 400 INVALID_QUERY for a from or to that is no RFC3339 time or no moment of the UTC years 1 to 9999, a from not before to, a window over 84 days, or from, to or q named more than once; 404 DELIVERY_NOT_CONFIGURED when no project has delivery on.",
	Handler:     (*server).getDeliveryTimeline,
	// The GETs that write.
	Read: readEntry{Writes: readWritesState, WriteNote: "When a deploy run has no configured production job, or its production job has no configured apply step, the read inserts a delivery_run_warnings row on the request's context (firstToWarn, on conflict doing nothing), once per process for each run and configuration."},
	Shape: routeShape{
		Status: http.StatusOK, Response: deliveryTimelineWithMeasures{}, Params: deliveryWindowQuery{},
	},
})
