package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/accept",
	Auth:        authAny,
	Description: "Accept a suggestion, applying its replacement.",
	Handler:     (*server).acceptComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: acceptedComment{}, Request: actorInput{}, OptionalRequest: true},
})
