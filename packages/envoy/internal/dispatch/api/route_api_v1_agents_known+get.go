package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/known",
	Auth:        authAny,
	Description: fmt.Sprintf("Which of the sessions the repeated id parameter names (1 to %d session ids) Dispatch holds any record of, so a page links a dispatch://agent/<session id> reference only where that conversation exists, an ended session's included: {sessions: [{session_id, title}]} in the order asked, each once, and no row for an id with no record. A record is the registry copy GET /api/v1/agents lists (whatever its age), an event the session wrote, a message targeted at it, a message or comment delivery to it, a file or image of its conversation, or an ask it follows; title is the registry copy's, null where that copy lists the session with none or does not list it. One statement answers the whole batch. No id, more than %d, a value that is no session id, or one longer than %d bytes, the longest session id a reference in text names, is 400 INVALID_QUERY naming the parameter; a parameter it does not take is read by nothing. It needs the person scope and admits exactly the callers GET /api/v1/agents/{session_id}/messages admits; session is the session the request acts for, as on every route.", contracts.KnownSessionsMax, contracts.KnownSessionsMax, knownSessionIDMax),
	Handler:     (*server).listKnownSessions,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: knownSessions{}, Params: knownSessionsQuery{},
	},
})
