package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/deliveries",
	Auth:        authAny,
	Description: "Retry a comment mention delivery; target is required when it has multiple mentions.",
	Handler:     (*server).createCommentDelivery,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "COMMENT_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.CommentDelivery{}, Request: createCommentDeliveryInput{},
	},
})
