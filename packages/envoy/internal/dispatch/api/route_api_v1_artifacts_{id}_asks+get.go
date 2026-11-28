package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/asks",
	Auth:        authAny,
	Description: "List a document's asks.",
	Handler:     (*server).listArtifactAsks,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []shapedAsk{}, Params: artifactAsksQuery{}},
})
