package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/reviews", Auth: authAny, Versions: onlyV2,
		Description: "Approve or request changes on a document {state: approved or changes_requested, reason?, version?} " +
			"(the person and approve scopes); changes_requested needs a reason (400 REASON_REQUIRED), and any other " +
			"state, or a version that is an integer below 1, is 400 INVALID_REVIEW (a version that is no integer 400 " +
			"INVALID_JSON). The review records the document's latest settled version, or, with an approval ask " +
			"open, the version that ask names for it, and then answers that ask, preserving its thread, and reviews every " +
			"document it named, each at the version it names. version is the version the caller read, a guard: a review " +
			"that would record another is 409 REVIEW_VERSION_MOVED, naming the version it would record, with nothing " +
			"written. Answers 201 the review of this document. 409 NO_VERSION, 409 ISSUE_CLOSED, 400 NOT_DOCUMENT, 400 " +
			"CAP_EXCEEDED for a reason past the bound, 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.createArtifactReviewV2,
		Resource: "reviews", Kind: kindCollection, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: artifactReviewV2Input{}},
		Shape:       routeShape{Status: http.StatusCreated, Response: model.ArtifactReview{}},
		OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
		Scope:       scopeProject, ProjectOf: projectPathArtifact,
		IDs: v2DocumentIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, documentMissingRefusal, issueClosedRefusal, artifactNotDocumentRefusal,
			valuePastBoundRefusal, noSettledVersionRefusal, reviewInvalidRefusal,
			reasonMissingRefusal,
			{Condition: apicode.ReviewedVersionMoved, Code: apicode.ReviewVersionMoved},
		},
	}
})
