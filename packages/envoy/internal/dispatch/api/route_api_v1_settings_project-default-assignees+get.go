package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/settings/project-default-assignees",
	Auth:        authAny,
	Description: "List every project with the person it defaults to, by project key: every project names one, and that person is the last step of the chain that decides who a question reaches. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.",
	Handler:     (*server).listProjectDefaultAssignees,
	// An assignee picker is a person's own read, so the route needs the person scope.
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []projectDefaultAssignee{}},
})
