package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/delivery/measures",
	Auth:        authAny,
	Description: "The delivery measures for [from, to) (default the last 7 days) and the same facets the timeline takes, and the six targets of the evals that judge them: deploys a day, lead time (merge, first commit and open to production; open to merge), change failure rate per deploy, failed deployment recovery time with the failures not restored, deployment rework rate, rework share, deploy-run success, a per-UTC-day series, the open P0 issues with no owner, and whether each target is met. The three failure measures fold their evals' stored day rows: each is null where no row holds a part of it, \"no data\" with no verdict, since a rate nothing measured is no 0. The runs are every deploy source's repository's of every project with delivery on, each judged by the rule its repository's stored eval day rows stand built under, as the timeline does. The six targets are those of the evals of the one project whose deploy sources read every deploy repository, where one project owns them all. Where two or more projects own them, or none is read, the route cannot name one project and reads the seeded catalogue's targets. 400 INVALID_QUERY for a from or to that is no RFC3339 time or no moment of the UTC years 1 to 9999, a from not before to, a window over 84 days, or from, to or q named more than once; 404 DELIVERY_NOT_CONFIGURED when no project has delivery on.",
	Handler:     (*server).getDeliveryMeasures,
	Read:        readEntry{Writes: readWritesState, WriteNote: "As the timeline's: an insert into delivery_run_warnings for a deploy run with no matching production job or apply step, on the request's context, once per process for each run and configuration (firstToWarn)."},
	Shape: routeShape{
		Status: http.StatusOK, Response: deliveryMeasuresResponse{}, Params: deliveryWindowQuery{},
	},
})
