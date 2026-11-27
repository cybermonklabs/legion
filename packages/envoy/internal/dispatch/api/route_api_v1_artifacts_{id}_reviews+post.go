package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/reviews",
	Auth:        authAny,
	Description: "Approve or request changes on a document's latest settled version; answers the open approval ask when present, preserving its thread, and reviews every document that ask named. An approval is 409 SPEC_FORM when a text of a covered document that it reviews breaks the form of The Spec. Its changes_reason is the text that a request for changes sends to the author. A request for changes is never refused.",
	Handler:     (*server).createArtifactReview,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	// A review settles what a person settles, so it needs the approve scope beside person.
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createArtifactReviewInput{},
		Refusals: []routeAnswer{{Status: http.StatusConflict, Body: specFormError{}}},
	},
})
