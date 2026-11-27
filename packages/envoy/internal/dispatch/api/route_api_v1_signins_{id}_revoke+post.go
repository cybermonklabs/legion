package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/signins/{id}/revoke",
	Auth:        authAny,
	Description: "End one of the caller's own sign-ins: that machine trades no token for them again and every access token already minted under it ends, while their sign-ins on other machines keep working. A sign-in of anyone else is an unknown id.",
	Handler:     (*server).revokeSignIn,
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a sign-in id at the secrets broker"},
	OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
	ApproveOnly: true,
	Shape: routeShape{
		// POST /v1/person-signins/{id}/revoke (cipher.Client.RevokeSignIn), which says how
		// many access tokens ended with the sign-in.
		Status: http.StatusOK, Response: wire.PersonSignInRevokedResponse{},
	},
})
