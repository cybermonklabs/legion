package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/proposed",
	Auth:        authAny,
	Description: "The nodes under the issue whose node state is proposed and that the caller may see: {issue, nodes}, each {node, excerpt}, node the graph node as every read answers it (kind, id, ref, state, visibility and the rest) and excerpt the words an edge at it shows, null where it has none. A node under the issue is the issue itself, a document of the issue or one of its blocks, an ask, a comment or a message of the issue or one of their blocks, and a lens, a finding, a risk or a constraint standing under one of those, in the order of their kind and their id. It takes no query. An issue Dispatch does not hold is 404 NOT_FOUND.",
	Handler:     (*server).listIssueProposed,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueProposed{}},
})
