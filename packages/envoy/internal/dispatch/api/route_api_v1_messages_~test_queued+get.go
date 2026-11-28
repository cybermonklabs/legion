package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/messages/_test/queued",
	Auth:        authAny,
	Description: "Test hook: list the targeted messages this process queued itself for a session neither the listener's registry nor its known-session store held, each as the envelope its session's subject would hold: {queued: [{subject, dedupe_key, event_id, source, payload_summary, payload, urgency}]}, oldest first; 404 QUEUED_ON_BUS on a process that publishes them to NATS.",
	Handler:     (*server).listQueuedMessages,
	TestHook:    true,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: queuedMessages{}},
})
