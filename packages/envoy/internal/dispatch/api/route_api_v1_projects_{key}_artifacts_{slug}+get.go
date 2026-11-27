package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}",
	Auth:        authAny,
	Description: "Read a project document by slug or unambiguous filename.",
	Handler:     (*server).getArtifact,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfDocumentAtACut},
	Shape:       routeShape{Status: http.StatusOK, Response: artifactRead{}, Params: asOfQuery{}},
})
