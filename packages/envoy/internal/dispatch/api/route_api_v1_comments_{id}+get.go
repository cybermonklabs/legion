package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/comments/{id}",
	Auth:        authAny,
	Description: "Read one comment with its thread.",
	Handler:     (*server).getComment,
	// The read answers writeHandlerError's NOT_FOUND for a comment id no row holds.
	ID:    &idRoute{Kind: "comment", NotFound: "NOT_FOUND"},
	Read:  readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route reads the comment and its thread as they stood at the cut. " + asOfDeletedCommentAtACut, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	Shape: routeShape{Status: http.StatusOK, Response: commentRead{}, Params: asOfQuery{}},
})
