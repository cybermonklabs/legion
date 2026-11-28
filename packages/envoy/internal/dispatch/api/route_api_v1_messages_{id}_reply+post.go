package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/messages/{id}/reply",
	Auth:        authBearer,
	Description: "The targeted session's reply to a delivery; the session names itself in actor. For an attempt sent for a route to a role (role:<name>), the role's live holder may reply too, as the Envoy listener names it; for one sent to a session (session:<id>), so may a session the listener names as having replaced the attempt's session, which it handed that session's unread messages. A listener that does not answer is 503 ENVOY_UNAVAILABLE, and a failed read of the attempt is 500, never a refusal. Once the attempt is answered, ?follow_up=true posts other text as the session's follow-up, threaded under its first reply; with follow_up=false or absent, or with text the session already posted there, nothing is posted and the stored message comes back marked duplicate. Any other follow_up is 400 MESSAGE_INPUT. A reply, never a follow-up, names one block of message {id} in in_reply_to_block. " + replyQuoteDescription("in_reply_to_block", "INVALID_MESSAGE"),
	Handler:     (*server).replyMessage,
	Successor:   successorIs(http.MethodPost, "/messages/{id}/replies"),
	WriteClass:  scopeSession,
	ID:          &idRoute{Kind: "message", NotFound: apicode.MessageNotFound},
	Shape: routeShape{
		Status: http.StatusCreated, Response: replyRead{}, Request: messageReplyInput{}, Params: messageReplyQuery{},
		// An attempt already answered is answered the stored reply, 200, marked duplicate
		// when nothing was posted; an error report answers the attempt itself.
		Also: []routeAnswer{
			{Status: http.StatusOK, Body: replyRead{}},
			{Status: http.StatusOK, Body: model.MessageDelivery{}},
		},
	},
})
