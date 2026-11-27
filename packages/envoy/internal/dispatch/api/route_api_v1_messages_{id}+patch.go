package api

import (
	"fmt"
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/messages/{id}",
	Auth:        authAny,
	Description: fmt.Sprintf("Edit an issue message's body: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing, as the comment edit reads it. The body it replaces is kept and GET /api/v1/messages/{id}/history reads it; the block ids follow the text, since no editor carries them through the edit. A message of a closed issue is edited too. A message of a conversation with no issue is 409 NOT_AN_ISSUE_MESSAGE, and a deleted message 409 %s. Answers the message as the edit left it, with previous {body}, the body the edit replaced, as the message held it when the edit took its issue's lock. The command bar's Undo of an edit sends that body back here under an in-process condition it carries on the request's context, never in the body: the message must still hold the body the edit left, decided under that lock, or the write back is refused 409 %s with nothing written; a write back carrying no condition is refused 500 %s.", codePostDeleted, codeMessageChanged, codeGoingConditionMissing),
	Handler:     (*server).editMessage,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "message", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: messageEdited{}, Request: editMessageInput{}},
})
