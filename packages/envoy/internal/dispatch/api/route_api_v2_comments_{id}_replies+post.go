package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/comments/{id}/replies", Auth: authBearer, Versions: onlyV2,
		Description: "The mentioned session's answer to one delivery attempt of a comment {actor, attempt, target?, " +
			"body | error, reply_to_block?, quote?, occurrence?}, under a bearer credential: the session names itself in actor, and the " +
			"attempt, of the mention target names (required where the comment has several mentions, 400 " +
			"MENTION_INPUT), must be that session's own; a cookie caller, an attempt that is not that session's own and " +
			"a body naming neither or both of body and error are 403 REPLY_FORBIDDEN. For a mention of a role " +
			"(role:<name>), the role's live holder, as the Envoy listener names it, may answer the attempt too; for a " +
			"mention of a session (session:<id>), so may a session the listener names as having replaced the " +
			"attempt's session, which it handed that session's unread messages. A listener that does not answer is " +
			"503 ENVOY_UNAVAILABLE, and a failed read of the mention is 500, never a " +
			"refusal. A body is stored as a reply in " +
			"the comment's thread, its reply_to_block naming one block of the comment it answers, and marks the attempt " +
			"sent; answers 201 the reply with ask_waiting_on, and appends comment.answered. An attempt already answered " +
			"posts nothing and answers 200 the stored reply. An error marks the attempt failed and answers 200 the " +
			"attempt. 404 COMMENT_NOT_FOUND for an id no comment has, or an attempt the mention does not have. " +
			replyQuoteDescription("reply_to_block", "INVALID_COMMENT"),
		Handler:  s.replyCommentV2,
		Resource: "replies", Kind: kindCollection, Key: "{id}",
		Body: requestBody{Kind: bodyJSON, Type: commentReplyInput{}},
		Shape: routeShape{
			Status: http.StatusCreated, Response: commentWriteResponse{},
			Also: []routeAnswer{
				{Status: http.StatusOK, Body: model.Comment{}},
				{Status: http.StatusOK, Body: model.CommentDelivery{}},
			},
		},
		Scope: scopeProject, ProjectOf: projectPathComment,
		IDs:    v2CommentIDs(),
		Reread: (*server).rereadCommentReply,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, commentMissingRefusal, replyNotRecipientRefusal, envoyUnreachableRefusal,
			mentionInvalidRefusal, commentInvalidRefusal, valuePastBoundRefusal, bodyDecisionBlockRefusal, postDeletedRefusal,
			targetMissingRefusal, quoteAmbiguousRefusal,
		},
	}
})
