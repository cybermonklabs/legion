package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts",
	Auth:        authAny,
	Description: "List a project's documents.",
	Handler:     (*server).listProjectArtifacts,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the documents that the project held then, each with its name, its versions and its approval at the cut. " + asOfRemovedDocumentNotListed},
	Shape: routeShape{
		Status: http.StatusOK, Response: []artifactRead{}, Params: projectArtifactsQuery{},
	},
})
