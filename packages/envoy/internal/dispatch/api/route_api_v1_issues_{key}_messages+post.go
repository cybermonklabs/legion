package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/messages",
	Auth:        authAny,
	Description: "Post an issue message; target and delivery address a session or role. A reply names the message it answers in in_reply_to, and one block of it in in_reply_to_block. " + replyQuoteDescription("in_reply_to_block", "INVALID_MESSAGE") + " A reply to a deleted message is 409 POST_DELETED: a withdrawn post takes no new text.",
	Handler:     (*server).createMessage,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: shapedMessage{}, Request: createMessageInput{}, Advice: writeAdvice{},
	},
})
