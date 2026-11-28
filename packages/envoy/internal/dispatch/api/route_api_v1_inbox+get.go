package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/inbox",
	Auth:        authAny,
	Description: "Open asks waiting on the caller, grouped by whose turn it is; ?project= and ?assignee=me|unassigned|<email> filter on the person each ask waits on, which is three steps resolved as the ask is read: the person the question was handed to, then the issue's assignee, then the project's default assignee (unassigned is every ask nobody waits on; an email nobody has signed in with is 400 ASSIGNEE_NOT_ALLOWED).",
	Handler:     (*server).listInbox,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route lists the asks open at the cut, on an issue open then or on a project document, that waited on the person at the cut by the same three steps. Snoozes keep no history, so the answer leaves out snoozed_until. An ask whose person is a project default that the records at the cut do not give matches no ?assignee=."},
	Shape:       routeShape{Status: http.StatusOK, Response: []inboxAsk{}, Params: inboxQuery{}},
})
