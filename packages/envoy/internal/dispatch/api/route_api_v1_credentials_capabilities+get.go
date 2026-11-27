package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/credentials/capabilities",
	Auth:        authAny,
	Description: "What the credential pages may offer against this deployment's secrets broker, relayed from the broker's own RFC 8414 metadata rather than from any Dispatch setting: machine_code_page says whether the broker still serves the device grant, so whether a machine ever prints a code for a person to type.",
	Handler:     (*server).getCredentialCapabilities,
	Successor:   successorIs(http.MethodGet, "/settings/credential-broker"),
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	// Not a relay: Dispatch reads one fact off the broker's metadata and answers its own shape.
	Shape: routeShape{Status: http.StatusOK, Response: credentialCapabilities{}},
})
