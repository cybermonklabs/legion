package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/issues/{key}/comments", Auth: authAny, Versions: onlyV2,
		Description: "Comment on an issue, a quote of one of its documents, or an ask {body, anchor?, reply_to?, " +
			"reply_to_block?, quote?, occurrence?, ask_id?, mentions?, delivery?, turn?, suggestion?}, a bearer naming its session in actor: a " +
			"reply names the comment it answers in reply_to and one block of it in reply_to_block; with ask_id in place of " +
			"reply_to, reply_to_block names one part of that ask. mentions [{target}] (at most 10, role:<name> or " +
			"session:<id>) deliver the comment in delivery's mode (btw, aside or steer, the default), and a suggestion " +
			"{replace_with} proposes text for its anchored quote. A reply to a resolved thread reopens it, unless the root " +
			"is a decided suggestion. A closed issue takes a comment but refuses a suggestion, 409 ISSUE_CLOSED; a " +
			"reply to a deleted comment is 409 POST_DELETED, a withdrawn post taking no new text. Answers " +
			"201 the comment with ask_waiting_on and advice beside it; a repeat under its Idempotency-Key answers the " +
			"comment as it stands, its owed mentions sent again. 400 INVALID_COMMENT, MENTION_INPUT or TURN_REQUIRES_ASK " +
			"for a comment it refuses, 400 CAP_EXCEEDED past a bound, 409 ROUTE_CHANGED when the issue's route moves under " +
			"the write, 404 ISSUE_NOT_FOUND. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT"),
		Handler:  s.createIssueCommentV2,
		Resource: "comments", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: commentInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: commentWriteResponse{}, Advice: writeAdvice{}, Refusals: anchorRefusalAnswers()},
		Scope: scopeProject, ProjectOf: projectPathIssue,
		Replay:   replayRecovered(s, s.replayComment),
		Refusals: commentWriteRefusals(issueMissingRefusal, issueClosedRefusal, anchorDocumentMissingRefusal),
	}
})
