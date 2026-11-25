package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/projects/{key}/proven-sessions",
	Auth:        authAny,
	Description: "Turn off the project's proven-sessions setting. This weakens a check, so the route needs the person scope. A credential the deployment configured, the shared token or a pod's, holds none and is 403 OAUTH_SCOPE_REQUIRED. A token scoped to another project is 403 PROJECT_SCOPE. An unknown project is 404 PROJECT_NOT_FOUND. A DELETE on a project that has the setting off changes nothing and appends no event. A change appends project.updated, which names the person as its writer. The answer is the project as GET /api/v1/projects lists it.",
	Handler:     (*server).deleteProjectProvenSessions,
	WriteClass:  scopeProject,
	// Turning a project's proven-sessions setting off weakens a check, so it is a person's call,
	// as the project's default assignee above is (project_proven_sessions.go). Turning it on
	// needs no scope.
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: model.Project{}},
})
