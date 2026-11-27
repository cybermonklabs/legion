package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/projects/{key}/spec-sources/{owner}/{repo}",
	Auth:        authAny,
	Description: "Remove a repository from a project's spec sources; 404 SOURCE_NOT_FOUND when it is none. Its approved spec stays, and an issue of it holds no change request from then on.",
	Handler:     (*server).deleteSpecSource,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true,
	},
})
