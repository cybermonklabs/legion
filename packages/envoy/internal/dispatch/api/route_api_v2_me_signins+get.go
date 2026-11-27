package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/signins", Auth: authAny, Versions: onlyV2,
		Description: "The caller's own sign-ins on their machines (`agent-secrets login`), from the secrets broker, " +
			"newest first: one per box key they have not revoked, each {signin_id, machine, thumbprint, created_at, " +
			"last_used_at}, naming the machine's account, the key's thumbprint, when the person signed in there and " +
			"when that machine last used it. Nobody lists another person's: Dispatch names the person from its own " +
			"session. Needs the person scope. " + brokerPageText + ". " + brokerRelayText + ".",
		Handler:  s.listSignInsV2,
		Resource: "signins", Kind: kindCollection, Key: "{id}",
		Query:       brokerListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[wire.PersonSignInResp]{}, Refusals: brokerRefusalAnswers(brokerSignInsCall)},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(brokerRefusals(nil, brokerSignInsCall)...),
	}
})
