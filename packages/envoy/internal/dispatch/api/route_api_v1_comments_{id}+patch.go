package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/comments/{id}",
	Auth:        authAny,
	Description: "Edit a comment's body: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing. The body it replaces is kept and GET /api/v1/comments/{id}/history reads it. The dashboard editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v1/comments/{id}/blocks gave, so each block keeps its id; without them the ids follow the text. A comment of a closed issue is edited too, and a deleted one is 409 POST_DELETED.",
	Handler:     (*server).editComment,
	Successor:   successorIs(http.MethodPatch, "/comments/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: apicode.NotFound},
	Shape:       routeShape{Status: http.StatusOK, Response: shapedComment{}, Request: editCommentInput{}},
})
