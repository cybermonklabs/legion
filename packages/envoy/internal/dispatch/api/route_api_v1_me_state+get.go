package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/state",
	Auth:        authAny,
	Description: "The caller's per-issue read state.",
	Handler:     (*server).getUserState,
	Successor:   successorIs(http.MethodGet, "/me/marks?kind=issue"),
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: userState{}},
})
