package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/artifacts/{id}/comments", Auth: authAny, Versions: onlyV2,
		Description: "Comment on a project document or one of its quotes {body, anchor?, reply_to?, reply_to_block?, " +
			"quote?, occurrence?, ask_id?, mentions?, delivery?, turn?, suggestion?}, a bearer naming its session in actor: a reply names the " +
			"comment it answers in reply_to, and a suggestion {replace_with} proposes text for its anchored quote. Answers " +
			"201 the comment with ask_waiting_on and advice beside it; a repeat under its Idempotency-Key answers the " +
			"comment as it stands, its owed mentions sent again. 400 INVALID_COMMENT for an empty body, 400 CAP_EXCEEDED " +
			"past a bound, 400 ARTIFACT_LINKED for an issue's document, 400 ARTIFACT_AGENT_OWNED for a conversation's file, " +
			"404 ARTIFACT_NOT_FOUND. " + replyQuoteDescription("reply_to_block", "INVALID_COMMENT"),
		Handler:  s.createArtifactCommentV2,
		Resource: "comments", Kind: kindCollection, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: commentInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: commentWriteResponse{}, Advice: writeAdvice{}, Refusals: anchorRefusalAnswers()},
		Scope: scopeProject, ProjectOf: projectPathArtifact,
		IDs:      v2DocumentIDs(),
		Replay:   replayRecovered(s, s.replayComment),
		Refusals: commentWriteRefusals(projectDocumentRefusals()...),
	}
})
