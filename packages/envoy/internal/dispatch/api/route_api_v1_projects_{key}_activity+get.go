package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/activity",
	Auth:        authAny,
	Description: fmt.Sprintf("The events of a project's issues counted per ISO week over the last ?weeks= weeks (default %d, 1 to %d), this week last: {project, weeks: [{week (2026-W45), start (Monday 00:00 UTC), events, kinds: {event type: count}}]}, every week of the span listed, a week with no event as 0. 400 INVALID_PROJECT for a key that is no project key, 400 INVALID_QUERY for a weeks outside that bound, 404 PROJECT_NOT_FOUND for a project Dispatch does not hold.", projectActivityWeeksDefault, projectActivityWeeksMax),
	Handler:     (*server).getProjectActivity,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: projectActivity{}, Params: projectActivityQuery{},
	},
})
