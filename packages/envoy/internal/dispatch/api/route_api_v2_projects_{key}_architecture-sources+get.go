package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/projects/{key}/architecture-sources", Auth: authAny, Versions: onlyV2,
		Description: "A project's architecture sources, one per repository, in the order they were added: " +
			architectureSourceText + ". The project's component model is every source's components together. limit 1 to " +
			"200 (default 50) and offset; {items, total, limit, offset}, no item for a project with no source. 404 " +
			"PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.listProjectArchitectureSourcesV2,
		Resource: "architecture-sources", Kind: kindCollection, Key: "{owner}/{repo}",
		Query:    architectureSourcesV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.ArchitectureSource]{}},
		Refusals: listRefusals(projectMissingRefusal),
	}
})
