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
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks that were open at the cut. The answer's as_of is the cut, and each age_seconds counts from the cut."},
	Shape:       routeShape{Status: http.StatusOK, Response: openAsksResponse{}, Params: openAsksQuery{}},
})
