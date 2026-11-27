package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// kept for one release: inbox-old-reads
var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/inbox",
	Auth:        authAny,
	Description: "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<email> filter on the person each ask waits on, which is three steps resolved as the ask is read: the person the question was handed to, then the issue's assignee, then the project's default assignee (unassigned is every ask nobody waits on; an email nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED).",
	Handler:     (*server).listInbox,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []openAskRow{}, Params: inboxQuery{}},
})
