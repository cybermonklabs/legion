package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/session-groups", Auth: authAny, Versions: onlyV2,
		Description: "Create a Delivery session group {name, sessions, actor?}, answering 201 the group {id, name, " +
			"sessions, created_by, created_at, updated_by, updated_at}. " + sessionGroupText + " A blank name, no " +
			"session, or a blank session id is 400 SESSION_GROUP_INPUT; a session another group holds is 409 " +
			"SESSION_IN_GROUP, a name another group has 409 SESSION_GROUP_NAME_TAKEN, and a name that is the title of " +
			"a session outside the group 409 SESSION_GROUP_NAME_IS_SESSION_TITLE.",
		Handler:  s.createSessionGroupV2,
		Resource: "session-groups", Kind: kindCollection, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: createDeliverySessionGroupInput{}},
		Shape:    routeShape{Status: http.StatusCreated, Response: delivery.SessionGroup{}},
		Scope:    scopeDeployment,
		Refusals: sessionGroupWriteRefusals(undeclaredKeyRefusal),
	}
})
