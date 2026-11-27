package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/references",
	Auth:        authAny,
	Description: "Cross-references to and from a document.",
	Handler:     (*server).getArtifactReferences,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/references"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.ArtifactReferences{}},
})
