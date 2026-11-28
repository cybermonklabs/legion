package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/artifacts",
	Auth:        authAny,
	Description: "List an issue's documents and files.",
	Handler:     (*server).listArtifacts,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the documents and files that the issue held then, each with its name, its versions and its approval at the cut. " + asOfRemovedDocumentNotListed},
	Shape:       routeShape{Status: http.StatusOK, Response: []artifactRead{}, Params: asOfQuery{}},
})
