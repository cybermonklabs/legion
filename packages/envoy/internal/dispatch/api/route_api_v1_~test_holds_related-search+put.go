package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/_test/holds/related-search",
	Auth:        authAny,
	Description: "Test hook: hold this process's next related search's keyword stage for a Go duration.",
	Handler:     (*server).armRelatedSearchHold,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusNoContent, Request: relatedReadHoldInput{}},
})
