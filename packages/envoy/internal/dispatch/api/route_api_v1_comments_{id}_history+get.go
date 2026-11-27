package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/comments/{id}/history",
	Auth:        authAny,
	Description: fmt.Sprintf("Every body an edit or a delete of this comment replaced, newest first, each with the actor that replaced it and the time: {deleted_at, deleted_by, entries: [{body, edited_by, edited_at}]}. Read by whoever reads the comment, which is any authenticated caller. At most %d rows in one answer; ?before=<RFC3339 time> answers the page after, the rows written before it. A comment nobody edited or deleted answers an empty list; a delete writes an entry of its own, so a comment deleted with no edit lists one, and a deleted comment answers the mark with every body empty, since no read of a deleted post gives its text.", contracts.PostHistoryPageLimit),
	Handler:     (*server).getCommentHistory,
	ID:          &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: asOfPostHistoryAtACut("comment")},
	Shape:       routeShape{Status: http.StatusOK, Response: postHistoryRead{}, Params: postHistoryQuery{}},
})
