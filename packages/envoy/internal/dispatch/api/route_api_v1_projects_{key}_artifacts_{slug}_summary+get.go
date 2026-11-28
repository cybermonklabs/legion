package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/artifacts/{slug}/summary",
	Auth:        authAny,
	Description: "What a reference to a project document shows, read without its versions: {id, project, slug, name, kind, latest_version}, the slug or unambiguous filename resolved as the project document read resolves it (404 ARTIFACT_NOT_FOUND, 400 ARTIFACT_REFERENCE_AMBIGUOUS).",
	Handler:     (*server).getProjectDocumentSummary,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: documentSummary{}},
})
