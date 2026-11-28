package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/blocks/{block_id}/keep",
	Auth:        authAny,
	Description: "Record that the caller agrees with the decision that block opens, as the text they read it at: {text_sha256}, the decision's own text_sha256 from GET /api/v1/issues/{key}/decisions. A decision whose text changed since is 409 DECISION_CHANGED with nothing written, and a body without the sum is 400 INVALID_KEEP; a repeat of the sum as it stands moves the row. The block must be the heading block of a decision of an issue's decisions.md outside `## Replaced`: any other block is 404 TARGET_NOT_FOUND. Answers the keep and appends decision.kept, which wakes nobody.",
	Handler:     (*server).keepDecision,
	Successor:   successorIs(http.MethodPut, "/issues/{key}/decisions/{block_id}/state"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", Blocks: true, NotFound: apicode.ArtifactNotFound, BlockNotFound: apicode.BlockNotFound},
	// Keeping a decision records the person's agreement with it ("Kept by" in the web app), which
	// takes it out of what waits on their review, so it settles what a person settles.
	OAuthScopes:  routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly:  true,
	ApproveState: "kept",
	Shape: routeShape{
		Status: http.StatusOK, Response: model.DecisionKeep{}, Request: keepDecisionInput{},
	},
})
