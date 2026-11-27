package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/comments/{id}/blocks",
	Auth:        authAny,
	Description: "A comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at, embedding}]}, item the comment's graph node, revision its indexed revision, from and to UTF-16 code units into its stored body, changed_revision and changed_at the revision its block's text last changed in, embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). While the comment is owed a block index, or was indexed from another body than it stores, indexed is false and blocks empty; the read writes nothing. An id written short - 8 characters or more of the hex digits and hyphens an id starts with - names the one comment that carries it, is 409 ID_AMBIGUOUS where two or more do, and is 404 NOT_FOUND where none does; any other non-uuid id is 400 COMMENT_ID_INPUT.",
	Handler:     (*server).getCommentBlocks,
	Successor:   deletedFor(http.MethodGet, "/comments/{id}?include=blocks"),
	ID:          &idRoute{Kind: "comment", NotFound: apicode.NotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.ItemBlocks{}},
})
