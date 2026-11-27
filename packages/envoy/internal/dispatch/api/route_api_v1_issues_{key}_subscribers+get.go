package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/subscribers",
	Auth:        authAny,
	Description: "Sessions subscribed to an issue's topics.",
	Handler:     (*server).listIssueSubscribers,
	Successor:   successorIs(http.MethodGet, "/issues/{key}/subscribers"),
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []subscriber{}},
})
