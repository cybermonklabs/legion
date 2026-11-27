package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
	Auth:        authAny,
	Description: "Remove one architecture source of a project and the components it projected; every other source's components stay.",
	Handler:     (*server).deleteArchitectureSource,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true,
	},
})
