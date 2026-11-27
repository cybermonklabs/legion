package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/events",
	Auth:        authAny,
	Description: "Page a document's event log as an issue's is paged (after, before, ids, order, limit); after and before are positions in its event sequence from 0 to 2147483647 (the largest the store holds), and any other is 400 INVALID_QUERY.",
	Handler:     (*server).listArtifactEvents,
	Successor:   successorIs(http.MethodGet, "/artifacts/{id}/events"),
	ID:          &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
})
