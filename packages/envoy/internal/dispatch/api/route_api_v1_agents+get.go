package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents",
	Auth:        authAny,
	Description: "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old.",
	Handler:     (*server).listAgents,
	Successor:   successorIs(http.MethodGet, "/agents"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []agentResponse{}},
})
