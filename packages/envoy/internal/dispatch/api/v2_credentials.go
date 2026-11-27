package api

// v2_credentials.go is the credential pages' routes of /api/v2 (R1 of dispatch://INFRA-4071/spec):
// the credential requests waiting on the caller and the state route their
// approve and deny become, the machine-login lookup, the grants and the machine logins the caller
// may revoke, and the caller's own sign-ins, each a relay to the secrets broker as its /api/v1
// route is (credential_requests.go). Five things change. A broker list, which the broker does
// not page, is sliced by Dispatch into the envelope. A revoke is a DELETE (the route map's J5),
// answering 204, or 200 {tokens_ended} for a sign-in. A refusal the broker answers is named as
// every /api/v2 refusal is, under the code the route declares for it, with the broker's message
// and the members the broker's refusal carries beside it (brokerRefusalV2), where /api/v1 relays
// the broker's bytes: the broker's own code at the broker's own status, but its refusals of
// Dispatch's own ServiceAccount token at 502, and its RATE_LIMITED, which spends the bucket of the
// address Dispatch calls from before any token is verified, at 503, since none of them is the
// caller's fault, and a code the route does not declare as 502 AGENT_SECRETS_REFUSAL_UNDECLARED
// (relayBrokerRefusalV2, R7). An id the broker holds no record of answers the broker's own 404
// under one code of its object (R7). And a deny of a service's machine login that the code's
// lookup read already denied answers the broker's 409 RECORD_TERMINAL as the no-op of R8, 200
// with the login as the lookup read it (putCredentialRequestStateV2). Every other answer of the
// broker is relayed verbatim, as /api/v1 relays it.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/wire"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The refusals of the broker's ids a credential route's path names (R7), and of every
// credential route's relay to the secrets broker (brokerRefusals).
var (
	credentialRequestMissingRefusal = apicode.Refusal{Condition: apicode.CredentialRequestMissing, Code: apicode.CredentialRequestNotFound}
	credentialGrantMissingRefusal   = apicode.Refusal{Condition: apicode.CredentialGrantMissing, Code: apicode.CredentialGrantNotFound}
	machineLoginMissingRefusal      = apicode.Refusal{Condition: apicode.MachineLoginMissing, Code: apicode.MachineLoginNotFound}
	signinMissingRefusal            = apicode.Refusal{Condition: apicode.SigninMissing, Code: apicode.SigninNotFound}
	brokerUnconfiguredRefusal       = apicode.Refusal{Condition: apicode.BrokerUnconfigured, Code: apicode.FeatureOff}
	brokerUnreachableRefusal        = apicode.Refusal{Condition: apicode.BrokerUnreachable, Code: apicode.AgentSecretsUnavailable}
	accountTokenUnreadableRefusal   = apicode.Refusal{Condition: apicode.ServiceAccountTokenUnreadable, Code: apicode.AgentSecretsAccountTokenUnreadable}
	brokerRefusalUndeclaredRefusal  = apicode.Refusal{Condition: apicode.BrokerRefusalUndeclared, Code: apicode.AgentSecretsRefusalUndeclared}
)

// The refusals the secrets broker answers on the routes the credential handlers call, each the
// broker's code relayed from it, answered under that code at the broker's own status.
var (
	brokerApproverAccountInputRefusal     = apicode.Refusal{Condition: apicode.BrokerApproverAccountInput, Code: apicode.ApproverAccountInput}
	brokerApproverIsGroupRefusal          = apicode.Refusal{Condition: apicode.BrokerApproverIsGroup, Code: apicode.ApproverIsGroup}
	brokerApproverReplacedRefusal         = apicode.Refusal{Condition: apicode.BrokerApproverReplaced, Code: apicode.ApproverReplaced}
	brokerApproverRequiredRefusal         = apicode.Refusal{Condition: apicode.BrokerApproverRequired, Code: apicode.ApproverRequired}
	brokerCodeMismatchRefusal             = apicode.Refusal{Condition: apicode.BrokerCodeMismatch, Code: apicode.CodeMismatch}
	brokerCodeRequiredRefusal             = apicode.Refusal{Condition: apicode.BrokerCodeRequired, Code: apicode.CodeRequired}
	brokerCredentialIDInputRefusal        = apicode.Refusal{Condition: apicode.BrokerCredentialIDInput, Code: apicode.CredentialIDInput}
	brokerDecideOnBrokerPageRefusal       = apicode.Refusal{Condition: apicode.BrokerDecideOnBrokerPage, Code: apicode.DecideOnBrokerPage}
	brokerGrantIDInputRefusal             = apicode.Refusal{Condition: apicode.BrokerGrantIDInput, Code: apicode.GrantIDInput}
	brokerGrantMovingRefusal              = apicode.Refusal{Condition: apicode.BrokerGrantMoving, Code: apicode.GrantMoving}
	brokerInternalRefusal                 = apicode.Refusal{Condition: apicode.BrokerInternal, Code: apicode.InternalError}
	brokerInvalidDecisionRefusal          = apicode.Refusal{Condition: apicode.BrokerInvalidDecision, Code: apicode.InvalidDecision}
	brokerInvalidDismissalRefusal         = apicode.Refusal{Condition: apicode.BrokerInvalidDismissal, Code: apicode.InvalidDismissal}
	brokerInvalidLookupRefusal            = apicode.Refusal{Condition: apicode.BrokerInvalidLookup, Code: apicode.InvalidLookup}
	brokerInvalidRevokeRefusal            = apicode.Refusal{Condition: apicode.BrokerInvalidRevoke, Code: apicode.InvalidRevoke}
	brokerJudgeDecidesRefusal             = apicode.Refusal{Condition: apicode.BrokerJudgeDecides, Code: apicode.JudgeDecides}
	brokerKeyBindsReadTokenRefusal        = apicode.Refusal{Condition: apicode.BrokerKeyBindsReadToken, Code: apicode.KeyBindsReadToken}
	brokerKeyHoldsLiveCredentialRefusal   = apicode.Refusal{Condition: apicode.BrokerKeyHoldsLiveCredential, Code: apicode.KeyHoldsLiveCredential}
	brokerMemberKindUncheckableRefusal    = apicode.Refusal{Condition: apicode.BrokerMemberKindUncheckable, Code: apicode.MemberKindUncheckable}
	brokerNoSuchCodeRefusal               = apicode.Refusal{Condition: apicode.BrokerNoSuchCode, Code: apicode.NoSuchCode}
	brokerNotApproverRefusal              = apicode.Refusal{Condition: apicode.BrokerNotApprover, Code: apicode.NotApprover}
	brokerPersonRequiredRefusal           = apicode.Refusal{Condition: apicode.BrokerPersonRequired, Code: apicode.PersonRequired}
	brokerProjectAccessUnavailableRefusal = apicode.Refusal{Condition: apicode.BrokerProjectAccessUnavailable, Code: apicode.ProjectAccessUnavailable}
	brokerRecordIDInputRefusal            = apicode.Refusal{Condition: apicode.BrokerRecordIDInput, Code: apicode.RecordIDInput}
	brokerRecordPendingRefusal            = apicode.Refusal{Condition: apicode.BrokerRecordPending, Code: apicode.RecordPending}
	brokerRecordTerminalRefusal           = apicode.Refusal{Condition: apicode.BrokerRecordTerminal, Code: apicode.RecordTerminal}
	brokerRequestEndedRefusal             = apicode.Refusal{Condition: apicode.BrokerRequestEnded, Code: apicode.RequestEnded}
	brokerRequestTooLargeRefusal          = apicode.Refusal{Condition: apicode.BrokerRequestTooLarge, Code: apicode.RequestTooLarge}
	brokerServiceLogsInWithTokenRefusal   = apicode.Refusal{Condition: apicode.BrokerServiceLogsInWithToken, Code: apicode.ServiceLogsInWithToken}
	brokerSigninIDInputRefusal            = apicode.Refusal{Condition: apicode.BrokerSigninIDInput, Code: apicode.SigninIDInput}
	brokerStateInputRefusal               = apicode.Refusal{Condition: apicode.BrokerStateInput, Code: apicode.StateInput}
)

// brokerAccountRefusal is a refusal the broker answers Dispatch's own call with before the route
// runs (brokerAccountRefusals): the status the broker answers it at, and the refusal /api/v2
// answers it as, under the broker's code at a status of its own (apicode.AccountLacksPower and
// apicode.RateLimited say why).
type brokerAccountRefusal struct {
	BrokerStatus int
	Refusal      apicode.Refusal
}

// brokerAccountRefusals are the refusals the broker answers Dispatch's own call with before the
// route runs, on every broker route a credential handler calls, each of which takes accountAuth
// (the broker's route table), so every call declares them: RATE_LIMITED, which spends the bucket
// of the address Dispatch calls from before any token is verified, and the refusals of Dispatch's
// own ServiceAccount token.
var brokerAccountRefusals = []brokerAccountRefusal{
	{http.StatusForbidden, apicode.Refusal{Condition: apicode.BrokerAccountLacksPower, Code: apicode.AccountLacksPower}},
	{http.StatusForbidden, apicode.Refusal{Condition: apicode.BrokerAccountNotListed, Code: apicode.AccountNotListed}},
	{http.StatusUnauthorized, apicode.Refusal{Condition: apicode.BrokerAccountTokenExpired, Code: apicode.AccountTokenExpired}},
	{http.StatusUnauthorized, apicode.Refusal{Condition: apicode.BrokerAccountTokenInvalid, Code: apicode.AccountTokenInvalid}},
	{http.StatusUnauthorized, apicode.Refusal{Condition: apicode.BrokerAccountTokenTooLongLived, Code: apicode.AccountTokenTooLongLived}},
	{http.StatusUnauthorized, apicode.Refusal{Condition: apicode.BrokerAccountTokensOff, Code: apicode.AccountTokensOff}},
	{http.StatusTooManyRequests, apicode.Refusal{Condition: apicode.BrokerRateLimited, Code: apicode.RateLimited}},
}

// brokerCall is one call a credential handler makes of the secrets broker: the broker's routes it
// calls, "<method> <pattern>" as the broker's route table writes them, and every refusal the
// broker answers on them, each as the refusal /api/v2 answers it with (relayBrokerRefusalV2).
// Refusals are the refusals of the routes' handler, under the broker's own code at the broker's
// own status, and brokerAccountRefusals are every call's. Missing, where the route's path names
// one of the broker's records, is the refusal of the broker's 404 NOT_FOUND: the not-found code of
// that object (R7). TestTheBrokerCallsDeclareEveryRefusalTheBrokerAnswers holds each call to the
// refusals the broker's source answers on its routes.
type brokerCall struct {
	Routes   []string
	Refusals []apicode.Refusal
	Missing  apicode.Refusal
}

// brokerNotFoundCode is the code the broker answers, at 404, an id it holds no record of.
const brokerNotFoundCode = "NOT_FOUND"

// code is the code /api/v2 answers a refusal of call with that the broker answered at status
// under name, and whether call declares one.
func (call brokerCall) code(status int, name string) (code apicode.Code, declared bool) {
	if status == http.StatusNotFound && name == brokerNotFoundCode && !call.Missing.Code.IsZero() {
		return call.Missing.Code, true
	}
	for _, refusal := range call.Refusals {
		if refusal.Code.Name() == name && refusal.Code.Status() == status {
			return refusal.Code, true
		}
	}
	for _, account := range brokerAccountRefusals {
		if account.Refusal.Code.Name() == name && account.BrokerStatus == status {
			return account.Refusal.Code, true
		}
	}
	return code, false
}

// without is call with the refusal of code left out, for a route that answers that refusal of the
// broker otherwise: PUT /api/v2/credential-requests/{id}/state reads the lookup's NO_SUCH_CODE as a
// code that selects no record (lookupMachineRecord) and relays the decision for the broker to
// answer, so its row declares the lookup's refusals without it.
func (call brokerCall) without(code apicode.Code) brokerCall {
	call.Refusals = slices.DeleteFunc(slices.Clone(call.Refusals), func(refusal apicode.Refusal) bool { return refusal.Code == code })
	return call
}

// The calls the credential handlers make of the secrets broker, each named for the cipher.Client
// method that makes it.
var (
	// brokerGrantsCall is Client.Grants.
	brokerGrantsCall = brokerCall{
		Routes:   []string{"GET /v1/grants"},
		Refusals: []apicode.Refusal{brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerInternalRefusal},
	}
	// brokerRevokeGrantCall is Client.RevokeByApprover.
	brokerRevokeGrantCall = brokerCall{
		Routes: []string{"POST /v1/grants/{id}/revoke-by-approver"},
		Refusals: []apicode.Refusal{
			brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerGrantIDInputRefusal, brokerGrantMovingRefusal,
			brokerInternalRefusal, brokerInvalidRevokeRefusal, brokerMemberKindUncheckableRefusal, brokerNotApproverRefusal,
			brokerProjectAccessUnavailableRefusal, brokerRequestEndedRefusal, brokerRequestTooLargeRefusal,
		},
		Missing: credentialGrantMissingRefusal,
	}
	// brokerPendingCall is Client.Pending.
	brokerPendingCall = brokerCall{
		Routes: []string{"GET /v1/pending"},
		Refusals: []apicode.Refusal{
			brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerInternalRefusal, brokerStateInputRefusal,
		},
	}
	// brokerDecisionCall is Client.Approve and Client.Deny, whose routes answer alike.
	brokerDecisionCall = brokerCall{
		Routes: []string{"POST /v1/credential-requests/{record}/approve", "POST /v1/credential-requests/{record}/deny"},
		Refusals: []apicode.Refusal{
			brokerApproverAccountInputRefusal, brokerApproverIsGroupRefusal, brokerApproverReplacedRefusal, brokerApproverRequiredRefusal,
			brokerCodeMismatchRefusal, brokerCodeRequiredRefusal,
			brokerDecideOnBrokerPageRefusal, brokerInternalRefusal, brokerInvalidDecisionRefusal, brokerJudgeDecidesRefusal,
			brokerKeyBindsReadTokenRefusal, brokerKeyHoldsLiveCredentialRefusal, brokerMemberKindUncheckableRefusal,
			brokerNotApproverRefusal, brokerProjectAccessUnavailableRefusal, brokerRecordIDInputRefusal,
			brokerRecordTerminalRefusal, brokerRequestTooLargeRefusal, brokerServiceLogsInWithTokenRefusal,
		},
		Missing: credentialRequestMissingRefusal,
	}
	// brokerDismissalCall is Client.Dismiss.
	brokerDismissalCall = brokerCall{
		Routes: []string{"POST /v1/credential-requests/{record}/dismiss"},
		Refusals: []apicode.Refusal{
			brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerInternalRefusal, brokerInvalidDismissalRefusal,
			brokerMemberKindUncheckableRefusal, brokerNotApproverRefusal, brokerProjectAccessUnavailableRefusal,
			brokerRecordIDInputRefusal, brokerRecordPendingRefusal, brokerRecordTerminalRefusal, brokerRequestTooLargeRefusal,
		},
		Missing: credentialRequestMissingRefusal,
	}
	// brokerLookupCall is Client.MachineLookup.
	brokerLookupCall = brokerCall{
		Routes: []string{"POST /v1/machine-logins/lookup"},
		Refusals: []apicode.Refusal{
			brokerCodeRequiredRefusal, brokerInternalRefusal, brokerInvalidLookupRefusal, brokerNoSuchCodeRefusal,
			brokerRequestTooLargeRefusal,
		},
	}
	// brokerMachineLoginsCall is Client.MachineLogins.
	brokerMachineLoginsCall = brokerCall{
		Routes:   []string{"GET /v1/launcher-credentials"},
		Refusals: []apicode.Refusal{brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerInternalRefusal},
	}
	// brokerRevokeMachineLoginCall is Client.RevokeMachineLogin.
	brokerRevokeMachineLoginCall = brokerCall{
		Routes: []string{"POST /v1/launcher-credentials/{id}/revoke-by-approver"},
		Refusals: []apicode.Refusal{
			brokerApproverIsGroupRefusal, brokerApproverRequiredRefusal, brokerCredentialIDInputRefusal, brokerInternalRefusal,
			brokerInvalidRevokeRefusal, brokerNotApproverRefusal, brokerRequestTooLargeRefusal,
		},
		Missing: machineLoginMissingRefusal,
	}
	// brokerSignInsCall is Client.SignIns.
	brokerSignInsCall = brokerCall{
		Routes:   []string{"GET /v1/person-signins"},
		Refusals: []apicode.Refusal{brokerInternalRefusal, brokerPersonRequiredRefusal},
	}
	// brokerRevokeSignInCall is Client.RevokeSignIn.
	brokerRevokeSignInCall = brokerCall{
		Routes: []string{"POST /v1/person-signins/{id}/revoke"},
		Refusals: []apicode.Refusal{
			brokerInternalRefusal, brokerInvalidRevokeRefusal, brokerPersonRequiredRefusal, brokerRequestTooLargeRefusal,
			brokerSigninIDInputRefusal,
		},
		Missing: signinMissingRefusal,
	}
)

// brokerRefusals are the refusals of a credential route, which relays to the secrets broker
// through calls (requireAgentSecrets, relayBrokerRefusalV2), after the route's own: a Dispatch with
// none, a broker that cannot be reached, a ServiceAccount token Dispatch cannot read, a refusal
// the broker answers under a code its call does not declare, and every refusal each call declares.
func brokerRefusals(own []apicode.Refusal, calls ...brokerCall) []apicode.Refusal {
	refusals := append(own, brokerUnconfiguredRefusal, brokerUnreachableRefusal, accountTokenUnreadableRefusal, brokerRefusalUndeclaredRefusal)
	for _, call := range calls {
		refusals = append(refusals, call.Refusals...)
		for _, account := range brokerAccountRefusals {
			refusals = append(refusals, account.Refusal)
		}
		if !call.Missing.Code.IsZero() {
			refusals = append(refusals, call.Missing)
		}
	}
	return refusals
}

// brokerRefusalAnswers are the refusal bodies of a credential route that calls the secrets broker
// through calls (routeShape.Refusals): brokerRefusalV2 at each status a 4xx refusal of the
// broker's lands on, which carries the broker's answer_id, and at the 502 of a refusal no call
// declares, which carries it where the broker's answer did.
func brokerRefusalAnswers(calls ...brokerCall) []routeAnswer {
	statuses := []int{apicode.AgentSecretsRefusalUndeclared.Status()}
	for _, call := range calls {
		for _, refusal := range call.Refusals {
			if refusal.Code.Status() < http.StatusInternalServerError {
				statuses = append(statuses, refusal.Code.Status())
			}
		}
		for _, account := range brokerAccountRefusals {
			statuses = append(statuses, account.Refusal.Code.Status())
		}
		if !call.Missing.Code.IsZero() {
			statuses = append(statuses, call.Missing.Code.Status())
		}
	}
	slices.Sort(statuses)
	answers := make([]routeAnswer, 0, len(statuses))
	for _, status := range slices.Compact(statuses) {
		answers = append(answers, routeAnswer{Status: status, Body: brokerRefusalV2{}})
	}
	return answers
}

// brokerRelayText is what every credential route says of the broker's answers.
const brokerRelayText = "404 FEATURE_OFF without a configured secrets broker, 503 AGENT_SECRETS_UNAVAILABLE when it " +
	"cannot be reached, 500 AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE when Dispatch cannot read its own ServiceAccount " +
	"token for it, and every other refusal the broker answers under its own code, each listed in x-refusal-codes, " +
	"with its message and, for a 4xx refusal of the broker's, its answer_id: at the broker's status, but its " +
	"refusals of Dispatch's own ServiceAccount token (ACCOUNT_LACKS_POWER, ACCOUNT_NOT_LISTED, ACCOUNT_TOKENS_OFF, " +
	"ACCOUNT_TOKEN_EXPIRED, ACCOUNT_TOKEN_INVALID, ACCOUNT_TOKEN_TOO_LONG_LIVED) at 502 and its RATE_LIMITED at 503 " +
	"with the broker's Retry-After, since the broker refused Dispatch rather than the caller; a refusal under a " +
	"code or at a status that is not listed is 502 AGENT_SECRETS_REFUSAL_UNDECLARED, naming the broker's status, " +
	"code and message"

// brokerPageText is what every credential list says of its page: the broker does not page, so
// Dispatch slices what it answers.
const brokerPageText = "limit 1 to 200 (default 50) and offset over what the broker answers, which Dispatch slices; " +
	"{items, total, limit, offset}"

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

// brokerRefusalV2 is the body /api/v2 answers a refusal of the secrets broker with
// (relayBrokerRefusalV2): the code /api/v2 answers it under and the broker's message, as errorBody
// carries them, and each member the broker's refusal carries beside those two, where it carries
// one (TestTheBrokerCallsDeclareEveryRefusalTheBrokerAnswers holds the broker's to these).
type brokerRefusalV2 struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	// AnswerID is the broker's id of its answer, which its log line of the refusal names: the
	// broker adds it to every 4xx refusal it answers (internal/cipher/answer), so a person tracing
	// a refused call has a thing to quote. A refusal at 5xx carries none.
	AnswerID string `json:"answer_id,omitempty"`
	// ApproveURL is the record's page on the broker, where a person signed in to the broker
	// decides the secret request: DECIDE_ON_BROKER_PAGE carries it, but on a broker that serves no
	// page.
	ApproveURL string `json:"approve_url,omitempty"`
}

// relayBrokerRefusalV2 answers a call of the secrets broker that failed, call the call it made
// (dispatch://INFRA-4071/spec R7). A refusal the broker answered is answered brokerRefusalV2,
// under the code call declares for its status and code (brokerCall.code), named as writeError
// names it (refusalPair), with the broker's message and the members its refusal carries beside
// it: its 404 NOT_FOUND of the id the path names, a sign-in of anyone else's among them, as that
// object's code, and every other under the broker's own code. A refusal call does not declare is
// 502 AGENT_SECRETS_REFUSAL_UNDECLARED, its message naming the broker's status, code and message.
// A refusal answered at 503 carries the broker's Retry-After where the broker sent one, as its
// RATE_LIMITED does. A call the broker never answered is answered as /api/v1 answers it
// (writeBrokerUnanswered); /api/v1 relays a refusal's bytes (relayBrokerResponse).
func relayBrokerRefusalV2(w http.ResponseWriter, r *http.Request, err error, call brokerCall) {
	var brokerErr *cipher.Error
	if !errors.As(err, &brokerErr) {
		writeBrokerUnanswered(w, r, err)
		return
	}
	// The broker answers every refusal as a JSON object; a body that is not one leaves every
	// member empty, and no code (cipher.Error.Code), which no call declares.
	var refusal brokerRefusalV2
	_ = json.Unmarshal(brokerErr.Body, &refusal)
	code, declared := call.code(brokerErr.Status, brokerErr.Code)
	if !declared {
		code = apicode.AgentSecretsRefusalUndeclared
		message := fmt.Sprintf("the secrets broker answered %d %s", brokerErr.Status, cmp.Or(brokerErr.Code, "naming no code"))
		if refusal.Error != "" {
			message += ": " + refusal.Error
		}
		refusal.Error = message
	}
	name, status := refusalPair(r, code)
	refusal.Code = name
	if status == http.StatusServiceUnavailable && brokerErr.RetryAfter != "" {
		w.Header().Set("Retry-After", brokerErr.RetryAfter)
	}
	WriteJSON(w, status, refusal)
}

// writeBrokerPage answers one page of a broker list: the items of the broker's answer, body, that
// items reads, sliced as query asks (pageOf), or the failure of call (relayBrokerRefusalV2).
func writeBrokerPage[Answer, Item any](s *server, w http.ResponseWriter, r *http.Request, body json.RawMessage, err error, call brokerCall, items func(Answer) []Item) {
	if err != nil {
		relayBrokerRefusalV2(w, r, err, call)
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
	writeBrokerPage(s, w, r, body, err, brokerGrantsCall, func(answer wire.ApproverGrantsResponse) []wire.ApproverGrantResp { return answer.Grants })
}

// revokeCredentialGrantV2 answers DELETE /api/v2/credential-grants/{id}: the revoke POST
// /api/v1/credential-grants/{id}/revoke relays (Client.RevokeByApprover), the caller the approver.
// The broker answers a grant of a private project the caller may not see 404 NOT_FOUND, as an id
// it holds no grant of, so that too is CREDENTIAL_GRANT_NOT_FOUND; 503 PROJECT_ACCESS_UNAVAILABLE,
// 503 MEMBER_KIND_UNCHECKABLE (#4848), 403 NOT_APPROVER and 409 GRANT_MOVING are answered under
// the broker's own codes, as every refusal of brokerRevokeGrantCall is.
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
		relayBrokerRefusalV2(w, r, err, brokerRevokeGrantCall)
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
	writeBrokerPage(s, w, r, body, err, brokerPendingCall, func(answer wire.PendingResponse) []wire.PendingEntry { return answer.Pending })
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
// deny of a service's login is the one decision whose caller Dispatch cannot judge: the broker
// refuses it to a person Dispatch calls a restricted member, a member kind it asks for, and checks
// that before it checks the record's state (machine.Service.ApplyDecision). So such a deny is
// relayed, and where the code read the login already denied, the broker's 409 RECORD_TERMINAL,
// which it answers only a caller it admitted, is that same no-op, answered with the login as the
// code read it. A decision that names no code reads no record: the broker refuses it, and its
// refusal is answered as every refusal of brokerDecisionCall is (relayBrokerRefusalV2).
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
	// standing is a service's login the code read already denied, which a deny sent again leaves
	// as it stands once the broker admits its caller.
	var standing json.RawMessage
	if input.Code != nil {
		record, raw, err := lookupMachineRecord(r.Context(), client, *input.Code)
		if err != nil {
			s.writeBrokerLookupError(w, r, err)
			return
		}
		current := credentialRequestStateV2Input{State: record.State, Code: input.Code}
		switch {
		case raw == nil || record.RecordID != id:
			// The code read no record, or another record than the path's: the broker decides.
		case input.State == "denied" && record.Service != nil && *record.Service != "":
			if record.State == input.State {
				standing = raw
			}
		case putChangesNothing(input, current, recordSetter(record, login), login):
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
		var brokerErr *cipher.Error
		if standing != nil && errors.As(err, &brokerErr) &&
			brokerErr.Status == apicode.RecordTerminal.Status() && brokerErr.Code == apicode.RecordTerminal.Name() {
			relayBrokerResponse(w, r, standing, nil)
			return
		}
		relayBrokerRefusalV2(w, r, err, brokerDecisionCall)
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
// PROJECT_ACCESS_UNAVAILABLE, 503 MEMBER_KIND_UNCHECKABLE (#4848), 409 RECORD_PENDING, 409
// RECORD_TERMINAL - is answered under the broker's own code, as every refusal of
// brokerDismissalCall is.
func dismissCredentialRequestV2(w http.ResponseWriter, r *http.Request, client *cipher.Client, login string) {
	dismissed, err := client.Dismiss(r.Context(), r.PathValue("id"), login)
	if err != nil {
		relayBrokerRefusalV2(w, r, err, brokerDismissalCall)
		return
	}
	relayBrokerResponse(w, r, dismissed, nil)
}

// brokerLookupError is a machine-login lookup's call of the broker that failed, which
// relayBrokerRefusalV2 answers; every other failure of lookupMachineRecord is Dispatch's own.
type brokerLookupError struct{ err error }

func (e *brokerLookupError) Error() string { return e.err.Error() }
func (e *brokerLookupError) Unwrap() error { return e.err }

// writeBrokerLookupError answers a lookupMachineRecord that failed: the broker's own failure as
// every failure of brokerLookupCall is answered (relayBrokerRefusalV2), and Dispatch's own - a
// body it could not encode, an answer it could not read - as a logged 500 (writeHandlerError),
// which no broker answer is.
func (s *server) writeBrokerLookupError(w http.ResponseWriter, r *http.Request, err error) {
	var call *brokerLookupError
	if errors.As(err, &call) {
		relayBrokerRefusalV2(w, r, call.err, brokerLookupCall)
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
	if errors.As(err, &brokerErr) && brokerErr.Status == apicode.NoSuchCode.Status() && brokerErr.Code == apicode.NoSuchCode.Name() {
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
// rule over the record's kind, approver and service decides it (record.MayDecide), so for a
// machine login, the one record this route reads, the no-op admits exactly the callers the
// decision it stands in for admits, a deny of a service's login aside, which
// putCredentialRequestStateV2 leaves to the broker: anyone signed in for a service's machine
// login, whatever approver it stored; otherwise the one person the approver names, so a person's
// machine login whose approver is `anyone`, which only an older broker opened, admits nobody, and
// its decision is relayed for the broker to refuse. MayDecide also admits anyone for a request
// for a shared human-tier secret (the approver `anyone`), where since #4848 the broker refuses a
// person Dispatch calls a restricted member; no such record reaches this rule, since the code's
// lookup reads machine logins alone. Dispatch holds no group's membership, so it hands the rule
// no groups: a group approver (group:<address>) admits nobody here, and its decision is relayed
// for the broker, which looks the group up, to decide. That costs nothing for a machine login,
// since the rule admits no group there at all.
func recordSetter(stored wire.RecordResponse, login string) putSetter {
	service := ""
	if stored.Service != nil {
		service = *stored.Service
	}
	if record.MayDecide(stored.Kind, stored.Approver, service, login, nil) {
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
	if err != nil {
		relayBrokerRefusalV2(w, r, err, brokerLookupCall)
		return
	}
	relayBrokerResponse(w, r, body, nil)
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
	writeBrokerPage(s, w, r, body, err, brokerMachineLoginsCall, func(answer wire.LauncherCredentialsResponse) []wire.LauncherCredentialResp {
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
		relayBrokerRefusalV2(w, r, err, brokerRevokeMachineLoginCall)
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
	writeBrokerPage(s, w, r, body, err, brokerSignInsCall, func(answer wire.PersonSignInsResponse) []wire.PersonSignInResp { return answer.SignIns })
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
		relayBrokerRefusalV2(w, r, err, brokerRevokeSignInCall)
		return
	}
	var revoked wire.PersonSignInRevokedResponse
	if err := json.Unmarshal(body, &revoked); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the secrets broker's sign-in revoke: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, signInRevokedV2{TokensEnded: revoked.TokensEnded})
}
