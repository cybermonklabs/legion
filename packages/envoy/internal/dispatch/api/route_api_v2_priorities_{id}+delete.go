package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/priorities/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Delete a priority, a bearer naming its session in actor: its state is written gone and no row is " +
			"removed, so the listing answers without it while its acts still name who ended it and when. A priority " +
			"already gone is 404 PRIORITY_NOT_FOUND, as is an id no priority carries; 204 otherwise. A credential scoped " +
			"to one project is refused.",
		Handler:  s.deletePriorityV2,
		Resource: "priorities", Kind: kindItem, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape:    routeShape{Status: http.StatusNoContent},
		Scope:    scopeDeployment,
		IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityMissingRefusal},
	}
})
