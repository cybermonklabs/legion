package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/messages",
	Auth:        authAny,
	Description: "A session's targeted messages, newest first.",
	Handler:     (*server).listAgentMessages,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []messageRead{}},
})
