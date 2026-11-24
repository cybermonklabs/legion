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
	Read:  readEntry{Writes: readWritesNothing, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	Shape: routeShape{Status: http.StatusOK, Response: commentRead{}},
})
