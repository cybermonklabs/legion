package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents",
	Auth:        authAny,
	Description: "Live sessions with roles, capabilities, open asks, and last activity, from Dispatch's copy of the Envoy listener's session list, at most 5 s old and refreshed ahead of that bound. A copy past its bound answers 503 ENVOY_UNAVAILABLE at once; only a server that has no copy yet waits, up to 2 s, for the listener's first answer.",
	Handler:     (*server).listAgents,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []agentResponse{}},
})
