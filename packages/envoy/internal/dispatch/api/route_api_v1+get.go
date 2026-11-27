package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1",
	Auth:        authPublic,
	Description: "This index: every route with its method, auth, and purpose.",
	Handler:     (*server).getRouteIndex,
	Successor:   successorIs(http.MethodGet, ""),
	// The other reads.
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: routeIndex{}},
})
