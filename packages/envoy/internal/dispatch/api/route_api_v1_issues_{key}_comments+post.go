package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/comments",
	Auth:        authAny,
	Description: "Comment on an issue, a document quote, or an ask. A reply names the comment it answers in reply_to, and one block of it in reply_to_block; with ask_id in place of reply_to, reply_to_block names one part of that ask. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT") + " A reply to a deleted comment is 409 POST_DELETED: a withdrawn post takes no new text. " + commentPartiesDescription,
	Handler:     (*server).createComment,
	Replay:      func(s *server) keyedReplay { return replayRecovered(s, s.replayComment) },
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: commentWriteResponse{}, Request: commentInput{}, Advice: writeAdvice{},
	},
})
