package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/deploys",
	Auth:        authAny,
	Description: "Read each program's current deploy in ?repo=owner/repo, ordered by program, with its source, writer and known or unknown workflow run attempt.",
	Handler:     (*server).listDeploys,
	Successor:   successorIs(http.MethodGet, "/deploys"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: deploysResponse{}, Params: deploysQuery{},
	},
})
