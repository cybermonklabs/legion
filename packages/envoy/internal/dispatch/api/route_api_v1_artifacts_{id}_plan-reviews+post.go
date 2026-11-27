package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/plan-reviews",
	Auth:        authAny,
	Description: "Record a plan review's approval of a plan version: {version}, on an issue's plan.md alone (409 NOT_PLAN_DOCUMENT otherwise). A plan is never sent to a person for approval, so POST /api/v1/artifacts/{id}/approval-requests refuses it with 409 PLAN_APPROVED_BY_REVIEW; a later plan version makes this approval stale, as a spec's goes stale.",
	Handler:     (*server).createArtifactPlanReview,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createPlanReviewInput{},
	},
})
