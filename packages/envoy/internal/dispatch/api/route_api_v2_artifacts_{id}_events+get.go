package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/artifacts/{id}/events", Auth: authAny, Versions: onlyV2,
		Description: "Page a project document's event log. " + ownerLogText + " 404 ARTIFACT_NOT_FOUND for a document that " +
			"does not exist; a document of an issue is 400 ARTIFACT_LINKED, its events being its issue's log, and a file of " +
			"an agent's conversation 400 ARTIFACT_AGENT_OWNED.",
		Handler:  s.listArtifactEventsV2,
		Resource: "events", Kind: kindLog, Position: seqPosition,
		Query:    ownerEventsV2Query{},
		List:     listBounds{Default: maxEventPage, Max: maxEventPage},
		Shape:    routeShape{Status: http.StatusOK, Response: page[model.Event]{}},
		IDs:      &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound},
		Refusals: listRefusals(projectDocumentRefusals(positionRefusal)...),
	}
})
