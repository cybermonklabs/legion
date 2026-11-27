package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}",
	Auth:        authAny,
	Description: "Read an issue with its open asks, primary document, children, and progress: {tasks, children}, its spec's task-list items (`- [ ]` / `- [x]`, nested lists included) and its direct children as {done, total}, each null when it has none. blocks is the keys of the issues whose blocked_by names this one, sorted, [] when none; GET /api/v1/issues lists no blocks. comment_count is how many comments GET /api/v1/issues/{key}/comments lists, left out when the issue holds none.",
	Handler:     (*server).getIssue,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueDetail{}},
})
