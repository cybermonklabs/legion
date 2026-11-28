package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/_test/rooms",
	Auth:        authAny,
	Description: "Test hook: list the documents this process holds a room of.",
	Handler:     (*server).listResidentRooms,
	TestHook:    true,
	// The test hooks' GET rows, which read this process's memory.
	Read:  readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{Status: http.StatusOK, Response: residentRooms{}},
})
