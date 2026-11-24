package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/settings/repo-projects",
	Auth:        authAny,
	Description: "Read only: the GitHub repository to project mappings as the last release left them, each marked whether it is its project's default and what repository it was renamed to, null for one that was never renamed. Nothing writes them: PUT /api/v1/projects/{key}/default-repository sets a project's default repository. The route stays for the Legion daemons that read it at boot, and the next release removes it.",
	Handler:     (*server).listRepoProjects,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.RepoProject{}},
})
