package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/events/log",
	Auth:        authAny,
	Description: "Page the whole event log by id, whatever owns each event (an issue, a document, a project or none): the events with id > after (default 0), oldest first, at most limit (1 to 1000, default 1000). An event's actor carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it.",
	Handler:     (*server).listEventLog,
	Read:        readEntry{Writes: readWritesNothing, WriteNote: "Writes nothing itself. Reading the safe horizon (events.Horizon) may run the horizon's observation on this request's goroutine, and that observation writes event_horizon on a context of its own (writePersisted), at most once a second, after its waiters have their answer: the server's write for every reader of the event log alike, not this request's."},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Event{}, Params: eventLogQuery{}},
})
