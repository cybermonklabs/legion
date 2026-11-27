package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/plan-reviews",
	Auth:        authAny,
	Description: "Record a plan review's approval of a plan version: {version}, on an issue's plan.md alone (409 NOT_PLAN_DOCUMENT otherwise). A plan is never sent to a person for approval, so POST /api/v1/artifacts/{id}/approval-requests refuses it with 409 PLAN_APPROVED_BY_REVIEW; a later plan version that changes what this approval covers makes it stale, as a spec's goes stale: on an issue Dispatch has converted to one tree, a change to its plan_task or plan_role blocks or their relations, and on any other, a version the approval does not carry to by its text.",
	Handler:     (*server).createArtifactPlanReview,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createPlanReviewInput{},
	},
})
