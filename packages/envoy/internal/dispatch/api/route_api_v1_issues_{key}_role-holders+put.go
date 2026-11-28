package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/issues/{key}/role-holders",
	Auth:        authAny,
	Description: fmt.Sprintf("Publish the session that holds each role of this issue {holders}: each entry {role, session}, role one of architect, planner, implementer, tester, reviewer and merger or a role the deployment defines (a name of the role grammar, ^[a-z][a-z0-9-]{0,31}$, other than controller) and session a session id of at most %d bytes, or [] for an issue where no session holds a role. legion-daemon:<project> is the session the Legion daemon of the issue's project writes as, <project> the key up to its first -. With DISPATCH_LEGION_STATUS_SCOPE on, only a Legion daemon's own broker token publishes: a credential that holds legion-status and proves session legion-daemon:<project>, or one that holds legion-status, proves session legion-daemon:<P> of another project's daemon and names that project P. Every other credential is 403 ROLE_HOLDER_FORBIDDEN, the shared token whose actor names that session included. With it off, the actor must be session legion-daemon:<project>, or session legion-daemon:<P> of another project's daemon whose broker token names P and proves that session, and any other actor is 403 ROLE_HOLDER_FORBIDDEN; a bearer caller names its own session, so any caller whose credential proves no session can name the session of the issue's project's daemon. While the project's proven_sessions is on (PUT /api/v1/projects/{key}/proven-sessions), the route takes a write only on a credential that proves that session, a broker token of it, and any other is 403 ROLE_HOLDER_NOT_PROVEN, after the 400 and 404 refusals below. Each stored pair records whether a daemon's own token wrote it, the issue's own daemon's or another project's daemon's, a pair the write keeps unchanged included, and only such a pair names an architect whose write R26 lets through. The body is the issue's whole set and replaces the stored set under the issue's owner lock, so a session that it does not name holds no role of the issue, two writes at once store the set of the one that finishes last, and a pair that did not change keeps its recorded_at. The write appends no issue event, notifies nobody and takes a closed issue. Each write the route takes marks the issue's project, and from then on a write of the spec or the Verification document of an issue of the project that Legion runs needs a person or an architect of the issue or of an issue above it (403 DOCUMENT_WRITER_NOT_ARCHITECT). Answers 200 {issue, holders} with the set this write stored, Legion's six roles in workflow order and then each other role by name, each {role, session, recorded_at}. A body without holders, a role that is none of those, a session that is not a session id or is over %d bytes, a role or a session named twice and an empty session are each 400 INVALID_ROLE_HOLDER, and nothing is stored; an issue Dispatch does not know is 404 ISSUE_NOT_FOUND. The session of an ended claim keeps its role until the daemon's next publication lands, and its process can still run in that window.", maxRoleHolderSessionBytes, maxRoleHolderSessionBytes),
	Handler:     (*server).putIssueRoleHolders,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: issueRoleHolders{}, Request: issueRoleHoldersInput{},
	},
})
