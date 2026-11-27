package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/writes/_test/fail-between/{writer}",
	Auth:        authAny,
	Description: "Test hook: arm one failure for the next write of that writer, injected between its store write and its projection write, so both roll back.",
	Handler:     (*server).armFailBetween,
	TestHook:    true,
	WriteClass:  scopeDeployment,
	Shape:       routeShape{Status: http.StatusOK, Response: testHookOK{}},
})
