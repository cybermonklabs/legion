package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/messages/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Edit an issue message's body {body, actor?}: its author alone (403 NOT_AUTHOR). A session names " +
			"itself in actor where its credential proves none; a person's own credential - a browser cookie, or a " +
			"personal token holding the person scope - is that person and names nothing, as the comment edit reads it. " +
			"The body it replaces is kept and GET /api/v2/messages/{id}/corrections reads it; the block ids follow the " +
			"text, since no editor carries them through the edit. A message of a closed issue is edited too. A message " +
			"of a conversation with no issue is 409 NOT_AN_ISSUE_MESSAGE, and a deleted message 409 POST_DELETED. " +
			"Answers the message and appends message.edited. 400 INVALID_MESSAGE for an empty body, 400 CAP_EXCEEDED " +
			"past a bound, 404 MESSAGE_NOT_FOUND.",
		Handler:  s.editMessageV2,
		Resource: "messages", Kind: kindItem, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: editMessageInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: model.Message{}},
		Scope: scopeProject, ProjectOf: projectPathMessage,
		IDs:    v2MessageIDs(),
		Reread: (*server).rereadMessage,
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageMissingRefusal, notAuthorRefusal, correctionOfDirectRefusal, messageInvalidRefusal,
			valuePastBoundRefusal, bodyDecisionBlockRefusal, postDeletedRefusal,
		},
	}
})
