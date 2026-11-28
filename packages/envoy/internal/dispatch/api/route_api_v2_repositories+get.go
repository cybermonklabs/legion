package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/repositories", Auth: authAny, Versions: onlyV2,
		Description: "Read only: the GitHub repository to project mappings as the last release left them, by " +
			"repository, of every project: {repo, project, default, renamed_to, created_by, created_at}, default " +
			"whether it was its project's default repository and renamed_to the owner/name it is called today, null " +
			"for one that was never renamed. Nothing writes them: PATCH /api/v2/projects/{key} sets a project's " +
			"default repository (default_repository). project=<key>, repeatable, keeps the mappings of those " +
			"projects, with no default project. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}.",
		Handler:  s.listRepositoriesV2,
		Resource: "repositories", Kind: kindCollection, Key: "{owner}/{repo}",
		CrossProject: true,
		Query:        repositoriesV2Query{},
		Shape:        routeShape{Status: http.StatusOK, Response: page[model.RepoProject]{}},
		Refusals:     listRefusals(),
	}
})
