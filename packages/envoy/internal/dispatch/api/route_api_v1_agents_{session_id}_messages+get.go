package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// kept for one release: inbox-old-reads
var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/messages",
	Auth:        authAny,
	Description: "A session's targeted messages, newest first: the rows GET /api/v1/agents/{session_id}/conversation answers, kept for the clients of the release before it.",
	Handler:     (*server).listAgentMessages,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	// kept for one release: inbox-old-reads
	Read: readEntry{Writes: readWritesNothing},
	// kept for one release: inbox-old-reads
	Shape: routeShape{Status: http.StatusOK, Response: []messageRead{}},
})
