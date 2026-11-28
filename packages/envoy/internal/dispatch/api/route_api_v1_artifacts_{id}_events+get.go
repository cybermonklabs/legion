package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/artifacts/{id}/events",
	Auth:        authAny,
	Description: "Page a document's event log as an issue's is paged (after, before, ids, order, limit); after and before are positions in its event sequence from 0 to 2147483647 (the largest the store holds), and any other is 400 INVALID_QUERY.",
	Handler:     (*server).listArtifactEvents,
	ID:          &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND"},
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfAnswers, AtACut: "At a cut the route pages only the events written at or before the cut. " + asOfEventLogAtACut},
	Shape:       routeShape{Status: http.StatusOK, Response: []model.Event{}, Params: eventListQuery{}},
})
