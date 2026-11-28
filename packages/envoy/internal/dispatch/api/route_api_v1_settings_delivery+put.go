package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPut,
	Pattern:     "/api/v1/settings/delivery",
	Auth:        authAny,
	Description: "Set the delivery timeline's one configuration record of the release before this one after proving the GitHub App can read the deploy repository; 409 DELIVERY_SETTINGS_ACCESS a credential or repository problem. This release reads it nowhere else: each project's settings, PUT /api/v1/projects/{key}/settings/delivery, replace it, and the next release removes this route.",
	Handler:     (*server).putDeliverySettings,
	WriteClass:  scopeDeployment,
	Shape: routeShape{
		Status: http.StatusOK, Response: delivery.DeliverySettings{}, Request: deliverySettingsInput{},
	},
})
