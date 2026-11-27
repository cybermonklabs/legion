package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/{id}/followers",
	Auth:        authAny,
	Description: "List the sessions an ask's answer and replies reach.",
	Handler:     (*server).listAskFollowers,
	Successor:   deletedFor(http.MethodGet, "/asks/{id}?include=thread"),
	ID:          &idRoute{Kind: "ask", NotFound: apicode.NotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: askFollowersRead{}},
})
