package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Publish the session that holds each Legion role of this issue {holders}, a "+
			"bearer naming its session in actor: each entry {role, session}, role one of architect, planner, "+
			"implementer, tester, reviewer and merger and session a session id of at most %d bytes, or [] for an "+
			"issue where no session holds a role. legion-daemon:<project> is the session the Legion daemon of the "+
			"issue's project writes as, <project> the key up to its first -. With DISPATCH_LEGION_STATUS_SCOPE on, "+
			"only a Legion daemon's own broker token publishes: a credential that holds legion-status and proves "+
			"session legion-daemon:<project>, or one that holds legion-status, proves session legion-daemon:<P> of "+
			"another project's daemon and names that project P. Every other credential is 403 "+
			"ROLE_HOLDER_FORBIDDEN, the shared token whose actor names that session included. With it off, the actor "+
			"must be session legion-daemon:<project>, or session legion-daemon:<P> of another project's daemon whose "+
			"broker token names P and proves that session, and any other actor is 403 ROLE_HOLDER_FORBIDDEN; a "+
			"bearer caller names its own session, so any caller whose credential proves no session can name the "+
			"session of the issue's project's daemon. While the project's proven_sessions is on (PATCH "+
			"/api/v2/projects/{key}), the route takes a write only on a credential that proves that session, a broker "+
			"token of it, and any other is 403 ROLE_HOLDER_NOT_PROVEN, after the 400 refusals below. Each stored pair "+
			"records whether a daemon's own token wrote it, the issue's own daemon's or another project's daemon's, a "+
			"pair the write keeps unchanged included, and only "+
			"such a pair names an architect whose write R26 lets through. The body is the "+
			"issue's whole set and replaces the stored set under the issue's owner lock, so a session that it does "+
			"not name holds no role of the issue, two writes at once store the set of the one that finishes last, "+
			"and a pair that did not change keeps its recorded_at. A write that changes the set of pairs appends one "+
			"issue event, issue.role_holders_changed, whose payload is {key, holders, previous_holders}: the set it "+
			"stored and the set it replaced, each in workflow order, each pair {role, session, recorded_at}, as the "+
			"write's actor and at the write's time; GET /api/v2/issues/{key}/events serves it, so the log says which "+
			"session held each role at a past time, from the release that writes the event on. A write that keeps "+
			"every pair appends nothing. The event is quiet and notifies nobody, and the write takes a closed issue. "+
			"Each write the route takes marks the issue's project, and from then on a "+
			"write of the spec or the Verification document of an issue of the project that Legion runs needs a "+
			"person or an architect of the issue or of an issue above it (403 DOCUMENT_WRITER_NOT_ARCHITECT). "+
			"Answers 200 the set this write stored, %s; a body naming the set already stored, in any order, on a "+
			"project a write has marked, whose every pair already records what this write's credential would record, "+
			"answers that set and writes nothing. A body without holders, a role that is not one of the six, a "+
			"session that "+
			"is not a session id or is over %d bytes, a role or a session named twice and an empty session are each "+
			"400 INVALID_ROLE_HOLDER, and nothing is stored. The session of an ended claim keeps its role until the "+
			"daemon's next publication lands, and its process can still run in that window. 404 ISSUE_NOT_FOUND for "+
			"an issue that does not exist.", maxRoleHolderSessionBytes, issueRoleHoldersText, maxRoleHolderSessionBytes),
		Handler:  s.putIssueRoleHoldersV2,
		Resource: "role-holder-set", Kind: kindSingleton,
		Body:  requestBody{Kind: bodyJSON, Type: issueRoleHoldersInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: issueRoleHolders{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, issueMissingRefusal,
			{Condition: apicode.RoleHolderNotTheDaemon, Code: apicode.RoleHolderForbidden},
			{Condition: apicode.RoleHoldersInvalid, Code: apicode.InvalidRoleHolder},
			{Condition: apicode.RoleHolderSessionUnproven, Code: apicode.RoleHolderNotProven},
		},
	}
})
