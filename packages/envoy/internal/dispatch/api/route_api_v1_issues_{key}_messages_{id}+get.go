package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/messages/{id}",
	Auth:        authAny,
	Description: "Read one message with its deliveries and replies.",
	Handler:     (*server).getMessage,
	Successor:   deletedFor(http.MethodGet, "/messages/{id}"),
	ID:          &idRoute{Kind: "message", WithinIssue: true, NotFound: apicode.NotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: messageRead{}},
})
