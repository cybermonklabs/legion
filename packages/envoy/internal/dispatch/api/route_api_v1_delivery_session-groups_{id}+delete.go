package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodDelete,
	Pattern:     "/api/v1/delivery/session-groups/{id}",
	Auth:        authAny,
	Description: "Delete a Delivery session group; its sessions show as themselves again.",
	Handler:     (*server).deleteDeliverySessionGroup,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a Delivery session group id"},
	Shape:       routeShape{Status: http.StatusNoContent, Request: actorInput{}, OptionalRequest: true},
})
