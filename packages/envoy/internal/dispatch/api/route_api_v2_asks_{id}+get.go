package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/asks/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Read one ask, as every list of asks answers it, with the block position of its anchor. " + askRowText +
			" Its thread's claimant is the session holding its issue's claim and route its issue's route, whom a person's " +
			"answer and replies also reach, and route_holder the live session that route reaches now: the session itself or " +
			"the role's holder; each is null when absent, route_holder also when nobody holds the route or the Envoy " +
			"listener did not answer, and all are null on a project document's ask. 404 ASK_NOT_FOUND for an id no ask has.",
		Handler:  s.getAskV2,
		Resource: "asks", Kind: kindItem, Key: "{id}",
		Query:       askReadV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: askV2{}},
		IDs:         v2AskIDs(),
		Authorities: []authorityRule{askMarkAuthority},
		Refusals:    []apicode.Refusal{undeclaredKeyRefusal, askMissingRefusal, actingSessionMismatchRefusal},
	}
})
