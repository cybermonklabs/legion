package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodDelete, Pattern: "/machine-logins/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods " +
			"and a person's pod included: anyone signed in revokes a service's login, only the person who approved " +
			"a person's machine login revokes that one, and only the person a person's pod login names (its " +
			"person: the broker's BROKER_POD_OPERATORS maps the pod's Kubernetes account to that person) revokes " +
			"that one, as the secrets broker decides. The revoke does not keep that pod out: it logs in again with " +
			"its token under a new login while the broker's BROKER_SERVICE_LOGINS names its account, and that " +
			"login is the person's while BROKER_POD_OPERATORS still maps the account to that person. A caller " +
			"Dispatch calls a restricted member revokes their own logins alone: a service's login is answered them " +
			"as an id no login has, and so it is to any caller while the broker cannot ask Dispatch what kind of " +
			"member the caller is, who still revokes their own. 204. Needs the person and approve scopes; a " +
			"credential scoped to one project is refused. 404 MACHINE_LOGIN_NOT_FOUND for an id the broker holds " +
			"no machine login of, and " + brokerRelayText + ".",
		Handler:  s.revokeMachineLoginV2,
		Resource: "machine-logins", Kind: kindItem, Key: "{id}",
		Shape:       routeShape{Status: http.StatusNoContent, Refusals: brokerRefusalAnswers(brokerRevokeMachineLoginCall)},
		OAuthScopes: v2SettlesScope,
		Scope:       scopeDeployment,
		IDs:         &idRoute{Reason: "a machine login id"},
		Refusals:    brokerRefusals([]apicode.Refusal{undeclaredKeyRefusal}, brokerRevokeMachineLoginCall),
	}
})
