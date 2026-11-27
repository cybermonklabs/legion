package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/messages/{id}/replies", Auth: authBearer, Versions: onlyV2,
		Description: "The targeted session's answer to one delivery attempt of a message {actor, attempt, body | error, " +
			"in_reply_to_block?, quote?, occurrence?}, under a bearer credential: the session names itself in actor, and the attempt must be " +
			"its own; a cookie caller, an attempt that is not its own and a body naming neither or both of body and " +
			"error are 403 REPLY_FORBIDDEN. For an attempt sent for a route to a role (role:<name>), the role's live " +
			"holder, as the Envoy listener names it, may answer it too; for one sent to a session (session:<id>), so " +
			"may a session the listener names as having replaced the attempt's session, which it handed that " +
			"session's unread messages. A listener that does not answer is 503 ENVOY_UNAVAILABLE, and a failed read " +
			"of the attempt is 500, never a refusal. A body is posted as the " +
			"session's reply in the message's " +
			"conversation, its in_reply_to_block naming one block of the message it answers, and marks the attempt sent; " +
			"answers 201 the reply and appends message.answered. Once the attempt is answered, follow_up=true posts other " +
			"text as the session's follow-up, threaded under its first reply (201); with follow_up=false (the default), or " +
			"with text the session already posted there, nothing is posted and the stored message answers 200 marked " +
			"duplicate. An error marks the attempt failed and answers 200 the attempt. 404 MESSAGE_NOT_FOUND for an id no " +
			"message has, or an attempt it does not have. " + replyQuoteDescription("in_reply_to_block", "INVALID_MESSAGE"),
		Handler:  s.replyMessageV2,
		Resource: "replies", Kind: kindCollection, Key: "{id}",
		Query: messageRepliesV2Query{},
		Body:  requestBody{Kind: bodyJSON, Type: messageReplyInput{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: replyRead{},
			Also: []routeAnswer{
				{Status: http.StatusOK, Body: replyRead{}},
				{Status: http.StatusOK, Body: model.MessageDelivery{}},
			},
		},
		Scope:  scopeSession,
		IDs:    v2MessageIDs(),
		Reread: (*server).rereadMessageReply,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageMissingRefusal, replyNotRecipientRefusal, envoyUnreachableRefusal,
			messageInputRefusal, messageInvalidRefusal, valuePastBoundRefusal, bodyDecisionBlockRefusal, postDeletedRefusal,
			targetMissingRefusal, quoteAmbiguousRefusal,
		},
	}
})
