package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/open",
	Auth:        authAny,
	Description: "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required.",
	Handler:     (*server).listOpenAsks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: openAsksResponse{}, Params: openAsksQuery{}},
})
