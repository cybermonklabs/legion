package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/resolve",
	Auth:        authAny,
	Description: "Resolve ?ref=<KEY | owner/repo#n> to an issue key.",
	Handler:     (*server).resolveIssue,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route resolves owner/repo#n by the external links that the issues held at the cut."},
	Shape:       routeShape{Status: http.StatusOK, Response: issueRefResolved{}, Params: issueResolveQuery{}},
})
