package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/{id}/followers",
	Auth:        authAny,
	Description: "List the sessions an ask's answer and replies reach.",
	Handler:     (*server).listAskFollowers,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: askFollowersRead{}},
})
