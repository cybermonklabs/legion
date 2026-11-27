package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/agents/{session_id}/messages",
	Auth:        authAny,
	Description: "Send an issue-less targeted message to a session: {body, delivery, in_reply_to?, connected_only?}, answered once its own delivery attempt has settled, on past a client that gives up. A session the Envoy listener knows and that is not running is a recipient: the listener stores the message and hands it over when that session runs again, and the attempt is recorded queued rather than sent. A session the listener knows nothing about (\"no live session <id>\") is queued too: Dispatch publishes the message on that session's own subject itself, and the attempt fails with \"no live session <id>\" only where Dispatch has no NATS to publish on or the publish fails. connected_only asks for a session that is running now and fails the attempt with \"the session is stopped\", nothing stored; every later attempt of the message asks what its first asked. A model or thinking switch the session would run as its own turn - a person's own Send or Aside, not one an agent wrote for them, whose whole body is one /model or /effort line, which would otherwise apply at an unknown later time - is held to a running session at every send whether or not the request set connected_only. Under an Idempotency-Key header (as every POST and PATCH takes one), a repeat with the same request answers 201 with the message the key made and its deliveries as they stand, Idempotency-Replayed: ?1, first sending the one attempt it still owes (none yet, or one stranded past its claim) and nothing else; a listener it cannot reach is recorded on the attempt, so a repeat is answered while the listener is down. An attempt a stopped process left owed is resumed by the server's delivery sweep whether anyone asks again or not.",
	Handler:     (*server).createAgentMessage,
	Replay:      func(s *server) keyedReplay { return replayRecovered(s, s.replayAgentMessage) },
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusCreated, Response: shapedMessage{}, Request: createAgentMessageInput{},
	},
})
