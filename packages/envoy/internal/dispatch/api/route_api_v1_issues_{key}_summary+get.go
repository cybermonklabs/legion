package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/summary",
	Auth:        authAny,
	Description: "What a reference to an issue shows, read without the full issue: {key, project, title, status, documents}, documents every artifact of the issue in the order the issue read lists them, each {id, slug, name, kind, latest_version, primary}, primary whether the document is the issue's spec; 404 NOT_FOUND for no such issue.",
	Handler:     (*server).getIssueSummary,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueSummary{}},
})
