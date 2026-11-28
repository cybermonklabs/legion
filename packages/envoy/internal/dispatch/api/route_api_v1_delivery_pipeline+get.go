package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/pipeline",
	Auth:        authAny,
	Description: "The Pipeline page: over the seven UTC days ending today (now, or ?at= for a fixed day), by the day each run started, the PR-checks workflow's run wall time and success, its five slowest jobs and the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs the settings name in push runs on main, and merge-to-production and PR-open-to-merge per merge day against their targets, over every deploy source's repository of every project with delivery on, each repository's runs judged by the rule its stored eval day rows stand built under, as the timeline does. The answer states the six targets of the evals of the one project whose deploy sources read every deploy repository, where one project owns them all. Where two or more projects own them, or none is read, the route cannot name one project and reads the seeded catalogue's targets. A job is ranked among the slowest with as many timed results as pr_checks.slowest_min_results states: the min_n of the job series (job:*) of the pr-ci eval of the project whose sources read every deploy repository, or 50 where two or more projects own them, none is read, or that project holds no pr-ci eval yet, and 1 where that min_n is null, since a job needs a timed result to have a median; the ranking is the one pr-ci's items read with sort=median. Not filtered by the timeline's window or facets; 400 INVALID_QUERY for a malformed at, one named more than once, or one whose seven days start before the UTC year 1, 404 DELIVERY_NOT_CONFIGURED when no project has delivery on.",
	Handler:     (*server).getDeliveryPipeline,
	Read:        readEntry{Writes: readWritesState, WriteNote: "As the timeline's, for the deploy runs on main since the first day it shows: an insert into delivery_run_warnings for a run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	Shape: routeShape{
		Status: http.StatusOK, Response: deliveryPipelineResponse{}, Params: deliveryPipelineQuery{},
	},
})
