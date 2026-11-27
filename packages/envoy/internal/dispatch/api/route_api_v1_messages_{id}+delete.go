package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/messages/{id}",
	Auth:        authAny,
	Description: "Delete an issue message: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing. The message keeps its row and its place in its thread, and every read of it gives deleted_at and deleted_by and no body; the body it held is kept, and GET /api/v1/messages/{id}/history reads the mark in its place. Each block is removed with its excerpt cleared, and every send still pending is failed, 'ended by the post's delete', which says nothing of whether its frame had already reached the session. A send its own sender moves while the delete is being written is 409 DELETE_CONTENDED with Retry-After: 1 and nothing written: send the same delete again. A message of a closed issue is deleted too. A message of a conversation with no issue is 409 NOT_AN_ISSUE_MESSAGE, and a second delete is 409 POST_DELETED.",
	Handler:     (*server).deleteMessage,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "message", NotFound: "NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Message{}, Request: actorInput{}, OptionalRequest: true,
	},
})
