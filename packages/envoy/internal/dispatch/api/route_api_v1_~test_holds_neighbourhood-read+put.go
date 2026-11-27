package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/_test/holds/neighbourhood-read",
	Auth:        authAny,
	Description: "Test hook: hold this process's next graph neighbourhood read for a Go duration.",
	Handler:     (*server).armNeighbourhoodReadHold,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusNoContent, Request: relatedReadHoldInput{}},
})
