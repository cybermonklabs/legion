package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/credential-grants",
	Auth:        authAny,
	Description: "List the live credential grants of the caller's Live grants page (?approver=me only): those the caller approved, those of the caller's own sessions, those the model judge approved in anyone's stead, and those a pod that acts for no person took without asking (a service's pod, or a person's pod the broker no longer maps), which nobody may revoke. A grant the caller approved, or whose session acts for the caller, is listed whatever project it touches; a person's pod the broker no longer maps to the caller acts for no one, so its other grants are listed to the caller naming no operator. Any other grant of a private project the caller may not see is left out, and one the broker cannot ask Dispatch about is left out while it cannot. A caller Dispatch calls a restricted member is listed the grants of the sessions that act for them alone; while the broker cannot ask Dispatch what kind of member the caller is, the list holds no other grant either.",
	Handler:     (*server).listCredentialGrants,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		// GET /v1/grants (cipher.Client.Grants).
		Status: http.StatusOK, Response: wire.ApproverGrantsResponse{}, Params: credentialApproverQuery{},
	},
})
