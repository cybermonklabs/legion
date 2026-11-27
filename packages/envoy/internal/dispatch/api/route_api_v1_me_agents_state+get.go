package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/agents/state",
	Auth:        authAny,
	Description: "The caller's per-agent conversation state: cleared_before, read_through, unread_replies (the session's replies to the caller's direct messages newer than both and not read by id), and pinned_at (when the caller pinned the session, which puts it in their sidebar's Agents list).",
	Handler:     (*server).getUserAgentState,
	Successor:   successorIs(http.MethodGet, "/me/marks?kind=agent"),
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: userAgentStates{}},
})
