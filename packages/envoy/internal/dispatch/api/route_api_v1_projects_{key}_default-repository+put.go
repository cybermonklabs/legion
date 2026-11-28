package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/default-repository",
	Auth:        authAny,
	Description: `Set or clear a project's default repository {repo}: "owner/name" sets it and null clears it. An issue of the project resolves to it when neither the issue nor an issue above it names a repository. A write of the value the project holds already changes nothing and appends no event; a change appends project.updated with the project row. A body without repo, or a repo that is not a repository GitHub could hold, is 400 INVALID_PROJECT, and an unknown project 404 PROJECT_NOT_FOUND. A deploy source of the project's delivery settings that names no repository reads the default, so for a project with delivery on and such a source a clear is 409 DELIVERY_NEEDS_REPOSITORY, and a repository another delivery-on project's source reads, or another source of the project names, is 409 DELIVERY_REPOSITORY_SHARED naming the project that reads it; a move clears the reconcile progress of the old and the new repository, so the new one's runs are imported. The default belongs to the deployment, so a project-scoped credential is refused it, the project this path names included. It declares no personal scope: the credential the deployment configured writes it, and so does a person.`,
	Handler:     (*server).putProjectDefaultRepository,
	// Which repository a project's issues resolve to when they name none: addressed under the
	// project it serves, and not belonging to it. Choosing the repository of every issue of a
	// project that names none is the deployment's write, so a credential that names one project
	// is refused it by name, the project its path names included.
	WriteClass: scopeDeployment,
	// A move a delivery-on project's source refuses answers the settings save's own refusal body
	// (delivery.SettingsError, defaultRepositoryMove), naming the project that reads the repository.
	Shape: routeShape{
		Status: http.StatusOK, Response: projectDefaultRepository{}, Request: projectDefaultRepositoryInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: delivery.SettingsError{}}},
	},
})
