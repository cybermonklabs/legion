package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/reviews",
	Auth:        authAny,
	Description: "Approve or request changes on a document's latest settled version; answers the open approval ask when present, preserving its thread, and reviews every document that ask named.",
	Handler:     (*server).createArtifactReview,
	Successor:   successorIs(http.MethodPost, "/artifacts/{id}/reviews"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	// A review settles what a person settles, so it needs the approve scope beside person.
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.ArtifactReview{}, Request: createArtifactReviewInput{},
	},
})
