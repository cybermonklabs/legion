package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/going/runs/{id}/stop",
	Auth:        authAny,
	Description: fmt.Sprintf("Stop the caller's own command-bar run at once: a run that is queued, works or waits is set stopped with the reason %q, its end frame names it, the agent cancels its model call in flight, and what the run did stays. Answers the run as it then stands; a run that had ended is answered as it ended, unchanged. 404 %s as GET /api/v1/going/runs/{id} answers it.", goingStoppedByPerson, codeGoingRunNotFound),
	Handler:     (*server).stopMyGoingRun,
	WriteClass:  scopeSession,
	ID:          &idRoute{Reason: "a going run's id, which the route stops for its own author alone and answers for itself"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: goingRun{}},
})
