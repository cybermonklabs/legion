package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/issues/{key}", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Change an issue, a bearer naming its session in actor: title (over %d "+
			"characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the "+
			"issue holds when the write commits rather than replacing them; not with labels), route, parent, "+
			"blocked_by, advances (with advances_revision, the revision the read gave, for a whole-set write), rank, "+
			"assignee (the email of a person who has signed in, or null; anyone may reassign), external_links, repo or "+
			"components ({mode: inherit|explicit|none, ids, reason}); design_gates, the whole set of documents its "+
			"design-gate steps record [{step, artifact_id, holds}], step one of decisions, criteria and spec, "+
			"artifact_id the document that step's gate recorded and holds whether the deployment's gate holds the "+
			"issue's tree on that step (left out or null, stored as null, which the superseded_approval advice answers "+
			"by the project's gated_steps), or [] for none, which appends no event and notifies nobody; and "+
			"refuse_open_foreign_children, true beside a status of "+
			"retro or done, which the Legion daemon's acceptance writes send: while a child of another project is open "+
			"(any status but done and icebox), the write changes nothing and is 409 FOREIGN_CHILD_OPEN, naming each "+
			"such child in issues [{key, project, status}] in key order. A design_gates step named twice, one that is "+
			"none of the three, or an empty artifact_id, and refuse_open_foreign_children beside any other status, are "+
			"400 INVALID_ISSUE. A closed issue takes rank, components, design_gates, an "+
			"external_links write adding no URL and a reopening status, and is 409 ISSUE_CLOSED for anything else. "+
			"Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution "+
			"kind withdrawn naming the closer, and releases its claim; reopening it opens exactly those asks again. "+
			"expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer "+
			"that sequence. A parent that would make the issue its own ancestor, and a parent or blocked_by that would "+
			"make it wait on itself, are 409 ISSUE_CYCLE. With the parent-status rule on, a change that breaks it is "+
			"409 PARENT_STATUS_CONFLICT. Without status-change options, answers the issue with advice; a patch that "+
			"changes nothing appends no event. With include_children, unlink_children, include_parent or dry_run true, "+
			"send status and optionally expected_sequence and actor, with no other issue fields; an option sent as "+
			"false asks for nothing and is taken as absent. The server checks "+
			"the plan, then changes each affected issue in a separate transaction, keeping the sequence fence and "+
			"the progress of a keyed attempt. A dry run returns the plan without writing. This form answers "+
			"{issue, changed}; a refusal carries the changes that committed before it. Closing with include_children "+
			"requires a browser sign-in. On an issue Legion runs, a close, a reopen and a labels or parent write that "+
			"takes the issue out of Legion (unlink_children among them) are 403 DOCUMENT_WRITER_NOT_ARCHITECT from a "+
			"session that is no person, no architect of the issue or of an issue above it, and not the session of the "+
			"project's Legion daemon. A move under a parent, after which Legion runs the issue, is 403 "+
			"DOCUMENT_WRITER_NOT_ARCHITECT from a session that is no person and no architect of the new parent or of "+
			"an issue above it; a move under a parent of another project is judged as a root of the issue's own "+
			"project, which this check does not judge. When Legion runs the issue, or an issue under it, before the "+
			"move, the move is 403 DOCUMENT_WRITER_NOT_ARCHITECT too, wherever the new parent is, from a session that "+
			"is no person and no architect of each such issue or of an issue above it where it stands. Neither check "+
			"of a move passes the session of the project's Legion daemon. With DISPATCH_LEGION_STATUS_SCOPE on, three "+
			"writes of a Legion issue (one that carries the legion label in any case, or is under an issue of its own "+
			"project that does) need a credential that holds the person scope or legion-status, in both forms: a "+
			"status write (the status the issue holds included) where the issue is a Legion issue before the call or "+
			"after it, the removal of its legion label, and a move after which it is no longer a Legion issue. A "+
			"credential that holds neither is 403 LEGION_STATUS_SCOPE_REQUIRED, with an insufficient_scope challenge "+
			"naming legion-status, decided before the first write and before PARENT_STATUS_CONFLICT. The one "+
			"exception is the owning architect's status write: a broker-issued token whose session the Legion "+
			"daemon's own token published as the architect of an issue above, of the issue's own project, before the "+
			"call and after it, writes done, backlog, icebox or triage, with neither unlink_children nor "+
			"include_parent and not done with include_children. When the request's credential names one project, "+
			"the status-change form leaves out each issue of a private project that the credential does not name, "+
			"and each issue that it reaches only through such an issue (a descendant below it, or an ancestor above "+
			"it): it does not move or unlink those issues, and neither its answer nor its dry run names them, and "+
			"the issues of a 409 PARENT_STATUS_CONFLICT leave out such an issue, so they can be empty. A priority of "+
			"0 needs the p0 scope when the write makes the issue's own priority 0, from none or from another value: "+
			"from a credential that does not hold p0 it is 403 OAUTH_SCOPE_REQUIRED naming p0, under a "+
			"WWW-Authenticate challenge for p0, before every refusal that reads the issue (409 ISSUE_CHANGED and 409 "+
			"ISSUE_CLOSED among them), and writes no field of the issue and no event of it. For a close that names "+
			"priority 0 from a credential without p0, the write reads the issue's own priority first, and where it "+
			"is not 0 it refuses the close before it settles the issue's documents; a close whose own priority moves "+
			"away from 0 between that read and the owner's lock can settle the documents and is then refused. "+
			"Keeping P0, lowering it, clearing it and a priority of 1 to 3 need no scope. "+
			"404 ISSUE_NOT_FOUND.", contracts.IssueTitleMax),
		Handler:  s.patchIssueV2,
		Resource: "issues", Kind: kindItem, Key: "{key}",
		Body: requestBody{Kind: bodyJSON, Type: patchIssueV2Input{}},
		Shape: routeShape{
			Status: http.StatusOK, Response: model.Issue{}, Advice: writeAdvice{},
			Also: []routeAnswer{{Status: http.StatusOK, Body: statusChangeAnswer{}}},
			Refusals: []routeAnswer{
				{Status: http.StatusConflict, Body: foreignChildOpenError{}},
				{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
				{Status: http.StatusConflict, Body: statusChangeConflict{}},
				// The status-change form's refusals carry changed at every status the handler
				// or the keyed layer answers one at (writeStatusChangeRefusal,
				// refuseStatusChangeKey): the keyed layer resolves the caller, so a credential
				// that does not resolve is refused there at 401, 429 or 503, and a close's or a
				// reopen's document work refuses at 503.
				{Status: http.StatusConflict, Body: statusChangeRefusal{}},
				{Status: http.StatusBadRequest, Body: statusChangeRefusal{}},
				{Status: http.StatusUnauthorized, Body: statusChangeRefusal{}},
				{Status: http.StatusForbidden, Body: statusChangeRefusal{}},
				{Status: http.StatusNotFound, Body: statusChangeRefusal{}},
				{Status: http.StatusUnprocessableEntity, Body: statusChangeRefusal{}},
				{Status: http.StatusTooManyRequests, Body: statusChangeRefusal{}},
				{Status: http.StatusInternalServerError, Body: statusChangeRefusal{}},
				{Status: http.StatusServiceUnavailable, Body: statusChangeRefusal{}},
			},
		},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		KeyedFor: s.patchIssueKeyedRoute,
		// A close or a reopen writes the state of its asks into their blocks (docs.WriteIssueAskStates).
		Refusals: documentRefusals(issueFieldRefusals(
			issueMissingRefusal, parentLoopRefusal, dependencyLoopRefusal, issueClosedRefusal,
			apicode.Refusal{Condition: apicode.OpenForeignChild, Code: apicode.ForeignChildOpen},
			apicode.Refusal{Condition: apicode.AdvancesRevisionMoved, Code: apicode.AdvancesRevision},
			apicode.Refusal{Condition: apicode.ExternalLinkHeld, Code: apicode.ExternalLinkTaken},
			issueStatusInvalidRefusal,
			parentStatusConflictRefusal,
			apicode.Refusal{Condition: apicode.StatusChangeOptionsInvalid, Code: apicode.StatusChangeInput},
			apicode.Refusal{Condition: apicode.StatusCascadeNeedsBrowser, Code: apicode.BrowserSignInRequired},
			apicode.Refusal{Condition: apicode.ExternalURLInvalid, Code: apicode.InvalidURL},
			apicode.Refusal{Condition: apicode.IssueSequenceMoved, Code: apicode.IssueChanged},
			apicode.Refusal{Condition: apicode.ParentLockContended, Code: apicode.ParentContended},
			rankInvalidRefusal,
			apicode.Refusal{Condition: apicode.IssueRouteInvalid, Code: apicode.RouteInvalid},
			specWriterNotArchitectRefusal,
			apicode.Refusal{Condition: apicode.LegionStatusWriteWithoutScope, Code: apicode.LegionStatusScopeRequired},
		)...),
	}
})
