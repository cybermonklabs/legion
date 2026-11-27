package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/results/{id}/retract",
	Auth:        authAny,
	Description: "Retract one recorded result, with the reason it is wrong: {reason, actor?}. {id} is the result's own id. The retraction is a record of its own: the result is neither updated nor deleted, and both records stay readable with the reason and the actor who wrote it. No surface counts a retracted result as evidence. Three callers are admitted and every other is 403 RETRACTION_FORBIDDEN: the session that recorded the result, a session holding the Dispatch claim of the issue the result was recorded on, and a caller whose credential carries a person, which is a personal token or a signed-in person's own session. A closed issue and a check of any when are retracted here, which is what recording refuses. A blank reason is 400 RETRACTION_INPUT, an id no result has is 404 RESULT_NOT_FOUND, and a result a retraction already stands against is 409 RESULT_RETRACTED. The answer is the retraction: {result_id, reason, session, actor, created_at, event_seq}. It is appended as result.retracted on the check's issue.",
	Handler:     (*server).retractCheckResult,
	// A result's retraction: the row belongs to the project of the issue the result was
	// recorded on, one hop through check_results (projectOfCheckResult).
	WriteClass: scopeProject,
	ID:         &idRoute{Reason: "a check result id"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.CheckResultRetraction{},
		Request: checkResultRetractionInput{},
	},
})
