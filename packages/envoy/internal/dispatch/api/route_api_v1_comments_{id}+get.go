package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/comments/{id}",
	Auth:        authAny,
	Description: "Read one comment with its thread.",
	Handler:     (*server).getComment,
	Successor:   successorIs(http.MethodGet, "/comments/{id}"),
	// The read answers writeHandlerError's NOT_FOUND for a comment id no row holds.
	ID:    &idRoute{Kind: "comment", NotFound: apicode.NotFound},
	Read:  readEntry{Writes: readWritesNothing, WriteNote: "The comment's anchor block is read through the document service. " + docsCompactionNote},
	Shape: routeShape{Status: http.StatusOK, Response: commentRead{}},
})
