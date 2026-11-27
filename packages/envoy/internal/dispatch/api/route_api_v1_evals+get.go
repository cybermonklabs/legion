package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/evals",
	Auth:        authAny,
	Description: "The evals of several projects in the project read's item shape and envelope {items, total, limit, offset}, by project in the order named and then by slug, the page running across them, delivery_freshness always null: ?project=, repeated, names the projects read, and with none named every project the store holds is read, by key, with no project picked for the caller. The window, compare range, kind, repo and page keys are the project read's, their bounds included. 400 INVALID_QUERY names a malformed key, or a key the route does not take (any but these and the global session parameter), 404 PROJECT_NOT_FOUND an unknown project.",
	Handler:     (*server).listEvals,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: evalListResponse{}, Params: evalsQuery{},
	},
})
