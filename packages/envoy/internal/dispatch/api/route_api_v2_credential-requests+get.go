package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/credential-requests", Auth: authAny, Versions: onlyV2,
		Description: "The credential requests waiting on the caller, from the secrets broker: those still pending, newest " +
			"first, then the expired ones nobody has dismissed, newest first. They are those naming the caller or a " +
			"group that lists the caller, a shared secret's request that names the caller as the person responsible " +
			"for the agent that made it, names no such person, or names one who may not see it, and a service's " +
			"machine login, which anyone signed in decides (one an older broker opened naming a person is listed for " +
			"that person alone); each {record_id, kind, identifiers, requested_at, session, approve_url?, state?, " +
			"expires_at?, dispatch_scopes, reason}, a secret request's approve_url " +
			"the broker's own page where it is decided. The caller is the approver; no query names another. A " +
			"caller Dispatch calls a restricted member is no approver of a secret or of a service's machine login, " +
			"so their list holds their own machine logins alone; while the broker cannot ask Dispatch what kind of " +
			"member the caller is, it holds none of those other rows either, unless the broker kept the caller as a " +
			"full member within the hour. Needs the person scope. " +
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
