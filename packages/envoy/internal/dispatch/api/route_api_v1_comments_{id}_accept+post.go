package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/accept",
	Auth:        authAny,
	Description: "Accept a suggestion, applying its replacement. A suggestion in a retired document, which takes no new text, is 409 DOCUMENT_RETIRED and writes nothing; in an issue's decisions document once it is retired it is 409 DECISIONS_ARE_NODES instead: a decision is a `decision` block in the spec.",
	Handler:     (*server).acceptComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: acceptedComment{}, Request: actorInput{}, OptionalRequest: true},
})
