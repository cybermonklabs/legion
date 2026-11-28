package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/versions/{number}/reviews",
	Auth:        authAny,
	Description: "Approve or request changes on one version of a document {state, reason?}: {number} is the version the caller read, and the route requires it. The review is recorded only when {number} is the version it would record, the document's latest settled version or, with an approval ask open, the version that ask names for the document; any other is 409 REVIEW_VERSION_MOVED, naming the version the review would record, with nothing written, and a {number} that is not a whole number of 1 or more is 400 INVALID_REVIEW. With an approval ask open the review answers it, preserving its thread, and reviews every document the ask named, each at the version the ask names. This is the route a client sends when the version is a guard: a build without it answers no route, rather than recording the review without the guard.",
	Handler:     (*server).createArtifactVersionReview,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	// The review that names the version its caller read settles what POST
	// /api/v1/artifacts/{id}/reviews settles, with that version as a guard.
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createArtifactReviewInput{},
	},
})
