package api

import (
	"fmt"
	"net/http"
)

// issuePriorityFields says what an issue read's three priority fields are, in the words the issue
// read and the listing both give them (refs.IssuePriorityTerms resolves the second and the third).
var issuePriorityFields = fmt.Sprintf("priority, the issue's own, null where it sets none; resolved_priority, the priority it works at: its own, else that of the nearest issue above it on its parent chain that has one, in any project and within %d links, else null; and priority_from, the key of the issue resolved_priority comes from, null where it is the issue's own or there is none. resolved_priority and priority_from are always present, null included", parentDepthCap)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}",
	Auth:        authAny,
	Description: "Read an issue with its open asks, primary document, children, and progress: {tasks, children}, its spec's task-list items (`- [ ]` / `- [x]`, nested lists included) and its direct children as {done, total}, each null when it has none. It carries " + issuePriorityFields + ". blocks is the keys of the issues whose blocked_by names this one, sorted, [] when none; GET /api/v1/issues lists no blocks. comment_count is how many comments GET /api/v1/issues/{key}/comments lists, left out when the issue holds none.",
	Handler:     (*server).getIssue,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueDetail{}},
})
