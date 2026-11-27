package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/asks/{id}/asked-of",
	Auth:        authAny,
	Description: "Hand an open ask to another person {asked_of}, the email of somebody who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise): the ask waits on them from now on, wherever its issue goes, and the Inbox lists it under them. It needs the person scope, since handing a question on is one person asking another. An ask that is not open is 409 ASK_NOT_OPEN and one whose issue is closed 409 ISSUE_CLOSED. Naming the assignee of the ask's own issue empties the field instead, so the ask follows its issue again; either way the row records who handed it on and when (asked_of_by, asked_of_at), and one ask.asked_of event names the person it waited on before and the one it waits on now. Answers the ask.",
	Handler:     (*server).putAskAskedOf,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedAsk{}, Request: askAskedOfInput{}},
})
