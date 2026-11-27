package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/credential-requests", Auth: authAny, Versions: onlyV2,
		Description: "The credential requests the caller may decide, from the secrets broker, newest first: those " +
			"naming the caller, and a shared secret's and a service's machine login, which anyone signed in decides, " +
			"each {record_id, kind, identifiers, requested_at, session, approve_url?}, a secret request's approve_url " +
			"the broker's own page where it is decided. The caller is the approver; no query names another. A " +
			"caller Dispatch calls a restricted member is no approver of a secret or of a service's machine login, " +
			"so their list holds their own machine logins alone; while the broker cannot ask Dispatch what kind of " +
			"member the caller is, it holds none of those other rows either. Needs the person scope. " +
			brokerPageText + ". " + brokerRelayText + ", so a client tells a Dispatch with no broker from one with " +
			"nothing waiting.",
		Handler:  s.listCredentialRequestsV2,
		Resource: "credential-requests", Kind: kindCollection, Key: "{id}",
		Query:       brokerListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[wire.PendingEntry]{}, Refusals: brokerRefusalAnswers(brokerPendingCall)},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(brokerRefusals(nil, brokerPendingCall)...),
	}
})
