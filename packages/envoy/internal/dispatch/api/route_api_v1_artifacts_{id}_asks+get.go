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
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []shapedAsk{}, Params: artifactAsksQuery{}},
})
