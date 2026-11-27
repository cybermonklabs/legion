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
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []agentResponse{}},
})
