package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/comments/{id}",
	Auth:        authAny,
	Description: fmt.Sprintf("Edit a comment's body: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing. The body it replaces is kept and GET /api/v1/comments/{id}/history reads it. The dashboard editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v1/comments/{id}/blocks gave, so each block keeps its id; without them the ids follow the text. A comment of a closed issue is edited too, and a deleted one is 409 %s. Answers the comment as the edit left it, with previous {body}, the body the edit replaced, as the comment held it when the edit took its owner's lock. The command bar's Undo of an edit sends that body back here under an in-process condition it carries on the request's context, never in the body: the comment must still hold the body the edit left, decided under that lock, or the write back is refused 409 %s with nothing written; a write back carrying no condition is refused 500 %s.", codePostDeleted, codeCommentChanged, codeGoingConditionMissing),
	Handler:     (*server).editComment,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: commentEdited{}, Request: editCommentInput{}},
})
