package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/asks",
	Auth:        authAny,
	Description: "List an issue's asks, each with its newest reply; ?state= filters, and ?thread=1 adds each ask's thread (replies, rewordings, answers, followers, claimant, route, route_holder).",
	Handler:     (*server).listIssueAsks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []issueAskRow{}, Params: issueAsksQuery{}},
})
