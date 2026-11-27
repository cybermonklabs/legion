package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/live/_test/rotate",
	Auth:        authAny,
	Description: "Test hook: close every open live socket on every process with 4001 (max age), as its age does.",
	Handler:     (*server).rotateLiveSockets,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusOK, Response: testHookOK{}},
})
