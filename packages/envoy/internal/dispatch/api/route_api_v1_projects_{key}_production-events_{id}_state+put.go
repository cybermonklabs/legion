package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/projects/{key}/production-events/{id}/state",
	Auth:        authAny,
	Description: "Reject, re-count or restore one failure of the project {state, reason, restored_by_deploy?}: rejected and counted keep their reason (required), writer and time, and restored sets the restore time, its writer and the restoring deploy where named; the answer is the row. A reject and a re-count are a person's (the person scope) or the project's controller's: the session the project's Legion daemon names as its controller when the write is made, read from that daemon's GET /legion/v1/controller before the write and proven by that session's broker-issued token. A restore is a person's. Anyone else is 403 PRODUCTION_EVENT_STATE_FORBIDDEN, and so is a controller's write where the project names no Legion daemon or its daemon names no controller or another session; 503 LEGION_CONTROLLER_UNREAD where Dispatch could not read the controller from the daemon (a daemon build before that route answers 404, no answer within 2 s, or an answer it cannot read), and a person can still write; a row the project does not hold 404 PRODUCTION_EVENT_NOT_FOUND, a deploy's row or a missing reason 400 PRODUCTION_EVENT_STATE_INPUT, a restoring deploy no deploy row is 409 PRODUCTION_EVENT_NO_DEPLOY, an unknown project 404 PROJECT_NOT_FOUND.",
	Handler:     (*server).putProductionEventState,
	// The record of deploys and failed changes: a failure's state, a row of the project its path
	// names. The route holds the row to that project itself.
	WriteClass: scopeProject,
	ID: &idRoute{
		Reason: "a production event id",
	},
	Shape: routeShape{
		Status: http.StatusOK, Response: prodevents.Event{}, Request: productionEventStateInput{},
	},
})
