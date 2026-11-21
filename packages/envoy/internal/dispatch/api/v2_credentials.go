package api

// v2_credentials.go is the credential pages' routes of /api/v2 (R1 of dispatch://INFRA-4071/spec):
// the credential requests waiting on the caller and the state route their
// approve and deny become, the machine-login lookup, the grants and the machine logins the caller
// may revoke, and the caller's own sign-ins, each a relay to the secrets broker as its /api/v1
// route is (credential_requests.go). Three things change. A broker list, which the broker does
// not page, is sliced by Dispatch into the envelope. A revoke is a DELETE (the route map's J5),
// answering 204, or 200 {tokens_ended} for a sign-in. And an id the broker holds no record of
// answers the broker's own 404 under one code of its object (R7): every other answer of
// the broker, a refusal among them, is relayed verbatim, as /api/v1 relays it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The refusals of the broker's ids a credential route's path names (R7), and of every
// credential route on a Dispatch with no secrets broker (requireAgentSecrets).
var (
	credentialRequestMissingRefusal = apicode.Refusal{Condition: apicode.CredentialRequestMissing, Code: apicode.CredentialRequestNotFound}
	credentialGrantMissingRefusal   = apicode.Refusal{Condition: apicode.CredentialGrantMissing, Code: apicode.CredentialGrantNotFound}
	machineLoginMissingRefusal      = apicode.Refusal{Condition: apicode.MachineLoginMissing, Code: apicode.MachineLoginNotFound}
	signinMissingRefusal            = apicode.Refusal{Condition: apicode.SigninMissing, Code: apicode.SigninNotFound}
	brokerUnconfiguredRefusal       = apicode.Refusal{Condition: apicode.BrokerUnconfigured, Code: apicode.FeatureOff}
)

// brokerRelayText is what every credential route says of the broker's answers.
const brokerRelayText = "404 FEATURE_OFF without a configured secrets broker, 503 AGENT_SECRETS_UNAVAILABLE when it " +
	"cannot be reached, 500 AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE when Dispatch cannot read its own ServiceAccount " +
	"token for it, and every other refusal the broker answers relayed with its own status and code"

// brokerPageText is what every credential list says of its page: the broker does not page, so
// Dispatch slices what it answers.
const brokerPageText = "limit 1 to 200 (default 50) and offset over what the broker answers, which Dispatch slices; " +
	"{items, total, limit, offset}"

// v2CredentialRows are the credential routes: rows 57, 58, 59, 61 and
// 62, 60, 117, 118, 186 and 187 of the route map.
func (s *server) v2CredentialRows() []routeRow {
	person := routeScopeSet{model.ScopePerson}
	settles := routeScopeSet{model.ScopePerson, model.ScopeApprove}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/credential-grants", Auth: authAny, Versions: onlyV2,
			Description: "The live credential grants of the caller's Live grants page, from the secrets broker, newest " +
				"first: those the caller approved, those of the caller's own sessions, those the model judge approved in " +
				"anyone's stead, and those a pod that acts for no person took without asking (a service's pod, or a " +
				"person's pod the broker no longer maps), which nobody may revoke; each {grant_id, granted, record_id, " +
				"enrollment, names, approver, decided_by, expires_at, created_at}. A grant the caller approved, or whose " +
				"session acts for the caller, is listed whatever project it touches; a person's pod the broker no longer " +
				"maps to the caller acts for no one, so its other grants are listed to the caller naming no operator. Any " +
				"other grant of a private project the caller may not see is left out, and one the broker cannot ask " +
				"Dispatch about is left out while it cannot. Dispatch names the caller from its own session, so nobody " +
				"lists another person's. Needs the person scope. " + brokerPageText + ". " + brokerRelayText + ".",
			Handler:  s.listCredentialGrantsV2,
			Resource: "credential-grants", Kind: kindCollection, Key: "{id}",
			Query:       brokerListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[wire.ApproverGrantResp]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(brokerUnconfiguredRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/credential-grants/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Revoke a credential grant as the caller, as the secrets broker decides: its approver, or the " +
				"person its enrollment acts for, ends it whatever project it touches, and a revoke has no way back. 204. " +
				"A grant revoked by someone it does not name is 404 CREDENTIAL_GRANT_NOT_FOUND when it belongs to a " +
				"private project the caller may not see, as an id the broker holds no grant of, and 503 " +
				"PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch; of the grants they may see, they end one " +
				"whose approver is anyone, and any other is 403 NOT_APPROVER. A grant the broker saw move to another box " +
				"each time it read it, three times in a row, is 409 GRANT_MOVING, revoking nothing; the same revoke " +
				"succeeds once it stays put. Needs the person and approve scopes; a credential scoped to one project is " +
				"refused. 404 CREDENTIAL_GRANT_NOT_FOUND for an id the broker holds no grant of, and " + brokerRelayText + ".",
			Handler:  s.revokeCredentialGrantV2,
			Resource: "credential-grants", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: settles,
			Scope:       scopeDeployment,
			IDs:         &idRoute{Reason: "a credential grant id"},
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, credentialGrantMissingRefusal, brokerUnconfiguredRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/credential-requests", Auth: authAny, Versions: onlyV2,
			Description: "The credential requests the caller may decide, from the secrets broker, newest first: those " +
				"naming the caller, and a shared secret's and a service's machine login, which anyone signed in decides, " +
				"each {record_id, kind, identifiers, requested_at, session, approve_url?}, a secret request's approve_url " +
				"the broker's own page where it is decided. The caller is the approver; no query names another. Needs the " +
				"person scope. " + brokerPageText + ". " + brokerRelayText + ", so a client tells a Dispatch with no " +
				"broker from one with nothing waiting.",
			Handler:  s.listCredentialRequestsV2,
			Resource: "credential-requests", Kind: kindCollection, Key: "{id}",
			Query:       brokerListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[wire.PendingEntry]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(brokerUnconfiguredRefusal),
		},
		{
			Method: http.MethodPut, Pattern: "/credential-requests/{id}/state", Auth: authAny, Versions: onlyV2,
			Description: "Decide a machine login as the caller {state, code?}, or dismiss an expired credential " +
				"request {state}. state approved or denied decides a machine login, and code is the login's " +
				"confirmation code as the person typed it, which the secrets broker checks on both. The broker " +
				"decides whether the caller is its approver, refuses a secret request with 403 DECIDE_ON_BROKER_PAGE " +
				"naming its page on the broker, and refuses a code that is missing or not the login's own. A request " +
				"the broker already holds in that state, sent by a caller the broker would let decide it (anyone signed " +
				"in, for a service's machine login; the person its approver names, for any other), is answered as it " +
				"stands and nothing is relayed. Answers 200 the request as the broker " +
				"records it once decided, the record POST /api/v2/machine-login-lookups answers. state dismissed takes " +
				"an expired request, a secret request or a machine login whose deadline passed before anybody decided " +
				"it, off every list for good, the caller's and, for a shared request, everybody's, and answers 200 the " +
				"dismissal {record_id, dismissed_by, dismissed_at}: only a person whose list holds it may dismiss it " +
				"(403 NOT_APPROVER), a request that can still be decided is 409 RECORD_PENDING, and one that was " +
				"decided, or cancelled before its deadline, is 409 RECORD_TERMINAL; a request already dismissed is " +
				"answered the dismissal the broker holds, and nothing is written. Whichever the state, a secret request " +
				"of a private project the caller may not see is 404 CREDENTIAL_REQUEST_NOT_FOUND, as an id the broker " +
				"holds no request of, and 503 PROJECT_ACCESS_UNAVAILABLE while the broker cannot ask Dispatch. Deciding " +
				"needs the person and approve scopes, dismissing the person scope; a credential scoped to one project is " +
				"refused. 404 CREDENTIAL_REQUEST_NOT_FOUND for an id the broker holds no request of, and " +
				brokerRelayText + ".",
			Handler:  s.putCredentialRequestStateV2,
			Resource: "state", Kind: kindState,
			Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
				"approved": credentialRequestStateV2Input{}, "denied": credentialRequestStateV2Input{},
				"dismissed": credentialRequestDismissV2Input{},
			}},
			Shape: routeShape{
				Status: http.StatusOK, Response: wire.RecordResponse{},
				Also: []routeAnswer{{Status: http.StatusOK, Body: wire.DismissResponse{}}},
			},
			OAuthScopes: person,
			Authorities: []authorityRule{
				{Where: authorityWhere{In: partBody, Name: "state"}, Value: "approved", Scopes: settles},
				{Where: authorityWhere{In: partBody, Name: "state"}, Value: "denied", Scopes: settles},
				{Where: authorityWhere{In: partBody, Name: "state"}, Value: "dismissed", Scopes: person},
			},
			Scope:    scopeDeployment,
			IDs:      &idRoute{Reason: "a credential request id"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, credentialRequestMissingRefusal, brokerUnconfiguredRefusal},
		},
		{
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
			Shape:       routeShape{Status: http.StatusOK, Response: wire.RecordResponse{}},
			OAuthScopes: settles,
			Scope:       scopeDeployment,
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, brokerUnconfiguredRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/machine-logins", Auth: authAny, Versions: onlyV2,
			Description: "The machine logins the caller may revoke that can still reach a secret, from the secrets " +
				"broker, newest first: every service's login, whoever approved it, the caller's own machines' and the " +
				"caller's own pods', unexpired, or expired with a session still running, each {credential_id, host, " +
				"service, subject, person, approved_by, issued_at, expires_at, expired}. A person's pod login is the " +
				"login of a pod whose Kubernetes account the broker's BROKER_POD_OPERATORS maps to a person: only that " +
				"person's list holds it, and its person names that person; person is null on every other login. " +
				"Needs the person scope. " + brokerPageText + ". " + brokerRelayText + ".",
			Handler:  s.listMachineLoginsV2,
			Resource: "machine-logins", Kind: kindCollection, Key: "{id}",
			Query:       brokerListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[wire.LauncherCredentialResp]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(brokerUnconfiguredRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/machine-logins/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Revoke a machine login, expired or not, ending every session it enrolled, a service's pods " +
				"and a person's pod included: anyone signed in revokes a service's login, only the person who approved " +
				"a person's machine login revokes that one, and only the person a person's pod login names (its " +
				"person: the broker's BROKER_POD_OPERATORS maps the pod's Kubernetes account to that person) revokes " +
				"that one, as the secrets broker decides. The revoke does not keep that pod out: it logs in again with " +
				"its token under a new login while the broker's BROKER_SERVICE_LOGINS names its account, and that " +
				"login is the person's while BROKER_POD_OPERATORS still maps the account to that person. 204. Needs " +
				"the person and approve scopes; a credential scoped to one project is refused. 404 " +
				"MACHINE_LOGIN_NOT_FOUND for an id the broker holds no machine login of, and " + brokerRelayText + ".",
			Handler:  s.revokeMachineLoginV2,
			Resource: "machine-logins", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: settles,
			Scope:       scopeDeployment,
			IDs:         &idRoute{Reason: "a machine login id"},
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, machineLoginMissingRefusal, brokerUnconfiguredRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/me/signins", Auth: authAny, Versions: onlyV2,
			Description: "The caller's own sign-ins on their machines (`agent-secrets login`), from the secrets broker, " +
				"newest first: one per box key they have not revoked, each {signin_id, machine, thumbprint, created_at, " +
				"last_used_at}, naming the machine's account, the key's thumbprint, when the person signed in there and " +
				"when that machine last used it. Nobody lists another person's: Dispatch names the person from its own " +
				"session. Needs the person scope. " + brokerPageText + ". " + brokerRelayText + ".",
			Handler:  s.listSignInsV2,
			Resource: "signins", Kind: kindCollection, Key: "{id}",
			Query:       brokerListV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[wire.PersonSignInResp]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(brokerUnconfiguredRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/me/signins/{id}", Auth: authAny, Versions: onlyV2,
			Description: "End one of the caller's own sign-ins: that machine trades no token for them again and every " +
				"access token already minted under it ends, while their sign-ins on other machines keep working. " +
				"Answers 200 {tokens_ended}, how many of the caller's live access tokens ended with it, 0 for a sign-in " +
				"revoked already. Needs the person and approve scopes; a credential scoped to one project is refused. " +
				"404 SIGNIN_NOT_FOUND for an id that is no sign-in of the caller's, anyone else's among them, and " +
				brokerRelayText + ".",
			Handler:  s.revokeSignInV2,
			Resource: "signins", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusOK, Response: signInRevokedV2{}},
			OAuthScopes: settles,
			Scope:       scopeDeployment,
			IDs:         &idRoute{Reason: "a sign-in id at the secrets broker"},
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, signinMissingRefusal, brokerUnconfiguredRefusal},
		},
	}
}

// brokerListV2Query is the query of every credential list: one page of what the broker answers.
type brokerListV2Query struct {
	pageQuery
}

// credentialRequestStateV2Input is PUT /api/v2/credential-requests/{id}/state's body of a decision,
// either arm: the state the caller decides the request into and the machine login's typed code,
// the one member of the browser's body Dispatch relays, as decisionFor relays it under /api/v1.
type credentialRequestStateV2Input struct {
	State string  `json:"state" enum:"approved,denied"`
	Code  *string `json:"code"`
}

// credentialRequestDismissV2Input is the same route's body of a dismissal: the state alone, as
// POST /api/v1/credential-requests/{id}/dismiss reads nothing of its body. The approver is the
// caller.
type credentialRequestDismissV2Input struct {
	State string `json:"state" enum:"dismissed"`
}

// signInRevokedV2 is DELETE /api/v2/me/signins/{id}'s answer: how many access tokens ended with the
// sign-in. It is a type of its own, so no /api/v1 shape's tags change (dispatch://INFRA-4071/spec
// R3).
type signInRevokedV2 struct {
	TokensEnded int `json:"tokens_ended"`
}

// relayBrokerRefusalV2 answers a broker call that failed: the broker's own 404 of the id the path
// names as missing, that object's code (dispatch://INFRA-4071/spec R7), and every other failure as
// /api/v1 relays it (relayBrokerResponse). The broker answers every id it holds no record of 404
// NOT_FOUND, a sign-in of anyone else's among them.
func relayBrokerRefusalV2(w http.ResponseWriter, r *http.Request, err error, missing apicode.Code, object string) {
	var brokerErr *cipher.Error
	if errors.As(err, &brokerErr) && brokerErr.Status == http.StatusNotFound && brokerErr.Code == "NOT_FOUND" {
		writeError(w, r, missing, fmt.Sprintf("the secrets broker holds no %s %s", object, r.PathValue("id")))
		return
	}
	relayBrokerResponse(w, r, nil, err)
}

// writeBrokerPage answers one page of a broker list: the items of the broker's answer, body, that
// items reads, sliced as query asks (pageOf), or the broker's failure as /api/v1 relays it.
func writeBrokerPage[Answer, Item any](s *server, w http.ResponseWriter, r *http.Request, body json.RawMessage, err error, items func(Answer) []Item) {
	if err != nil {
		relayBrokerResponse(w, r, nil, err)
		return
	}
	var answer Answer
	if err := json.Unmarshal(body, &answer); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the secrets broker's list: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(items(answer), v2Query[brokerListV2Query](r).pageQuery))
}

// listCredentialGrantsV2 answers GET /api/v2/credential-grants: one page of the grants GET
// /api/v1/credential-grants?approver=me relays, the caller the approver.
func (s *server) listCredentialGrantsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	body, err := client.Grants(r.Context(), model.CanonicalLogin(actor.ID))
	writeBrokerPage(s, w, r, body, err, func(answer wire.ApproverGrantsResponse) []wire.ApproverGrantResp { return answer.Grants })
}

// revokeCredentialGrantV2 answers DELETE /api/v2/credential-grants/{id}: the revoke POST
// /api/v1/credential-grants/{id}/revoke relays (Client.RevokeByApprover), the caller the approver.
// The broker answers a grant of a private project the caller may not see 404 NOT_FOUND, as an id
// it holds no grant of, so that too is CREDENTIAL_GRANT_NOT_FOUND; 503 PROJECT_ACCESS_UNAVAILABLE,
// 403 NOT_APPROVER and 409 GRANT_MOVING are relayed as /api/v1 relays them.
func (s *server) revokeCredentialGrantV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	if _, err := client.RevokeByApprover(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID)); err != nil {
		relayBrokerRefusalV2(w, r, err, apicode.CredentialGrantNotFound, "credential grant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listCredentialRequestsV2 answers GET /api/v2/credential-requests: one page of the requests GET
// /api/v1/credential-requests?approver=me relays. A Dispatch with no broker is 404 FEATURE_OFF, as
// every other credential route but the capabilities read answers it, where /api/v1 answers null:
// an empty page would read as nothing waiting, and a client stops asking only on the refusal.
func (s *server) listCredentialRequestsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	body, err := client.Pending(r.Context(), model.CanonicalLogin(actor.ID))
	writeBrokerPage(s, w, r, body, err, func(answer wire.PendingResponse) []wire.PendingEntry { return answer.Pending })
}

// putCredentialRequestStateV2 answers PUT /api/v2/credential-requests/{id}/state: the approve or the
// deny POST /api/v1/credential-requests/{id}/approve and .../deny relay (Client.Approve,
// Client.Deny), the caller the approver, answered with the request as the broker then records it;
// and the dismissal POST /api/v1/credential-requests/{id}/dismiss relays (dismissCredentialRequestV2).
//
// The no-op rule of dispatch://INFRA-4071/spec R8 is the broker's request already in that state,
// sent by a caller the broker would let decide it: a machine login's typed code is the only thing
// that selects its record, so the record the code reads (Client.MachineLookup) is held to the
// path's id, the state sent and the caller as one who may decide it (recordSetter) before anything
// is relayed, and a PUT that would change nothing answers that record and relays no decision. A
// decision that names no code reads no record: the broker refuses it, and its refusal is relayed.
func (s *server) putCredentialRequestStateV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	if _, dismissal := v2BodyArm(r).(credentialRequestDismissV2Input); dismissal {
		dismissCredentialRequestV2(w, r, client, model.CanonicalLogin(actor.ID))
		return
	}
	input, id, login := v2Body[credentialRequestStateV2Input](r), r.PathValue("id"), model.CanonicalLogin(actor.ID)
	if input.Code != nil {
		record, raw, err := lookupMachineRecord(r.Context(), client, *input.Code)
		if err != nil {
			s.writeBrokerLookupError(w, r, err)
			return
		}
		current := credentialRequestStateV2Input{State: record.State, Code: input.Code}
		if raw != nil && record.RecordID == id && putChangesNothing(input, current, recordSetter(record, login), login) {
			relayBrokerResponse(w, r, raw, nil)
			return
		}
	}
	decide := client.Deny
	if input.State == "approved" {
		decide = client.Approve
	}
	decided, err := decide(r.Context(), id, cipher.Decision{Approver: login, Code: input.Code})
	if err != nil {
		relayBrokerRefusalV2(w, r, err, apicode.CredentialRequestNotFound, "credential request")
		return
	}
	if input.Code == nil {
		relayBrokerResponse(w, r, decided, nil)
		return
	}
	record, raw, err := lookupMachineRecord(r.Context(), client, *input.Code)
	switch {
	case err != nil:
		s.writeBrokerLookupError(w, r, err)
	case raw == nil:
		s.writeHandlerError(w, r, fmt.Errorf("the secrets broker decided credential request %s and then read no record by its code", id))
	case record.RecordID != id:
		// The broker reads a code's newest record (machine.LookupByCode), and a code another
		// login took since can name another record: that one is no answer for this request.
		s.writeHandlerError(w, r, fmt.Errorf("the secrets broker decided credential request %s, and its code now reads record %s", id, record.RecordID))
	default:
		relayBrokerResponse(w, r, raw, nil)
	}
}

// dismissCredentialRequestV2 answers the dismissed arm of PUT
// /api/v2/credential-requests/{id}/state: the dismissal POST /api/v1/credential-requests/{id}/dismiss
// relays (Client.Dismiss), login the approver, answered with the dismissal as the broker records
// it. The no-op rule of dispatch://INFRA-4071/spec R8 is the broker's own: it records a request's
// dismissal once and answers every later one with that dismissal's values, writing nothing, so a
// request already dismissed is answered 200 as it stands. The broker's 404 of an id it holds no
// request of, or of a private project's request the caller may not see, is the route's
// CREDENTIAL_REQUEST_NOT_FOUND, and every other refusal - 403 NOT_APPROVER, 503
// PROJECT_ACCESS_UNAVAILABLE, 409 RECORD_PENDING, 409 RECORD_TERMINAL - is relayed as /api/v1
// relays it.
func dismissCredentialRequestV2(w http.ResponseWriter, r *http.Request, client *cipher.Client, login string) {
	dismissed, err := client.Dismiss(r.Context(), r.PathValue("id"), login)
	if err != nil {
		relayBrokerRefusalV2(w, r, err, apicode.CredentialRequestNotFound, "credential request")
		return
	}
	relayBrokerResponse(w, r, dismissed, nil)
}

// brokerLookupError is a machine-login lookup's call of the broker that failed, which
// relayBrokerResponse answers as /api/v1 relays it; every other failure of lookupMachineRecord is
// Dispatch's own.
type brokerLookupError struct{ err error }

func (e *brokerLookupError) Error() string { return e.err.Error() }
func (e *brokerLookupError) Unwrap() error { return e.err }

// writeBrokerLookupError answers a lookupMachineRecord that failed: the broker's own failure as
// /api/v1 relays it (relayBrokerResponse), and Dispatch's own - a body it could not encode, an
// answer it could not read - as a logged 500 (writeHandlerError), which no broker answer is.
func (s *server) writeBrokerLookupError(w http.ResponseWriter, r *http.Request, err error) {
	var call *brokerLookupError
	if errors.As(err, &call) {
		relayBrokerResponse(w, r, nil, call.err)
		return
	}
	s.writeHandlerError(w, r, err)
}

// lookupMachineRecord reads the machine login the typed code selects (Client.MachineLookup): the
// record and its bytes as the broker answered them, or neither where the broker holds no login of
// that code (404 NO_SUCH_CODE). A failed call of the broker is a brokerLookupError, and any other
// error Dispatch's own.
func lookupMachineRecord(ctx context.Context, client *cipher.Client, code string) (wire.RecordResponse, json.RawMessage, error) {
	request, err := json.Marshal(wire.MachineLookupRequest{Code: code})
	if err != nil {
		return wire.RecordResponse{}, nil, fmt.Errorf("encode a machine-login lookup: %w", err)
	}
	raw, err := client.MachineLookup(ctx, request)
	var brokerErr *cipher.Error
	if errors.As(err, &brokerErr) && brokerErr.Status == http.StatusNotFound && brokerErr.Code == "NO_SUCH_CODE" {
		return wire.RecordResponse{}, nil, nil
	}
	if err != nil {
		return wire.RecordResponse{}, nil, &brokerLookupError{err: err}
	}
	var record wire.RecordResponse
	if err := json.Unmarshal(raw, &record); err != nil {
		return wire.RecordResponse{}, nil, fmt.Errorf("read the secrets broker's machine login: %w", err)
	}
	return record, raw, nil
}

// recordSetter is who set a credential request's state as the no-op rule of
// dispatch://INFRA-4071/spec R8 compares the caller with, login its canonical login: the caller
// where the broker would let it decide stored, and otherwise a setter nobody matches. The broker's
// one rule decides it (record.MayDecide, over the record's kind, approver and service), so the
// no-op admits exactly the callers the decision it stands in for admits: anyone signed in for a
// service's machine login, whatever approver it stored, and for a request for a shared human-tier
// secret (the approver `anyone`); otherwise the one person the approver names, so a person's
// machine login whose approver is `anyone`, which only an older broker opened, admits nobody, and
// its decision is relayed for the broker to refuse.
func recordSetter(stored wire.RecordResponse, login string) putSetter {
	service := ""
	if stored.Service != nil {
		service = *stored.Service
	}
	if record.MayDecide(stored.Kind, stored.Approver, service, login) {
		return setBy(login)
	}
	return setBy("")
}

// lookupMachineLoginV2 answers POST /api/v2/machine-login-lookups: the lookup POST
// /api/v1/credential-requests/machine-lookup relays (Client.MachineLookup), its body the code the
// decoder read, written again.
func (s *server) lookupMachineLoginV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	request, err := json.Marshal(v2Body[wire.MachineLookupRequest](r))
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("encode a machine-login lookup: %w", err))
		return
	}
	body, err := client.MachineLookup(r.Context(), request)
	relayBrokerResponse(w, r, body, err)
}

// listMachineLoginsV2 answers GET /api/v2/machine-logins: one page of the machine logins GET
// /api/v1/machine-logins relays (Client.MachineLogins).
func (s *server) listMachineLoginsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	body, err := client.MachineLogins(r.Context(), model.CanonicalLogin(actor.ID))
	writeBrokerPage(s, w, r, body, err, func(answer wire.LauncherCredentialsResponse) []wire.LauncherCredentialResp {
		return answer.Credentials
	})
}

// revokeMachineLoginV2 answers DELETE /api/v2/machine-logins/{id}: the revoke POST
// /api/v1/machine-logins/{id}/revoke relays (Client.RevokeMachineLogin), the caller the approver.
func (s *server) revokeMachineLoginV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	if _, err := client.RevokeMachineLogin(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID)); err != nil {
		relayBrokerRefusalV2(w, r, err, apicode.MachineLoginNotFound, "machine login")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listSignInsV2 answers GET /api/v2/me/signins: one page of the sign-ins GET /api/v1/signins relays
// (Client.SignIns), the caller the person.
func (s *server) listSignInsV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	body, err := client.SignIns(r.Context(), model.CanonicalLogin(actor.ID))
	writeBrokerPage(s, w, r, body, err, func(answer wire.PersonSignInsResponse) []wire.PersonSignInResp { return answer.SignIns })
}

// revokeSignInV2 answers DELETE /api/v2/me/signins/{id}: the revoke POST /api/v1/signins/{id}/revoke
// relays (Client.RevokeSignIn), the caller the person, answered with how many tokens it ended.
func (s *server) revokeSignInV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	noteActingAgent(r, actor)
	client, ok := s.requireAgentSecrets(w, r)
	if !ok {
		return
	}
	body, err := client.RevokeSignIn(r.Context(), r.PathValue("id"), model.CanonicalLogin(actor.ID))
	if err != nil {
		relayBrokerRefusalV2(w, r, err, apicode.SigninNotFound, "sign-in of yours")
		return
	}
	var revoked wire.PersonSignInRevokedResponse
	if err := json.Unmarshal(body, &revoked); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the secrets broker's sign-in revoke: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, signInRevokedV2{TokensEnded: revoked.TokensEnded})
}
