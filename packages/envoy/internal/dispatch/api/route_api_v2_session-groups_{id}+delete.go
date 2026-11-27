package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/session-groups/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Delete a Delivery session group {actor?}: the group and its memberships go, and its sessions " +
			"show as themselves again. 204. " + sessionGroupText + " 404 SESSION_GROUP_NOT_FOUND for an id no group " +
			"carries.",
		Handler:  s.deleteSessionGroupV2,
		Resource: "session-groups", Kind: kindItem, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: actorInput{}, Optional: true},
		Shape:    routeShape{Status: http.StatusNoContent},
		Scope:    scopeDeployment,
		IDs:      v2SessionGroupIDs(),
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, sessionGroupMissingRefusal},
	}
})
