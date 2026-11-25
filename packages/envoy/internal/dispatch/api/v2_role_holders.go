package api

// v2_role_holders.go is an issue's role holders under /api/v2: the two routes #4228 added to
// /api/v1 before /api/v2's change merged (testdata/v1-only-routes.txt), each answered at
// /api/v2/issues/{key}/role-holder-set, a singleton of the issue whose PUT replaces it whole. A
// singleton that holds a set is named the singular of the plural /api/v1 named it by, and -set
// (the architect's ruling, dispatch://INFRA-4071/comment/fa6c9d57-e6a5-462b-8d95-063e5b48b163),
// so the name is singular, as R1.1 of dispatch://INFRA-2946/spec names a singleton, and a reader
// of the /api/v1 name can tell it. Both run the work their /api/v1 route runs
// (getIssueRoleHolders, putIssueRoleHoldersFrom), the issue checked first and answered 404
// ISSUE_NOT_FOUND (dispatch://INFRA-4071/spec R7).

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/legionagents"
)

// issueRoleHoldersText is what both role-holder routes' descriptions say of the set they answer.
const issueRoleHoldersText = "{issue, holders}, each holder {role, session, recorded_at} in workflow order, [] when " +
	"no session holds a role"

// v2RoleHolderRows are the role-holder routes of an issue, #4228's two rows of /api/v1.
func (s *server) v2RoleHolderRows() []routeRow {
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/role-holder-set", Auth: authAny, Versions: onlyV2,
			Description: "The session that holds each Legion role of this issue, as the Legion daemon last published " +
				"it: " + issueRoleHoldersText + ". A closed issue's read too. 404 ISSUE_NOT_FOUND for an issue that does " +
				"not exist.",
			Handler:  s.getIssueRoleHolders,
			Resource: "role-holder-set", Kind: kindSingleton,
			Shape:    routeShape{Status: http.StatusOK, Response: issueRoleHolders{}},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
		{
			Method: http.MethodPut, Pattern: "/issues/{key}/role-holder-set", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("Publish the session that holds each Legion role of this issue {holders}, a "+
				"bearer naming its session in actor: each entry {role, session}, role one of architect, planner, "+
				"implementer, tester, reviewer and merger and session a session id of at most %d bytes, or [] for an "+
				"issue where no session holds a role. The actor must be session legion-daemon:<project>, the key up to "+
				"its first -, the session the Legion daemon of the issue's project writes as; any other actor is 403 "+
				"ROLE_HOLDER_FORBIDDEN. Dispatch takes the session a bearer caller names as the caller names it, so any "+
				"caller whose credential proves no session of its own can name that one: the deployment's shared token, "+
				"and a person's own agent token whatever its scopes. While the project's proven_sessions is on (PATCH "+
				"/api/v2/projects/{key}), the route takes a write only on a credential that proves that session, a broker "+
				"token of it, and any other is 403 ROLE_HOLDER_NOT_PROVEN, after the 400 refusals below. The body is the "+
				"issue's whole set and replaces the stored set under the issue's owner lock, so a session that it does "+
				"not name holds no role of the issue, two writes at once store the set of the one that finishes last, "+
				"and a pair that did not change keeps its recorded_at. The write appends no issue event, notifies nobody "+
				"and takes a closed issue. Each write the route takes marks the issue's project, and from then on a "+
				"write of the spec or the Verification document of an issue of the project that Legion runs needs a "+
				"person or an architect of the issue or of an issue above it (403 DOCUMENT_WRITER_NOT_ARCHITECT). "+
				"Answers 200 the set this write stored, %s; a body naming the set already stored, in any order, on a "+
				"project a write has marked, answers that set and writes nothing. A body without holders, a role that "+
				"is not one of the six, a session that "+
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
		},
	}
}

// putIssueRoleHoldersV2 answers PUT /api/v2/issues/{key}/role-holder-set: the publication PUT
// /api/v1/issues/{key}/role-holders makes (putIssueRoleHoldersFrom), once the no-op rule of
// dispatch://INFRA-4071/spec R8 has found the body is a change. The set records no setter, and the
// daemon of the issue's project is its one writer, so a body naming the stored set answers it and
// writes nothing whoever sends it (noSetterRecorded), once the core has asked the proof a write
// would ask for and found the project marked (putIssueRoleHoldersFrom). The set is compared in
// workflow order, the order it is stored and answered in, since the body names a set and its
// order says nothing.
func (s *server) putIssueRoleHoldersV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	body := v2Body[issueRoleHoldersInput](r)
	s.putIssueRoleHoldersFrom(w, r, body, func(stored issueRoleHolders) bool {
		sent := body
		ordered := slices.SortedFunc(slices.Values(*body.Holders), func(left, right issueRoleHolder) int {
			return cmp.Compare(slices.Index(legionagents.Roles, left.Role), slices.Index(legionagents.Roles, right.Role))
		})
		sent.Holders = &ordered
		held := make([]issueRoleHolder, len(stored.Holders))
		for index, holder := range stored.Holders {
			held[index] = issueRoleHolder{Role: holder.Role, Session: holder.Session}
		}
		present := sent
		present.Holders = &held
		return putChangesNothing(sent, present, noSetterRecorded, "")
	})
}
