package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/messages",
	Auth:        authAny,
	Description: "List an issue's message threads: every root message, oldest first, each with its deliveries and its replies. " + threadPageDescription("A message thread is a root message and every message under it at any depth, and its row is the list's row with last_activity_at.", "?person=in keeps each thread with an entry a person wrote, and each thread whose root has no target; ?person=none keeps each thread whose root targets an agent, as role:<name> or session:<id>, and that holds no entry a person wrote. Neither value holds a thread whose root the Legion daemon of the issue's project wrote, the session legion-daemon:<PROJECT> (legion-daemon:ACME on an ACME issue), whoever wrote its other entries, so each other thread is on the pages of exactly one of the two."),
	Handler:     (*server).listIssueMessages,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: []messageRead{}, Params: issueMessagesQuery{},
		Also: []routeAnswer{{Status: http.StatusOK, Body: messageThreadPage{}}},
	},
})
