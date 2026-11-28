package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/role-holders",
	Auth:        authAny,
	Description: "The sessions that hold the roles of this issue, as the Legion daemon last published them: {issue, holders}, each {role, session, claim, works_step, recorded_at}, claim the Legion claim token the session holds its role by (empty where the publication named none) and works_step whether that claim works the role's step. Legion's six roles come in the order of the default steps and then each role the deployment defines by name, the sessions of one role by session, since a role can have more than one session; [] when no session holds a role. A closed issue's read too. An issue Dispatch does not know is 404 ISSUE_NOT_FOUND.",
	Handler:     (*server).getIssueRoleHolders,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape:       routeShape{Status: http.StatusOK, Response: issueRoleHolders{}},
})
