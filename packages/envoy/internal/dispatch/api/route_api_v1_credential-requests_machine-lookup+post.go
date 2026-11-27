package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/credential-requests/machine-lookup",
	Auth:        authAny,
	Description: "Resolve a pending machine login by its typed confirmation code, returning its facts.",
	Handler:     (*server).lookupMachineCredential,
	WriteClass:  scopeDeployment,
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		// POST /v1/machine-logins/lookup (cipher.Client.MachineLookup). Dispatch forwards
		// the body to the broker unparsed (readRelayBody) rather than reading it, but the broker
		// does read it, so the route takes the shape the broker declares.
		Status: http.StatusOK, Response: wire.RecordResponse{}, Request: wire.MachineLookupRequest{},
	},
})
