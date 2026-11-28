package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/agents/{session_id}/stream/_test/publish",
	Auth:        authAny,
	Description: "Test hook: publish one frame to a session's conversation viewers on every process.",
	Handler:     (*server).publishAgentStreamFrame,
	TestHook:    true,
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: testHookOK{}, Params: publishFrameQuery{},
		// The body is the frame's own bytes, read whole and published unparsed; only
		// ?as=responder reads it, as {"responding"} (publishFrameResponderInput,
		// agent_stream.go). Naming that one arm would state a contract the route does not
		// hold its caller to, so the body is an object of any shape.
		OpaqueRequest: "the frame to publish, read whole and published unparsed; under " +
			"?as=responder it is read as {\"responding\": bool} instead",
		// publishAgentStreamFrame parses those bytes as JSON only under ?as=responder, so every
		// other call carries whatever bytes it likes, none included.
		OptionalRequest: true,
	},
})
