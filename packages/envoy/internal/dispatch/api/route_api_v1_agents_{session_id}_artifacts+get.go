package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/artifacts",
	Auth:        authAny,
	Description: fmt.Sprintf("The newest %d files and images of an agent's conversation, newest first (created_at, then id), each as GET /api/v1/agents/{session_id}/artifacts/{slug} reads it, with its versions; [] for a conversation that holds none. An older file is not listed and is still read by its slug. A reference picker lists them under the session, each addressed dispatch://agent/<session_id>/artifact/<slug>. A conversation's files are part of the conversation, so the route needs the person scope and admits exactly the callers GET /api/v1/agents/{session_id}/messages admits, reading session as that route does.", agentArtifactsListMax),
	Handler:     (*server).listAgentArtifacts,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []artifactRead{}},
})
