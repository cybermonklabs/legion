package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/plan-reviews", Auth: authAny, Versions: onlyV2,
		Description: "Record a plan review's approval of a plan version {version, actor?}, any authenticated caller, on " +
			"an issue's plan.md alone, and only while it is a document (409 NOT_PLAN_DOCUMENT otherwise; for a plan an " +
			"upload stored as a file or an image, the refusal names its kind, and a Markdown upload under its name makes " +
			"it a document): version is any settled version from 1 to the latest (400 INVALID_REVIEW otherwise). A plan " +
			"is never sent to a person for approval, so POST /api/v2/artifacts/{id}/approval-requests refuses it; a " +
			"later plan version makes this approval stale, as a spec's goes stale. Answers 201 " +
			"the review. 409 NO_VERSION, 409 ISSUE_CLOSED, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.createArtifactPlanReviewV2,
		Resource: "plan-reviews", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: createPlanReviewInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: model.ArtifactReview{}},
		Scope: scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, noSettledVersionRefusal, reviewInvalidRefusal,
			{Condition: apicode.NotThePlan, Code: apicode.NotPlanDocument},
		},
	}
})
