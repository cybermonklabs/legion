package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/projects/{key}/architecture-sources/{owner}/{repo}", Auth: authAny, Versions: onlyV2,
		Description: "Add a repository as an architecture source of a project, or change the branch of the source it " +
			"is {branch}, a bearer naming its session in actor, after proving the GitHub App can read it and that branch " +
			"is the name of a branch it has; a missing branch, a commit SHA, a relative ref (main~1, HEAD) or a refs/ path " +
			"is 409 SOURCE_ACCESS naming the value and the repository, as a credential problem is, and nothing is " +
			"written. A blank branch and a repository that is not owner/name are 400 SOURCE_INPUT. A save resets the " +
			"source's sync bookkeeping, keeps its components and appends settings.architecture_source.updated. A source " +
			"records no setter, so a save of the branch an enabled source already has, by any caller, answers the source " +
			"as it stands and writes nothing, the access check and the reset included. Answers 200 the source: " +
			architectureSourceText + ". 404 PROJECT_NOT_FOUND for a project that does not exist.",
		Handler:  s.putArchitectureSourceV2,
		Resource: "architecture-sources", Kind: kindItem, Key: "{owner}/{repo}",
		Body:  requestBody{Kind: bodyJSON, Type: putArchitectureSourceInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.ArchitectureSource{}},
		Scope: scopeProject, ProjectOf: projectPathProject,
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal, sourceInputRefusal, sourceUnreadableRefusal},
	}
})
