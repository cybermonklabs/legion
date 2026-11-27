package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/messages/{id}/blocks",
	Auth:        authAny,
	Description: "A message's current blocks in body order: {item, indexed, blocks: [{id, type, text, ref, from, to, embedding}]}, item the message's graph node, from and to UTF-16 code units into its stored body, ref absent for a direct message's block (it has no address), embedding where meaning search stands on the block (embedded, pending, or none while no window covers it). Read under GET /api/v1/messages/{id}'s rule: a credential without the person scope reading a direct message names its session in ?session= (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN). While the message is owed a block index, indexed is false and blocks empty; the read writes nothing.",
	Handler:     (*server).getMessageBlocks,
	Successor:   deletedFor(http.MethodGet, "/messages/{id}?include=blocks"),
	ID:          &idRoute{Kind: "message", NotFound: apicode.NotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.ItemBlocks{}},
})
