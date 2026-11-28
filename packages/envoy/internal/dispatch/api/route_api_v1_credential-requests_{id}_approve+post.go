package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:       http.MethodPost,
	Pattern:      "/api/v1/credential-requests/{id}/approve",
	Auth:         authAny,
	Description:  "Approve a machine login as the caller with its typed code; the broker decides whether the caller is its approver, and refuses a secret request with 403 DECIDE_ON_BROKER_PAGE naming its page on the secrets broker. A secret request of a private project the caller may not see is 404, as an id no request has, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch.",
	Handler:      (*server).approveCredentialRecord,
	Successor:    successorIs(http.MethodPut, "/credential-requests/{id}/state"),
	WriteClass:   scopeDeployment,
	ID:           &idRoute{Reason: "a credential request id"},
	OAuthScopes:  routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly:  true,
	ApproveState: "approved",
	Shape: routeShape{
		// POST /v1/credential-requests/{record}/approve (cipher.Client.Approve).
		Status: http.StatusOK, Response: wire.DecisionResponse{}, Request: credentialDecisionInput{}, OptionalRequest: true,
	},
})
