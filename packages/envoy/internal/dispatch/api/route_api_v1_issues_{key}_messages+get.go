package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/messages",
	Auth:        authAny,
	Description: "List an issue's message threads: every root message, oldest first, each with its deliveries and its replies.",
	Handler:     (*server).listIssueMessages,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []messageRead{}},
})
