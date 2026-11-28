package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/default-assignee",
	Auth:        authAny,
	Description: "Move a project's default assignee to another person {assignee} (the email of a person who has signed in): who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to, and who a question on an issue nobody holds reaches. A project always names a person, so an assignee that is absent, null or blank is 400 INVALID_PROJECT and the default cannot be cleared. Needs the person scope, as the default names a person: a credential the deployment configured, the shared token or a pod's, holds none.",
	Handler:     (*server).putProjectDefaultAssignee,
	Successor:   successorIs(http.MethodPatch, "/projects/{key}"),
	WriteClass:  scopeProject,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: projectDefaultAssignee{}, Request: projectDefaultAssigneeInput{},
	},
})
