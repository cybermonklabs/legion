package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/broadcasts",
	Auth:        authAny,
	Description: "Send one message to many sessions: {body, delivery, session_ids}, one targeted message per recipient under a shared broadcast, answered once it is committed while each recipient's delivery runs behind the answer. A selected session the Envoy listener knows and that is not running is a recipient like any other, its copy stored for it and its attempt recorded queued; one the listener knows nothing about, or that does not advertise the mode, is excluded and named in the response, never switched to another mode. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 at once with the broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions, Idempotency-Replayed: ?1, and puts every recipient it still owes on its way behind the answer. A recipient a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.",
	Handler:     (*server).createBroadcast,
	WriteClass:  scopeDeployment,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusCreated, Response: broadcastCreated{}, Request: createBroadcastInput{},
	},
})
