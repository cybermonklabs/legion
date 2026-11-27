package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/resolve",
	Auth:        authAny,
	Description: "Resolve a comment thread.",
	Handler:     (*server).resolveComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedComment{}, Request: actorInput{}, OptionalRequest: true},
})
