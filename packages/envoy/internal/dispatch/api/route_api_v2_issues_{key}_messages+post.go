package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/messages", Auth: authAny, Versions: onlyV2,
		Description: "Post a message on an issue {body, in_reply_to?, in_reply_to_block?, quote?, occurrence?, target?, delivery?, " +
			"urgency?}, " +
			"a bearer naming its session in actor: a reply names the message of this issue it answers in in_reply_to and " +
			"one block of it in in_reply_to_block; target (role:<name> or session:<id>) with delivery (btw, aside or " +
			"steer) delivers it, and a person's reply that names no target is delivered as its thread was; urgency is " +
			"low, med, high or blocking. A closed issue takes it; a reply to a deleted message is 409 POST_DELETED, a " +
			"withdrawn post taking no new text. Answers 201 the message once a targeted one's first " +
			"attempt has settled, with advice beside it; a repeat under its Idempotency-Key answers the message as it " +
			"stands, its owed attempt sent first. 400 INVALID_MESSAGE or MESSAGE_INPUT for a message it refuses, 404 " +
			"ISSUE_NOT_FOUND. " + replyQuoteDescription("in_reply_to_block", "INVALID_MESSAGE"),
		Handler:  s.createIssueMessageV2,
		Resource: "messages", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: createMessageInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: shapedMessage{}, Advice: writeAdvice{}},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Replay:   replayRecovered(s, s.replayIssueMessage),
		Refusals: messageWriteRefusals(issueMissingRefusal),
	}
})
