package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/me/signins/{id}", Auth: authAny, Versions: onlyV2,
		Description: "End one of the caller's own sign-ins: that machine trades no token for them again and every " +
			"access token already minted under it ends, while their sign-ins on other machines keep working. " +
			"Answers 200 {tokens_ended}, how many of the caller's live access tokens ended with it, 0 for a sign-in " +
			"revoked already. Needs the person and approve scopes; a credential scoped to one project is refused. " +
			"404 SIGNIN_NOT_FOUND for an id that is no sign-in of the caller's, anyone else's among them, and " +
			brokerRelayText + ".",
		Handler:  s.revokeSignInV2,
		Resource: "signins", Kind: kindItem, Key: "{id}",
		Shape:       routeShape{Status: http.StatusOK, Response: signInRevokedV2{}, Refusals: brokerRefusalAnswers(brokerRevokeSignInCall)},
		OAuthScopes: v2SettlesScope,
		Scope:       scopeDeployment,
		IDs:         &idRoute{Reason: "a sign-in id at the secrets broker"},
		Refusals:    brokerRefusals([]apicode.Refusal{undeclaredKeyRefusal}, brokerRevokeSignInCall),
	}
})
