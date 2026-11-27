package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/settings/delivery", Auth: authAny,
		Description: "Set the delivery timeline's configuration after proving the GitHub App can read the deploy repository; 409 DELIVERY_SETTINGS_ACCESS a credential or repository problem.",
		Handler:     s.putDeliverySettings,
		Body:        requestBody{Kind: bodyJSON, Type: deliverySettingsInput{}},
		Shape:       routeShape{Status: http.StatusOK, Response: delivery.DeliverySettings{}},
		// A deployment-wide setting, which a credential scoped to one project may not change.
		Scope:      scopeDeployment,
		OfINFRA979: true,
	}
})
