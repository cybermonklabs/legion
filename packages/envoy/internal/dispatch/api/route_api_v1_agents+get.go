package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents",
	Auth:        authAny,
	Description: "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old, newest seen first. last_activity is the time of the session's newest event, and last_activity_issue and last_activity_type say where it is: that event's issue (absent for a document on no issue) and its type, both absent for a session with no event. ?limit= answers that many of the newest seen alone, reading the open asks and last activity of those alone (400 INVALID_QUERY for anything but a whole number of 1 or more); without it every session is listed.",
	Handler:     (*server).listAgents,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []agentResponse{}, Params: agentListQuery{}},
})
