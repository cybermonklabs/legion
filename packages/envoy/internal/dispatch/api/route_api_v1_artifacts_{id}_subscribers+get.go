package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/subscribers",
	Auth:        authAny,
	Description: "Sessions subscribed to a document's topics.",
	Handler:     (*server).listArtifactSubscribers,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/subscribers"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []subscriber{}},
})
