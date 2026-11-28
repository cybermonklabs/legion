package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/plan-reviews",
	Auth:        authAny,
	Description: "Record a plan review's approval of a plan version: {version}, on an issue's plan.md alone, and only while it is a document (409 NOT_PLAN_DOCUMENT otherwise; for a plan an upload stored as a file or an image, the refusal names its kind, and a Markdown upload under its name makes it a document). A plan is never sent to a person for approval, so POST /api/v1/artifacts/{id}/approval-requests refuses it with 409 PLAN_APPROVED_BY_REVIEW; a later plan version makes this approval stale, as a spec's goes stale.",
	Handler:     (*server).createArtifactPlanReview,
	Successor:   successorIs(http.MethodPost, "/artifacts/{id}/plan-reviews"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createPlanReviewInput{},
	},
})
