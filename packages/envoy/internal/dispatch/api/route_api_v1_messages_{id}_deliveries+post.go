package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/messages/{id}/deliveries",
	Auth:        authAny,
	Description: "Retry delivering a targeted message.",
	Handler:     (*server).createDelivery,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "message", NotFound: "MESSAGE_NOT_FOUND"},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.MessageDelivery{}, Request: createDeliveryInput{},
	},
})
