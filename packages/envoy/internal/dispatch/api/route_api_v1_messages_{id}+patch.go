package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/messages/{id}",
	Auth:        authAny,
	Description: "Edit an issue message's body: its author alone (403 NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own credential - a browser cookie, or a personal token holding the person scope - is that person and names nothing, as the comment edit reads it. The body it replaces is kept and GET /api/v1/messages/{id}/history reads it; the block ids follow the text, since no editor carries them through the edit. A message of a closed issue is edited too. A message of a conversation with no issue is 409 NOT_AN_ISSUE_MESSAGE, and a deleted message 409 POST_DELETED.",
	Handler:     (*server).editMessage,
	Successor:   successorIs(http.MethodPatch, "/messages/{id}"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "message", NotFound: apicode.NotFound},
	Shape:       routeShape{Status: http.StatusOK, Response: model.Message{}, Request: editMessageInput{}},
})
