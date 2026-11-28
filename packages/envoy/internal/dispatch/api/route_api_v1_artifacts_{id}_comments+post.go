package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/artifacts/{id}/comments",
	Auth:        authAny,
	Description: "Comment on a document or one of its quotes. A reply names the comment it answers in reply_to, and one block of it in reply_to_block. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT") + " A reply to a deleted comment is 409 POST_DELETED: a withdrawn post takes no new text. " + commentPartiesDescription,
	Handler:     (*server).createArtifactComment,
	Successor:   successorIs(http.MethodPost, "/artifacts/{id}/comments"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
})
