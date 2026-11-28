package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/comments/{id}/reply",
	Auth:        authBearer,
	Description: "The mentioned session's reply to a delivery; the session names itself in actor. For a mention of a role (role:<name>), the role's live holder may reply too, as the Envoy listener names it; for a mention of a session (session:<id>), so may a session the listener names as having replaced the attempt's session, which it handed that session's unread messages. A listener that does not answer is 503 ENVOY_UNAVAILABLE, and a failed read of the mention is 500, never a refusal. A reply names one block of comment {id} in reply_to_block. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT"),
	Handler:     (*server).replyComment,
	Successor:   successorIs(http.MethodPost, "/comments/{id}/replies"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: apicode.CommentNotFound},
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentReplyInput{},
		// An attempt already answered is answered the stored reply, 200; an error report
		// answers the attempt itself (comment_delivery.go).
		Also: []routeAnswer{
			{Status: http.StatusOK, Body: model.Comment{}},
			{Status: http.StatusOK, Body: model.CommentDelivery{}},
		},
	},
})
