package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/role-holders",
	Auth:        authAny,
	Description: "The session that holds each Legion role of this issue, as the Legion daemon last published it: {issue, holders}, each {role, session, recorded_at} in workflow order, [] when no session holds a role. A closed issue's read too. An issue Dispatch does not know is 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).getIssueRoleHolders,
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: issueRoleHolders{}},
})
