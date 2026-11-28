package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/search",
	Auth:        authAny,
	Description: searchDescription(searchDeclaresFour),
	Handler:     (*server).search,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfSearchAtACut},
	Shape:       routeShape{Status: http.StatusOK, Response: model.SearchResponse{}, Params: searchRouteQuery{}},
})
