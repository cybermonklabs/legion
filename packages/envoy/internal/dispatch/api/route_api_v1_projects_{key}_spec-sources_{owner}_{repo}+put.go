package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/spec-sources/{owner}/{repo}",
	Auth:        authAny,
	Description: "Name a repository a spec source of a project {branch}, or change the branch of the one it is, and turn it on: from then on an issue of that repository holds a change request, and the approval of its spec writes the request into the repository's approved spec or is refused. A blank branch is 400 SOURCE_INPUT.",
	Handler:     (*server).putSpecSource,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.SpecSource{}, Request: putSpecSourceInput{},
	},
})
