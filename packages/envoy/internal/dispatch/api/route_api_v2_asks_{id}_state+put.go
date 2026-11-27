package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/asks/{id}/state", Auth: authAny, Versions: onlyV2,
		Description: "Move an ask to a state, its body by state. {state: answered, selected?, text?, expected_edited_at, " +
			"expected_answer_at?} answers it, which needs the person scope, and the approve scope for an approval request: " +
			"expected_edited_at is the edited_at of the wording answered, null for one never edited (absent is 400 " +
			"ASK_REVISION_REQUIRED, one the ask has moved past 409 ASK_EDITED), and a change of the current answer by the " +
			"person who gave it names that answer's at in expected_answer_at (another person's change is 403 NOT_ANSWERER, " +
			"one the answer has moved past 409 ASK_ANSWER_CHANGED, an approval's 409 ASK_APPROVAL_REVIEW). {state: resolved " +
			"or retracted, reason} closes it without an answer, for any caller, a bearer naming its session in actor. " +
			"{state: declined, reason?} declines it, which needs the person scope: the reason stands beside the words " +
			"Dispatch writes for the asking agent, and an approval request is 409 ASK_NOT_DECLINABLE. A block ask's block " +
			"records the state too. A body equal to the ask's present state and fields, from the caller who set them, " +
			"answers 200 and writes nothing; any other first answer to an answered ask, and a resolve or a decline of one, " +
			"is 409 ASK_ANSWERED, and a resolved ask 409 ASK_RESOLVED. An approve-only token (the secrets broker's " +
			"proxy for a person's approvals) may send state answered of an approval request and no other declared " +
			"state: its answer of any other ask, a saved answer under an Idempotency-Key and a repeat of the person's " +
			"own answer included, and each other state this route declares (resolved, retracted, declined) are 403 " +
			"APPROVE_ONLY_TOKEN. " +
			"Answers the ask as GET /api/v2/asks/{id} reads it; " +
			"400 STATE_INVALID for a state it does not declare, 400 INVALID_ANSWER, INVALID_RESOLUTION or CAP_EXCEEDED for a body its " +
			"state refuses, 409 ISSUE_CLOSED on a closed issue, 404 ASK_NOT_FOUND. " +
			specWriterDocumentText("a retraction or a resolution of a block ask whose block, or a copy of it, is in its "+
				"spec or Verification document,"),
		Handler:  s.putAskStateV2,
		Resource: "state", Kind: kindState,
		Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
			"answered":  answerAskStateInput{},
			"resolved":  resolveAskStateInput{},
			"retracted": resolveAskStateInput{},
			"declined":  declineAskStateInput{},
		}},
		Shape:       routeShape{Status: http.StatusOK, Response: askWriteV2{}},
		Authorities: []authorityRule{askAnsweredAuthority, askDeclinedAuthority},
		Scope:       scopeProject, ProjectOf: projectPathAsk,
		IDs: v2AskIDs(),
		// A block ask's state is written into its block (docs.SetBlockAttributes, docs.SettleCopiesOf).
		Refusals: documentRefusals(
			undeclaredKeyRefusal, askMissingRefusal, issueClosedRefusal, valuePastBoundRefusal, askBlockMissingRefusal,
			apicode.Refusal{Condition: apicode.AskAnsweredBefore, Code: apicode.AskAnswered},
			apicode.Refusal{Condition: apicode.AnsweredFirstByAnother, Code: apicode.AskClosed},
			apicode.Refusal{Condition: apicode.AskResolvedBefore, Code: apicode.AskResolved},
			apicode.Refusal{Condition: apicode.AnswerMoved, Code: apicode.AskAnswerChanged},
			apicode.Refusal{Condition: apicode.ApprovalAnsweredBefore, Code: apicode.AskApprovalReview},
			apicode.Refusal{Condition: apicode.QuestionEditedSinceRead, Code: apicode.AskEdited},
			apicode.Refusal{Condition: apicode.ApprovalDeclined, Code: apicode.AskNotDeclinable},
			apicode.Refusal{Condition: apicode.RevisionMissing, Code: apicode.AskRevisionRequired},
			apicode.Refusal{Condition: apicode.AnswerInvalid, Code: apicode.InvalidAnswer},
			apicode.Refusal{Condition: apicode.ResolutionInvalid, Code: apicode.InvalidResolution},
			apicode.Refusal{Condition: apicode.NotTheAnswerer, Code: apicode.NotAnswerer},
			reasonMissingRefusal,
			specWriterNotArchitectRefusal,
		),
	}
})
