package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/_test/quiesce",
	Auth:        authAny,
	Description: "Test hook: close every live document on every process and finish the settlements in flight.",
	Handler:     (*server).quiesceDocuments,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusOK, Response: testHookOK{}},
})
