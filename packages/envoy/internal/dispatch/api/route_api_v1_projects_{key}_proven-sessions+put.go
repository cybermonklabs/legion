package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/proven-sessions",
	Auth:        authAny,
	Description: "Turn on the project's proven-sessions setting. With it on, an architect's write of an issue's spec or Verification document passes only on a credential that proves the architect's session. A role-holder publication then passes only on a credential that proves legion-daemon:<project>, or legion-daemon:<P> of another project's daemon on a broker token that names P. Any credential that may write the project's issues may turn it on, and the route declares no OAuth scope. A bearer names its session in actor. A token scoped to another project turns it on, as the shared token does. A token scoped to one project gets 403 PROJECT_SCOPE for a project that does not exist and for a private project that it does not name. Any other caller gets 404 PROJECT_NOT_FOUND for a project that does not exist. A PUT on a project that has the setting on changes nothing and appends no event. A change appends project.updated, which names its writer. The answer is the project as GET /api/v1/projects lists it. The command bar's undo of a change that turned the setting off makes this write conditional by an in-process condition on the request's context, never a body member: the project must hold the setting off, as that change left it, and no project.updated event after that change may have changed it, else 409 PROJECT_CHANGED, nothing written (500 GOING_CONDITION_MISSING for such a write back carrying no condition). Only the project's roll-out issue turns it on, once the project's daemon and pods write with their own tokens. Before that, it stops every architect write and every role-holder publication on the shared token until a person turns it off.",
	Handler:     (*server).putProjectProvenSessions,
	WriteClass:  scopeProject,
	// The answer is the project as GET /api/v1/projects lists it (project_proven_sessions.go). A
	// bearer names its session in actor, which a caller may leave out.
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Project{}, Request: actorInput{}, OptionalRequest: true,
	},
})
