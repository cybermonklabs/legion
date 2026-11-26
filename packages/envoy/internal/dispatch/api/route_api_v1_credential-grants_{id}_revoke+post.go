package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/credential-grants/{id}/revoke",
	Auth:        authAny,
	Description: "Revoke a credential grant as the caller: its approver, or the person its enrollment acts for, ends it whatever project it touches. A grant revoked by someone it does not name is 404 when it belongs to a private project the caller may not see, as an id no grant has, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch; of the grants they may see, they end one whose approver is anyone, and any other is 403 NOT_APPROVER. A grant the broker saw move to another box each time it read it, three times in a row, is 409 GRANT_MOVING, revoking nothing; the same revoke succeeds once it stays put. A caller Dispatch calls a restricted member ends a grant only as the person its enrollment acts for: any other is 403 NOT_APPROVER, saying why. While Dispatch does not answer the broker what kind of member the caller is (a timeout, a failed connection, a gateway's 502, 503 or 504, Dispatch's own DATABASE_UNAVAILABLE or OIDC_ISSUER_UNAVAILABLE, or a 500 naming no code or INTERNAL, which Dispatch answers while its database fails), only the person the grant names as its approver ends it as an approver: every revoke through a group or as anyone then, whatever session holds the grant, and every revoke while Dispatch answers anything else the broker cannot read (a refused account, a kind the broker does not know, another code, a certificate or a name that does not resolve), is 503 MEMBER_KIND_UNCHECKABLE, revoking nothing. A grant on a person's session can still be ended then by that person, who asks Dispatch nothing; a grant on a session that acts for no person (a service's pod, a person's pod the broker no longer maps, or a machine's own session) is ended through the broker by nobody but its named approver until Dispatch answers.",
	Handler:     (*server).revokeCredentialGrant,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a credential grant id"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		// POST /v1/grants/{id}/revoke-by-approver (cipher.Client.RevokeByApprover).
		Status: http.StatusOK, Response: wire.StateResponse{},
	},
})
