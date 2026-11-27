package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     goingCataloguePath,
	Auth:        authAny,
	Description: goingCatalogueDescription(),
	Handler:     (*server).getGoingCatalogue,
	Successor:   successorIs(http.MethodGet, "/going-actions"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingCatalogueBody{}},
})
