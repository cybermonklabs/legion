package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/legion/daemon",
	Auth:        authAny,
	Description: "The Legion daemon this project's state is read from, with the actor that first named it and when it was last written; null when the project names none. With NATS, Dispatch records the state events that daemon publishes, and reads its `GET /legion/v1/state` whole only where the events cannot carry the state. No route lists every project's daemon: a page that shows them all reads each project's. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist.",
	Handler:     (*server).getLegionSetting,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: legion.Settings{},
		// A project that names no daemon answers 200 and the JSON literal `null`
		// (settings_legion.go).
		Nullable: true,
	},
})
