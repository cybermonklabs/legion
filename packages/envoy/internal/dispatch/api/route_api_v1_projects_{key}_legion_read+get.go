package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/legion/read",
	Auth:        authAny,
	Description: "The newest read of the Legion daemon of the project its path names, as the header of GET /api/v1/projects/{key}/legion/board gives it (its read_at, read_ok and read_error): read_at, when the newest record Dispatch made of that daemon was made, a batch of its state events or a whole read of its state, null where Dispatch made none; ok, whether that read succeeded, false where none was made; and error, why it did not, null where it did or none was made. While the project owes a whole read, ok and error are the newest whole read's, good or failed, so a batch of state events applied after a failed whole read does not show a good read. The `/ws/live` `legion-read` view carries the same body and sends each change, at every batch the Legion intake applies and every whole read it makes, failed or not. 400 INVALID_PROJECT for a key that is not a project key, 404 PROJECT_NOT_FOUND for a project that does not exist, 404 LEGION_NOT_CONFIGURED for a project that names no daemon.",
	Handler:     (*server).getLegionRead,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: legionReadResponse{},
	},
})
