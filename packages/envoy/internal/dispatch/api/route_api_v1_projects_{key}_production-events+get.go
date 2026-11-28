package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/production-events",
	Auth:        authAny,
	Description: "The project's deploys and failed changes, one page {items, total, limit, offset} newest first (limit 1-200, default 50): a deploy by its finished_at and a failure by its detected_at within [from, to) (RFC3339, a moment of the UTC years 1 to 9999, default the last 7 days, at most 84); kind keeps deploy or failure rows, state counted or rejected ones (a deploy is always counted), and deploy_id (a uuid) the project's failures that blame that deploy, an empty page for an id of no row. 400 INVALID_QUERY names a malformed key, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND an unknown project.",
	Handler:     (*server).listProductionEvents,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: productionEventList{}, Params: productionEventsQuery{},
	},
})
