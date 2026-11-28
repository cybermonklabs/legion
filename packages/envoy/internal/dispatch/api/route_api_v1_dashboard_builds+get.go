package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/dashboard/builds",
	Auth:        authAny,
	Description: "Which dashboard builds are served: {serving, live}, serving the entry chunk (/assets/index-*.js) of the build this process serves (empty when none), live the distinct entry chunks of the builds every live server process serves, sorted. A tab offers its reload notice only when live holds exactly one entry and it is not the tab's own.",
	Handler:     (*server).getDashboardBuilds,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: dashboardBuilds{}},
})
