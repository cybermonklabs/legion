package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/credential-requests/{id}/deny",
	Auth:        authAny,
	Description: "Deny a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker. A secret request of a private project the caller may not see is 404, as an id no request has, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch. A service's machine login is denied by anyone signed in but a caller Dispatch calls a restricted member: 403 NOT_APPROVER, and 503 MEMBER_KIND_UNCHECKABLE while the broker cannot ask Dispatch what kind of member the caller is, denying nothing.",
	Handler:     (*server).denyCredentialRecord,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a credential request id"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		// POST /v1/credential-requests/{record}/deny (cipher.Client.Deny), which answers
		// the terminal state rather than a credential.
		Status: http.StatusOK, Response: wire.StateResponse{}, Request: credentialDecisionInput{}, OptionalRequest: true,
	},
})
