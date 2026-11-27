package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/me/agents/{session_id}/state",
	Auth:        authAny,
	Description: "Clear an agent's conversation for the caller (cleared_before hides exchanges at or before it), mark it read (read_through only moves forward), mark some of its replies read by id (read_replies, the session's own messages), and/or pin it to the caller's sidebar (pinned: true pins it, false unpins it, and neither touches the Clear); answers the session's state.",
	Handler:     (*server).putUserAgentState,
	Successor:   successorIs(http.MethodPatch, "/me/marks/{kind}/{id}", "kind=agent", "id={session_id}"),
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: userAgentState{}, Request: userAgentStateInput{},
	},
})
