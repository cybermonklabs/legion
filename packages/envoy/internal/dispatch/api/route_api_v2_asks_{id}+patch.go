package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/asks/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Change an open ask, one of two ways a request: reword it {question?, options?, multiple?, urgency?}, " +
			"its author or a credential holding the person scope (another session's ask is 403 NOT_AUTHOR otherwise), a " +
			"block ask's `:::ask` block rewritten with it; or hand it to another person {asked_of}, the email of somebody " +
			"who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise), which needs the person scope: the ask waits on them " +
			"from now on, wherever its issue goes, and naming the assignee of its own issue empties the field instead, so the " +
			"ask follows its issue again; either way the row records who handed it on and when (asked_of_by, asked_of_at). " +
			"A body naming both, or neither, is 400 INVALID_ASK. A bearer names its session in actor on a reword; a hand-on " +
			"is the person's act, recorded as the person the credential acts for. Answers the ask as GET " +
			"/api/v2/asks/{id} reads it. An ask that is not open is 409 ASK_NOT_OPEN, an approval request's wording 409 " +
			"ASK_KIND_FIXED, an ask on a closed issue 409 ISSUE_CLOSED; 404 ASK_NOT_FOUND. " +
			specWriterDocumentText("a reword of a block ask in its spec or Verification document"),
		Handler:  s.editAskV2,
		Resource: "asks", Kind: kindItem, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: editAskV2Input{}},
		Shape:       routeShape{Status: http.StatusOK, Response: askWriteV2{}},
		Authorities: []authorityRule{askAskedOfAuthority},
		Scope:       scopeProject, ProjectOf: projectPathAsk,
		IDs: v2AskIDs(),
		// A reword of a block ask rewrites its block (docs.SetAskBlockText).
		Refusals: documentRefusals(
			undeclaredKeyRefusal, askMissingRefusal, notAuthorRefusal, assigneeNotSignedInRefusal, valuePastBoundRefusal,
			issueClosedRefusal, askBlockMissingRefusal, documentPastSizeBoundRefusal, askInvalidRefusal,
			apicode.Refusal{Condition: apicode.AskNotOpenToEdit, Code: apicode.AskNotOpen},
			apicode.Refusal{Condition: apicode.AskKindChange, Code: apicode.AskKindFixed},
			apicode.Refusal{Condition: apicode.AskBlockUnrepresentable, Code: apicode.AskBlockText},
			specWriterNotArchitectRefusal,
		),
	}
})
