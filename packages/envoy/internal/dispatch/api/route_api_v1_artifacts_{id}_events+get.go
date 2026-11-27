package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/events",
	Auth:        authAny,
	Description: "Page a document's event log as an issue's is paged (after, before, ids, order, limit, exclude_types); after and before are positions in its event sequence from 0 to 2147483647 (the largest the store holds), and any other is 400 INVALID_QUERY. exclude_types is at most 50 comma-separated event types the page leaves out before limit counts it; a blank exclude_types leaves out none, and an unknown type or an empty item in the list is 400 INVALID_QUERY.",
	Handler:     (*server).listArtifactEvents,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
})
