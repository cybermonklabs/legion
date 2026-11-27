package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/_test/room-budget",
	Auth:        authAny,
	Description: "Test hook: set the open documents' memory budget to bytes (0 restores the real one), so a browser test can fill it.",
	Handler:     (*server).setRoomBudget,
	// It acts on the process its call lands on, alone.
	TestHook:   true,
	WriteClass: scopeDeployment,
	Shape: routeShape{
		Status: http.StatusOK, Response: roomBudgetState{}, Request: roomBudgetInput{},
	},
})
