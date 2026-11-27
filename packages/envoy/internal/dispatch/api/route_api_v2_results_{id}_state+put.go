package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/results/{id}/state", Auth: authAny, Versions: onlyV2,
		Description: "Retract one recorded result, with the reason it is wrong {state: retracted, reason}, a bearer " +
			"naming its session in actor. The retraction is a record of its own: the result is neither updated nor " +
			"deleted, and both stay readable. Three callers are admitted and every other is 403 RETRACTION_FORBIDDEN: " +
			"the session that recorded the result, a session holding the Dispatch claim of the issue it was recorded " +
			"on, and a caller whose credential carries a person. A closed issue and a check of any when are retracted " +
			"here. Answers the result with retracted {reason, actor, created_at}, and appends result.retracted on the " +
			"check's issue; the same retraction by the caller who made it answers the result and writes nothing, and " +
			"any other retraction of a retracted result is 409 RESULT_RETRACTED. A blank reason is 400 " +
			"RETRACTION_INPUT, any other state 400 STATE_INVALID; 404 RESULT_NOT_FOUND for an id no result has.",
		Handler:  s.putResultStateV2,
		Resource: "state", Kind: kindState,
		Body:  requestBody{Kind: bodyStateUnion, Arms: map[string]any{"retracted": retractResultStateInput{}}},
		Shape: routeShape{Status: http.StatusOK, Response: model.CheckResult{}},
		Scope: scopeProject, ProjectOf: projectPathResult,
		IDs: &idRoute{Reason: "a check result id"},
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, resultMissingRefusal,
			{Condition: apicode.ResultRetractedBefore, Code: apicode.ResultRetracted},
			{Condition: apicode.RetractionByOther, Code: apicode.RetractionForbidden},
			{Condition: apicode.RetractionReasonBlank, Code: apicode.RetractionInput},
		},
	}
})
