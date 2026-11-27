package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/machine-logins", Auth: authAny, Versions: onlyV2,
		Description: "The machine logins the caller may revoke that can still reach a secret, from the secrets " +
			"broker, newest first: every service's login, whoever approved it, the caller's own machines' and the " +
			"caller's own pods', unexpired, or expired with a session still running, each {credential_id, host, " +
			"service, subject, person, approved_by, issued_at, expires_at, expired}. A person's pod login is the " +
			"login of a pod whose Kubernetes account the broker's BROKER_POD_OPERATORS maps to a person: only that " +
			"person's list holds it, and its person names that person; person is null on every other login. A " +
			"caller Dispatch calls a restricted member is listed their own machines' and pods' logins alone, and so " +
			"is any caller while the broker cannot ask Dispatch what kind of member the caller is. Needs the person " +
			"scope. " + brokerPageText + ". " + brokerRelayText + ".",
		Handler:  s.listMachineLoginsV2,
		Resource: "machine-logins", Kind: kindCollection, Key: "{id}",
		Query:       brokerListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[wire.LauncherCredentialResp]{}, Refusals: brokerRefusalAnswers(brokerMachineLoginsCall)},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(brokerRefusals(nil, brokerMachineLoginsCall)...),
	}
})
