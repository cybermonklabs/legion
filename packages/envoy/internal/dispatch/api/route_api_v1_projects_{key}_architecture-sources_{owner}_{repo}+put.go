package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
	Auth:        authAny,
	Description: "Add a repository as an architecture source of a project, or change the branch of the source it is {branch}, after proving the GitHub App can read it and that branch is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is written. A re-save resets the source's sync bookkeeping and keeps its components.",
	Handler:     (*server).putArchitectureSource,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.ArchitectureSource{}, Request: putArchitectureSourceInput{},
	},
})
