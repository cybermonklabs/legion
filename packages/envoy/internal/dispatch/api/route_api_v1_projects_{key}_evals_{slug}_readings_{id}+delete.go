package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/projects/{key}/evals/{slug}/readings/{id}",
	Auth:        authAny,
	Description: "Refused, whatever the id names: 405 READING_APPEND_ONLY. A reading is never deleted; a correction is a new reading naming the one it replaces in replaces.",
	Handler:     (*server).refuseReadingChange,
	// The route writes nothing and reads no row: it refuses every delete of a stored reading
	// (405 READING_APPEND_ONLY), whatever reading the path names.
	WriteClass: scopeNoWrite,
	ID: &idRoute{
		Reason: "an eval reading's id, which the route refuses to delete whatever reading it names (405 READING_APPEND_ONLY)",
	},
	Shape: routeShape{
		// The route answers no success: 204 is the shape a success with no body would take, and
		// the one answer it gives is the 405, with the plain error body the default answer
		// covers.
		Status: http.StatusNoContent,
	},
})
