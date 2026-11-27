package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Auth: authAny, Versions: onlyV2,
		Description: "Decide a machine login as the caller {state, code?}, or dismiss an expired credential " +
			"request {state}. state approved or denied decides a machine login, and code is the login's " +
			"confirmation code as the person typed it, which the secrets broker checks on both. The broker " +
			"decides whether the caller is its approver, refuses a secret request with 403 DECIDE_ON_BROKER_PAGE " +
			"naming its page on the broker in approve_url, and refuses a code that is missing or not the login's " +
			"own. A service's machine login is denied by anyone signed in but a caller Dispatch calls a restricted " +
			"member. A request " +
			"the broker already holds in that state, sent by a caller the broker would let decide it, is answered " +
			"as it stands: an approval of a service's machine login sent by anyone signed in, and a decision of " +
			"another sent by the person its approver names, relaying nothing. A deny of a service's machine login " +
			"is relayed, since the broker alone asks what kind of member its caller is, and where the code read the " +
			"login already denied, the broker's refusal of a decided login (below) is answered 200 that login as it " +
			"stands. Answers 200 the request as the broker " +
			"records it once decided, the record POST /api/v2/machine-login-lookups answers. state dismissed takes " +
			"an expired request, a secret request or a machine login whose deadline passed before anybody decided " +
			"it, off every list for good, the caller's and, for a shared request, everybody's, and answers 200 the " +
			"dismissal {record_id, dismissed_by, dismissed_at}: only a person whose list holds it may dismiss it, " +
			"and a caller Dispatch calls a restricted member dismisses only their own machine logins; a request " +
			"already dismissed is answered the dismissal the broker holds, and nothing is written. A caller the " +
			"broker does not let decide or dismiss a request is 403 NOT_APPROVER, saying why; a request that can " +
			"still be decided is 409 RECORD_PENDING to a dismissal, and one that was decided, or cancelled before " +
			"its deadline, is 409 RECORD_TERMINAL to a decision and a dismissal alike; and while the broker cannot " +
			"ask Dispatch what kind of member the caller is, a deny of a service's machine login, and a dismissal " +
			"of a secret request or of a service's machine login, is 503 MEMBER_KIND_UNCHECKABLE, writing nothing. " +
			"Whichever the state, a secret request " +
			"of a private project the caller may not see is 404 CREDENTIAL_REQUEST_NOT_FOUND, as is an id the broker " +
			"holds no request of, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch. Deciding " +
			"needs the person and approve scopes, dismissing the person scope; a credential scoped to one project is " +
			"refused. An approve-only token (the secrets broker's proxy for a person's approvals) may decide, and " +
			"may dismiss nothing: state dismissed is 403 APPROVE_ONLY_TOKEN. " + brokerRelayText + ".",
		Handler:  s.putCredentialRequestStateV2,
		Resource: "state", Kind: kindState,
		Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
			"approved": credentialRequestStateV2Input{}, "denied": credentialRequestStateV2Input{},
			"dismissed": credentialRequestDismissV2Input{},
		}},
		Shape: routeShape{
			Status: http.StatusOK, Response: wire.RecordResponse{},
			Also:     []routeAnswer{{Status: http.StatusOK, Body: wire.DismissResponse{}}},
			Refusals: brokerRefusalAnswers(brokerDecisionCall, brokerDismissalCall, brokerLookupCall.without(apicode.NoSuchCode)),
		},
		OAuthScopes: v2PersonScope,
		Authorities: []authorityRule{
			{Where: authorityWhere{In: partBody, Name: "state"}, Value: "approved", Scopes: v2SettlesScope},
			{Where: authorityWhere{In: partBody, Name: "state"}, Value: "denied", Scopes: v2SettlesScope},
			{Where: authorityWhere{In: partBody, Name: "state"}, Value: "dismissed", Scopes: v2PersonScope},
		},
		Scope:    scopeDeployment,
		IDs:      &idRoute{Reason: "a credential request id"},
		Refusals: brokerRefusals([]apicode.Refusal{undeclaredKeyRefusal}, brokerDecisionCall, brokerDismissalCall, brokerLookupCall.without(apicode.NoSuchCode)),
	}
})
