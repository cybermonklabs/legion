package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}",
	Auth:        authAny,
	Description: "Read a document's metadata and current version.",
	Handler:     (*server).getArtifact,
	// The {id} is an artifact, looked for among every document and file, as a whole artifact id
	// is. The read goes through loadArtifact, which answers ARTIFACT_NOT_FOUND.
	ID:    &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:  readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfDocumentAtACut},
	Shape: routeShape{Status: http.StatusOK, Response: artifactRead{}, Params: asOfQuery{}},
})
