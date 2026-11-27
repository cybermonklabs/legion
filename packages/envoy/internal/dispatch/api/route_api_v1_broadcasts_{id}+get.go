package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/broadcasts/{id}",
	Auth:        authAny,
	Description: "One broadcast with every recipient's delivery attempts and replies, recipients in the order the send named them; a broadcast from before that order was stored falls back to created_at, id.",
	Handler:     (*server).getBroadcast,
	Successor:   successorIs(http.MethodGet, "/broadcasts/{id}"),
	// The id names none of the five kinds a prefix shortens — an ask, a comment, a message, a
	// document, a document block — so no prefix is looked for and the handler answers for the
	// value as it always has. A whole uuid an {id} spells another of uuid.Parse's ways is still
	// rewritten to the canonical spelling, as it is on every route that takes an id
	// (canonicalizeUUIDPath).
	ID:          &idRoute{Reason: "a broadcast id: one send to many sessions"},
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: broadcastRead{}},
})
