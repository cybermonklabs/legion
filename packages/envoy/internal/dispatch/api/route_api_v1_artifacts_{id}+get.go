package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}",
	Auth:        authAny,
	Description: "Read a document's metadata and current version.",
	Handler:     (*server).getArtifact,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}"),
	// The {id} is an artifact, looked for among every document and file, as a whole artifact id
	// is. The read goes through loadArtifact, which answers ARTIFACT_NOT_FOUND.
	ID:    &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: artifactRead{}},
})
