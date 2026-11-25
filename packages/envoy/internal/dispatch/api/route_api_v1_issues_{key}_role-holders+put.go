package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/role-holders",
	Auth:        authAny,
	Description: fmt.Sprintf("Publish the session that holds each Legion role of this issue {holders}: each entry {role, session}, role one of architect, planner, implementer, tester, reviewer and merger and session a session id of at most %d bytes, or [] for an issue where no session holds a role. The actor must be session legion-daemon:<project>, the key up to its first -, the session the Legion daemon of the issue's project writes as; any other actor is 403 ROLE_HOLDER_FORBIDDEN. Dispatch takes the session a bearer caller names as the caller names it, so any caller whose credential proves no session of its own can name that one: the deployment's shared token, and a person's own agent token whatever its scopes. While the project's proven_sessions is on (PUT /api/v1/projects/{key}/proven-sessions), the route takes a write only on a credential that proves that session, a broker token of it, and any other is 403 ROLE_HOLDER_NOT_PROVEN, after the 400 and 404 refusals below. The body is the issue's whole set and replaces the stored set under the issue's owner lock, so a session that it does not name holds no role of the issue, two writes at once store the set of the one that finishes last, and a pair that did not change keeps its recorded_at. The write appends no issue event, notifies nobody and takes a closed issue. Each write the route takes marks the issue's project, and from then on a write of the spec or the Verification document of an issue of the project that Legion runs needs a person or an architect of the issue or of an issue above it (403 DOCUMENT_WRITER_NOT_ARCHITECT). Answers 200 {issue, holders} with the set this write stored, in workflow order, each {role, session, recorded_at}. A body without holders, a role that is not one of the six, a session that is not a session id or is over %d bytes, a role or a session named twice and an empty session are each 400 INVALID_ROLE_HOLDER, and nothing is stored; an issue Dispatch does not know is 404 ISSUE_NOT_FOUND. The session of an ended claim keeps its role until the daemon's next publication lands, and its process can still run in that window.", maxRoleHolderSessionBytes, maxRoleHolderSessionBytes),
	Handler:     (*server).putIssueRoleHolders,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: issueRoleHolders{}, Request: issueRoleHoldersInput{},
	},
})
