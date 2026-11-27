package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/conversation",
	Auth:        authAny,
	Description: "One session's conversation, newest activity first: each root message that reaches the session - targeted at it, or named by a delivery attempt to it whatever that attempt's state - with every reply under it and each message's delivery attempts, read for the person the credential acts for. It lists the 50 conversations that moved last and, past them, every conversation holding a reply of the session's that the caller has not read, each such root marked unread. It needs the person scope; session is the session the request acts for, as on every route. GET /api/v1/agents/{session_id}/messages answers the same rows.",
	Handler:     (*server).listAgentConversation,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []messageRead{}},
})
