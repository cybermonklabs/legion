package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/measures",
	Auth:        authAny,
	Description: "The four delivery measures for [from, to) (default the last 7 days) and the same facets the timeline takes, and the six targets of the evals of the deploy repository's project: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per PR and per deploy, time to restore, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met. The deploy repository's project is the one project whose published default repository is the deploy repository. If no project or more than one publishes the deploy repository as its default, the route cannot name one project and reads the seeded catalogue's targets. flags_source says whether a failure source fed the change failure rate and time to restore: with none (none) every change_failure_rate count is 0 and every rate null, time_to_restore has no median, and both verdicts are null, since a rate nothing measured is no 0; change_failure_rate itself is never null. It answers under the delivery settings the stored eval day rows stand built under, as the timeline does. 400 INVALID_QUERY for a from or to that is no RFC3339 time or no moment of the UTC years 1 to 9999, a from not before to, a window over 84 days, or from, to or q named more than once; 404 DELIVERY_NOT_CONFIGURED without a delivery_settings row.",
	Handler:     (*server).getDeliveryMeasures,
	Read:        readEntry{Writes: readWritesState, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	Shape: routeShape{
		Status: http.StatusOK, Response: deliveryMeasuresResponse{}, Params: deliveryWindowQuery{},
	},
})
