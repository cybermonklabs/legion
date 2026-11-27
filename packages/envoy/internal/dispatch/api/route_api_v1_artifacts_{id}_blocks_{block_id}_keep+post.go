package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/blocks/{block_id}/keep",
	Auth:        authAny,
	Description: "Record that the caller agrees with a decision, as the text they read it at: {text_sha256}, the sum of the decision's own text as they read it. The block is an agent's decision, a decision block of any document of an issue, on whose node the Keep appends, in one write, a kept_by tuple to the caller and a kept_text_sum value holding the sum, with the decision_keeps row as their projection. Until the decisions step retires it, the block may also be a section heading of an issue's decisions document an earlier release stored, outside `## Replaced`, whose sum is that of the section's markdown from the heading to its last block; that Keep writes the decision_keeps row, which the conversion carries onto the section's decision node. A decision's text sum covers its own words and none of the readings it closes with. A decision whose text changed since is 409 DECISION_CHANGED with nothing written, and a body without the sum is 400 INVALID_KEEP; a repeat of the sum as it stands moves the row. A decision marked replaced, a heading of a retired decisions document, and any other block are 404 TARGET_NOT_FOUND. Answers the keep and appends decision.kept, which wakes nobody.",
	Handler:     (*server).keepDecision,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", Blocks: true, NotFound: "ARTIFACT_NOT_FOUND", BlockNotFound: "TARGET_NOT_FOUND"},
	// Keeping a decision records the person's agreement with it ("Kept by" in the web app), which
	// takes it out of what waits on their review, so it settles what a person settles.
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.DecisionKeep{}, Request: keepDecisionInput{},
	},
})
