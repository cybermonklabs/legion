package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/messages/{id}/history",
	Auth:        authAny,
	Description: fmt.Sprintf("Every body an edit or a delete of this message replaced, newest first, each with the actor that replaced it and the time: {deleted_at, deleted_by, entries: [{body, edited_by, edited_at}]}. Read under GET /api/v1/messages/{id}'s rule: a credential without the person scope reading a direct message names its session in ?session= (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN). At most %d rows in one answer; ?before=<RFC3339 time> answers the page after, the rows written before it. A message nobody edited or deleted answers an empty list; a delete writes an entry of its own, so a message deleted with no edit lists one, and a deleted message answers the mark with every body empty, since no read of a deleted post gives its text.", contracts.PostHistoryPageLimit),
	Handler:     (*server).getMessageHistory,
	Successor:   successorIs(http.MethodGet, "/messages/{id}/corrections"),
	ID:          &idRoute{Kind: "message", NotFound: apicode.NotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: postHistoryRead{}, Params: postHistoryQuery{},
	},
})
