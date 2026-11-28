package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/_test/room-counters",
	Auth:        authAny,
	Description: "Test hook: count this process's room failures, lock-waiting compactions and room closes of a document, time its settlement resumptions of it, and read the awareness client entries its room holds now.",
	Handler:     (*server).getRoomCounters,
	TestHook:    true,
	ID:          &idRoute{Reason: "a test hook's document id, naming a live room rather than a stored document"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: docs.RoomCounters{}},
})
