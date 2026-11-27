package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/settings/credential-broker", Auth: authAny, Versions: onlyV2,
		Description: "What the credential pages may offer against this deployment's secrets broker, relayed from the " +
			"broker's own RFC 8414 metadata rather than from any Dispatch setting: {machine_code_page}, whether the " +
			"broker still serves the device grant, so whether a machine ever prints a code for a person to type; false " +
			"for a Dispatch with no broker. Needs the person scope.",
		Handler:  s.getCredentialCapabilities,
		Resource: "credential-broker", Kind: kindSingleton,
		Shape:       routeShape{Status: http.StatusOK, Response: credentialCapabilities{}},
		OAuthScopes: v2PersonScope,
		Refusals:    []apicode.Refusal{undeclaredKeyRefusal},
	}
})
