package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/messages/{id}/deliveries",
	Auth:        authAny,
	Description: "Retry delivering a targeted message.",
	Handler:     (*server).createDelivery,
	Successor:   successorIs(http.MethodPost, "/messages/{id}/deliveries"),
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "message", NotFound: apicode.MessageNotFound},
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.MessageDelivery{}, Request: createDeliveryInput{},
	},
})
