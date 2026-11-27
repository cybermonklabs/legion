package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/_test/room-charges",
	Auth:        authAny,
	Description: "Test hook: what the room budget charges each room it holds a charge for, beside its own figures, {charges, used, limit}.",
	Handler:     (*server).roomCharges,
	// It reads the process its call lands on, alone.
	TestHook: true,
	// The test hooks' GET rows, which read this process's memory.
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: roomChargesState{}},
})
