package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/pipeline",
	Auth:        authAny,
	Description: "The Pipeline page: over the seven UTC days ending today (now, or ?at= for a fixed day), by the day each run started, the PR-checks workflow's run wall time and success, its five slowest jobs (50 timed results to rank) and the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main, and merge-to-production and PR-open-to-merge per merge day against their targets; not filtered by the timeline's window or facets; 400 INVALID_QUERY for a malformed at, 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.",
	Handler:     (*server).getDeliveryPipeline,
	Read:        readEntry{Writes: readWritesState, AsOf: asOfNoHistory, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	Shape: routeShape{
		Status: http.StatusOK, Response: deliveryPipelineResponse{}, Params: deliveryPipelineQuery{},
	},
})
