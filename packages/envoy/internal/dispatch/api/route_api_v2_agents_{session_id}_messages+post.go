package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/agents/{session_id}/messages", Auth: authAny, Versions: onlyV2,
		Description: "Send an issue-less targeted message to a session {body, delivery, in_reply_to?, in_reply_to_block?, " +
			"quote?, occurrence?, connected_only?}, which needs the person scope: delivery is btw, aside or steer, and a reply names a message " +
			"of this session's conversation in in_reply_to. It is answered 201 once its own " +
			"attempt has settled. A session the Envoy listener knows and that is not running is a recipient: the listener " +
			"stores the message and hands it over when that session runs again, and the attempt is recorded queued; one " +
			"the listener knows nothing about fails the attempt with \"no live session <id>\". connected_only asks for a " +
			"session running now and fails the attempt with \"the session is stopped\", nothing stored, as a person's own " +
			"Send or Aside whose whole body is one /model or /effort line always does. A repeat under its Idempotency-Key " +
			"answers the message as it stands, its owed attempt sent first. 400 INVALID_MESSAGE or MESSAGE_INPUT for a " +
			"message it refuses, the second for an in_reply_to outside this session's conversation among others. " +
			replyQuoteDescription("in_reply_to_block", "INVALID_MESSAGE"),
		Handler:  s.createAgentMessageV2,
		Resource: "messages", Kind: kindCollection, Key: "{id}",
		Body:        requestBody{Kind: bodyJSON, Type: createAgentMessageInput{}},
		Shape:       routeShape{Status: http.StatusCreated, Response: shapedMessage{}},
		OAuthScopes: v2PersonScope,
		Scope:       scopeSession, SessionPath: "session_id",
		Replay:   replayRecovered(s, s.replayAgentMessage),
		Refusals: messageWriteRefusals(),
	}
})
