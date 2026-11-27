package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/asks/open",
	Auth:        authAny,
	Description: "Open asks in one scope, with counts: ?author_session= for one session's own, or ?project= for every open ask on a project's issues and documents; exactly one is required. A ?project= read also carries default_assignee, the login of the person the project defaults to, whom a question on an issue nobody holds reaches, or null where no project has the key: every credential that may make this read reads it, the shared token and a pod's among them, also where no open ask waits on that person. An ?author_session= read carries no default_assignee. Listing every project's default and moving one still need the person scope.",
	Handler:     (*server).listOpenAsks,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: openAsksResponse{}, Params: openAsksQuery{}},
})
