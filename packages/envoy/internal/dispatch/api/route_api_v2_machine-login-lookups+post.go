package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/machine-login-lookups", Auth: authAny, Versions: onlyV2,
		Description: "Read a machine login by its typed confirmation code {code}, the only thing that selects one: " +
			"a direct link can never approve a machine login. Writes nothing. Answers 200 the request as the secrets " +
			"broker records it: {record_id, kind, state, approver, enrollment, session, identifiers, service, reason, " +
			"lifetime_seconds, rules_version, expires_at, requested_at, decided}. The broker answers a missing code " +
			"400 CODE_REQUIRED and a code no machine login has 404 NO_SUCH_CODE, relayed. Needs the person and " +
			"approve scopes; a credential scoped to one project is refused. " + brokerRelayText + ".",
		Handler:  s.lookupMachineLoginV2,
		Resource: "machine-login-lookups", Kind: kindComputation,
		Body:        requestBody{Kind: bodyJSON, Type: wire.MachineLookupRequest{}},
		Shape:       routeShape{Status: http.StatusOK, Response: wire.RecordResponse{}, Refusals: brokerRefusalAnswers(brokerLookupCall)},
		OAuthScopes: v2SettlesScope,
		Scope:       scopeDeployment,
		Refusals:    brokerRefusals([]apicode.Refusal{undeclaredKeyRefusal}, brokerLookupCall),
	}
})
