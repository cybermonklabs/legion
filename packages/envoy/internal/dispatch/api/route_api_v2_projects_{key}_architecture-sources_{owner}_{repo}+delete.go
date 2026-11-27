package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Auth: authAny, Versions: onlyV2,
		Description: "Remove one architecture source of a project and the components it projected, a bearer naming " +
			"its session in actor; every other source's components stay. Appends settings.architecture_source.updated " +
			"with deleted true. 204. A repository that is not owner/name is 400 SOURCE_INPUT, one the project has no " +
			"source from 404 SOURCE_NOT_FOUND; 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.deleteArchitectureSourceV2,
		Resource: "architecture-sources", Kind: kindItem, Key: "{owner}/{repo}",
		Body:  requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape: routeShape{Status: http.StatusNoContent},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal, architectureSourceMissingRefusal},
	}
})
