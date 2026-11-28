package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/broadcasts",
	Auth:        authAny,
	Description: "Recent broadcasts, newest first, with their recipient and reply counts.",
	Handler:     (*server).listBroadcasts,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: []broadcastSummary{}},
})
