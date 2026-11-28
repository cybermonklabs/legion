package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/reject",
	Auth:        authAny,
	Description: "Reject a suggestion.",
	Handler:     (*server).rejectComment,
	Successor:   successorIs(http.MethodPut, "/comments/{id}/state"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: apicode.NotFound},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedComment{}, Request: actorInput{}, OptionalRequest: true},
})
