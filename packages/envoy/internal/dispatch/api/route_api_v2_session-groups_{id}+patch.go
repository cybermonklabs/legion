package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/session-groups/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, " +
			"remove?, actor?}, answering the group, with the create's refusals; 400 SESSION_GROUP_INPUT for a blank " +
			"name or an edit that would leave it empty. " + sessionGroupText + " 404 SESSION_GROUP_NOT_FOUND for an " +
			"id no group carries.",
		Handler:  s.updateSessionGroupV2,
		Resource: "session-groups", Kind: kindItem, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: updateDeliverySessionGroupInput{}},
		Shape:    routeShape{Status: http.StatusOK, Response: delivery.SessionGroup{}},
		Scope:    scopeDeployment,
		IDs:      v2SessionGroupIDs(),
		Refusals: sessionGroupWriteRefusals(undeclaredKeyRefusal, sessionGroupMissingRefusal),
	}
})
