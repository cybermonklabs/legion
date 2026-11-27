package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/users",
	Auth:        authAny,
	Description: "Everyone who has signed in, by lowercase email: the assignee picker's options.",
	Handler:     (*server).listUsers,
	Successor:   successorIs(http.MethodGet, "/users"),
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: listUsersResponse{}},
})
