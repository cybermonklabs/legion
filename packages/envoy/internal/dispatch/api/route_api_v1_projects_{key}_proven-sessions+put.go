package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/proven-sessions",
	Auth:        authAny,
	Description: "Turn on the project's proven-sessions setting. With it on, an architect's write of an issue's spec or Verification document passes only on a credential that proves the architect's session. A role-holder publication then passes only on a credential that proves legion-daemon:<project>. Any credential that may write the project's issues may turn it on, and the route declares no OAuth scope. A bearer names its session in actor. A token scoped to another project is 403 PROJECT_SCOPE. An unknown project is 404 PROJECT_NOT_FOUND. A PUT on a project that has the setting on changes nothing and appends no event. A change appends project.updated, which names its writer. The answer is the project as GET /api/v1/projects lists it. Only the project's roll-out issue turns it on, once the project's daemon and pods write with their own tokens. Before that, it stops every architect write and every role-holder publication on the shared token until a person turns it off.",
	Handler:     (*server).putProjectProvenSessions,
	WriteClass:  scopeProject,
	// Both answer the project as GET /api/v1/projects lists it (project_proven_sessions.go). A
	// bearer names its session in the PUT's actor; the DELETE reads its person off the
	// credential and takes no body.
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Project{}, Request: actorInput{}, OptionalRequest: true,
	},
})
