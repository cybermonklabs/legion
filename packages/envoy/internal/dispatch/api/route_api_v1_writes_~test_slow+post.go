package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/writes/_test/slow",
	Auth:        authAny,
	Description: "Test hook: {method, path, delay_ms} delays every keyed run of that method and path by delay_ms after its key is claimed, on the handler's own context; 0 disarms.",
	Handler:     (*server).armSlowWrite,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusOK, Response: testHookOK{}, Request: slowWriteInput{}},
})
