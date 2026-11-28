package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/asks",
	Auth:        authAny,
	Description: "List a document's asks.",
	Handler:     (*server).listArtifactAsks,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/asks"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the document's asks that existed then, each at its newest state event at or before the cut."},
	Shape:       routeShape{Status: http.StatusOK, Response: []shapedAsk{}, Params: artifactAsksQuery{}},
})
