package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/delivery/session-groups/{id}",
	Auth:        authAny,
	Description: "Rename a Delivery session group, add sessions to it and remove sessions from it {name?, add?, remove?}, with the create route's refusals; 400 SESSION_GROUP_INPUT for an edit that would leave it empty.",
	Handler:     (*server).updateDeliverySessionGroup,
	Successor:   successorIs(http.MethodPatch, "/session-groups/{id}"),
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a Delivery session group id"},
	Shape: routeShape{
		Status: http.StatusOK, Response: delivery.SessionGroup{}, Request: updateDeliverySessionGroupInput{},
	},
})
