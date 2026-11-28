package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects",
	Auth:        authAny,
	Description: "List projects. A project with a default repository carries it as default_repo, and a project with none carries no default_repo. Each project carries proven_sessions, true while its proven-sessions setting is on (PUT and DELETE /api/v1/projects/{key}/proven-sessions). A project's default assignee is not listed here: GET /api/v1/settings/project-default-assignees reads it, with the person scope.",
	Handler:     (*server).listProjects,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Project{}},
})
