package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/comments/{id}",
	Auth:        authAny,
	Description: "Delete a comment: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing. The comment keeps its row and its place in its thread, and every read of it gives deleted_at and deleted_by and no body; the body it held is kept, and GET /api/v1/comments/{id}/history reads the mark in its place. A suggestion of it still pending is rejected out of the document before the body goes, so an insert suggestion's text leaves the document with it; the suggestion's replacement text goes from the comment with the body, and the document version an accepted suggestion made, which the accept named after the comment's body, is renamed to the change it made (its artifact.version event keeps the name it carried); each block is removed with its excerpt cleared and the text every revision recorded of it cleared too, the margin mark is projected again with no text, and every mention attempt still pending is failed, 'ended by the post's delete', which says nothing of whether its frame had already reached the session. A mention attempt its own sender moves while the delete is being written is 409 DELETE_CONTENDED with Retry-After: 1 and nothing written: send the same delete again. A comment of a closed issue is deleted too; a second delete is 409 POST_DELETED.",
	Handler:     (*server).deleteComment,
	Successor:   successorIs(http.MethodDelete, "/comments/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: apicode.NotFound},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Comment{}, Request: actorInput{}, OptionalRequest: true,
	},
})
