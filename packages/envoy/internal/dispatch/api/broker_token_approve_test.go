// broker_token_approve_test.go holds that no token the secrets broker issued for a machine to hold
// holds the approve scope, whatever its project scope, so every route that needs approve refuses
// each of them at the gate, while the browser session of the person a token names is answered
// there as before. The one broker-issued token that holds approve, the approve-only token the
// broker's proxy spends and no machine holds, is approve_only_token_test.go's. A broker token is
// made from a file on the machine that holds it (a machine login or a person's sign-in), so
// whoever copies that file holds it; approve settles what only the person settles, so it needs
// the person's browser, a personal token they chose to give it, or that approve-only token. The
// answer to an approval request checks approve inside its handler, and its test is
// TestAnsweringAnApprovalAskNeedsTheApproveScope's broker row. A job's token does not hold person
// either, since it names no person, so every route that needs person refuses it at the same gate.
//
// The tests at the end of this file hold that token's decision of a credential request: its
// approve and its deny decide as its person, bound to the account its credential records where the
// account binding holds, and every other broker token is refused them.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// approveTestToken is one kind of broker-issued token, as the broker answers its introspection,
// and the refusal every route that needs approve answers it: its code, and the scope its message
// and its WWW-Authenticate challenge name, empty for a refusal that carries no challenge.
type approveTestToken struct {
	name   string
	token  string
	answer ciphertoken.Introspection
	code   string
	scope  string
}

// approveTestTokens is every kind of token the broker mints for a session: one acting as its
// person, refused for approve itself; one an operator's entry mapped to one project, one of a
// machine nobody signed in on and a job's two, named and public, which hold no scope and are
// refused for person, the row's first; and a read-only one, which the read-only gate answers
// before any scope is read.
func approveTestTokens(jkt string) []approveTestToken {
	return []approveTestToken{
		{"a person-scoped broker token", "a-person-scoped-token", personScopedTokenAnswer(jkt), codeScopeRequired, model.ScopeApprove},
		{"a named broker token", "a-named-token", liveNamedToken(jkt), codeScopeRequired, model.ScopePerson},
		{"a service-only broker token", "a-service-only-token", serviceOnlyToken(jkt), codeScopeRequired, model.ScopePerson},
		{"a job's named broker token", "a-job-named-token", liveJobNamedToken(jkt), codeScopeRequired, model.ScopePerson},
		{"a job's public broker token", "a-job-public-token", liveJobPublicToken(jkt), codeScopeRequired, model.ScopePerson},
		{"a read-only broker token", "a-read-only-token", readOnlyToken(jkt), contracts.DispatchReadOnlyToken, ""},
	}
}

// approveRows is every row of the server's own route table whose OAuth scopes name approve. The
// rows are read from the table, never listed here, so a row added later is held to the rule too.
func approveRows(served *server) []apiRoute {
	var rows []apiRoute
	for _, route := range served.routes() {
		if slices.Contains(route.OAuthScopes, model.ScopeApprove) {
			rows = append(rows, route)
		}
	}
	return rows
}

// newCredentialRelayRig is newBrokerTokenRig with the credential routes relaying to a fake secrets
// broker, which records every call Dispatch relays to it.
func newCredentialRelayRig(t *testing.T) (*fakeBrokerRig, *brokerTokenRig) {
	t.Helper()
	credentials := newFakeBrokerRig(t)
	return credentials, newBrokerTokenRigOn(t, testServerOptions{agentSecretsURL: credentials.server.URL})
}

// TestNoBrokerTokenGetsApprove: each row of the route table that needs approve refuses every kind
// of broker token 403 before its handler runs, the token that acts as its person refused for
// approve itself, and nothing reaches the secrets broker's decision routes. The same person's
// browser session is not refused for its credential on any row. The rows are the table's own; a
// walk that found none, or missed a row a stolen machine file would use (a credential approve, a
// machine login's revoke, a personal token's mint, a decision's Keep), fails rather than passes.
func TestNoBrokerTokenGetsApprove(t *testing.T) {
	t.Parallel()
	credentials, rig := newCredentialRelayRig(t)
	caller := newBrokerCaller(t)
	tokens := approveTestTokens(caller.jkt)
	for _, token := range tokens {
		rig.broker.Says(token.token, token.answer)
	}

	rows := approveRows(rig.served)
	names := make([]string, 0, len(rows))
	for _, route := range rows {
		names = append(names, route.Method+" "+route.Pattern)
	}
	for _, want := range []string{
		"POST /api/v1/credential-requests/{id}/approve",
		"POST /api/v1/machine-logins/{id}/revoke",
		"POST /api/v1/me/agent-tokens",
		"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("the route table's approve rows %v do not hold %s: the walk below cannot hold it to the rule", names, want)
		}
	}
	t.Logf("the route table names approve on %d rows: %v", len(rows), names)

	for _, route := range rows {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := routeProbePath(route.Pattern)
			body := json.RawMessage(routeProbeBody(route))
			before := credentials.calls
			for _, token := range tokens {
				response := rig.serve(caller.presenting(t, route.Method, path, token.token, body))
				refusal := decodeBody[refusalAnswer](t, response)
				if response.Code != http.StatusForbidden || refusal.Code != token.code {
					t.Fatalf("%s %s under %s = %d %s, want 403 %s", route.Method, route.Pattern, token.name,
						response.Code, response.Body.String(), token.code)
				}
				if token.scope == "" {
					continue
				}
				if challenge := response.Header().Get("WWW-Authenticate"); !strings.Contains(refusal.Error, token.scope) ||
					challenge != insufficientScopeChallenge(token.scope) {
					t.Fatalf("%s %s under %s refused %q with challenge %q, want the %s scope named in both",
						route.Method, route.Pattern, token.name, refusal.Error, challenge, token.scope)
				}
			}
			if reached := credentials.calls - before; reached != 0 {
				t.Fatalf("%s %s: the broker tokens' requests reached the secrets broker %d times, want none", route.Method, route.Pattern, reached)
			}
			browser := rig.serve(withPerson(brokerRequest(t, route.Method, path, body, "", ""), brokerTestPerson))
			if refused, why := authRefusal(browser); refused {
				t.Fatalf("%s %s refuses %s's browser session with %s: %s", route.Method, route.Pattern, brokerTestPerson, why, browser.Body.String())
			}
		})
	}
}

// TestABrokerTokenGrantedApproveStillMintsAndDecidesNothing: the scope gate is not the only thing
// that keeps a broker token from minting and deciding. Here a person-scoped broker token is given
// approve beside person (brokerPersonScopes, the grant TestNoBrokerTokenGetsApprove fails on), so
// the gate lets it through to the mint, the approve and the deny; each handler still refuses it,
// 403 OAUTH_SCOPE_REQUIRED naming approve, before anything reaches the secrets broker or writes a
// token (refuseUnboundCredential): the re-checks those handlers make once a body has arrived have
// no arm for a broker token that is not approve-only, whose decision alone carries a binding to the
// account it records (TestABrokerTokenThatIsNotApproveOnlyDecidesNothingWhateverItRecords). It
// changes a package variable, so it does not run in parallel.
//
// Control: refuseUnboundCredential answering nil. The mint then writes a token (201) and the
// approve and the deny reach the secrets broker.
func TestABrokerTokenGrantedApproveStillMintsAndDecidesNothing(t *testing.T) {
	granted := brokerPersonScopes
	brokerPersonScopes = []string{model.ScopePerson, model.ScopeApprove}
	t.Cleanup(func() { brokerPersonScopes = granted })
	credentials, rig := newCredentialRelayRig(t)
	caller := newBrokerCaller(t)
	const token = "a-person-scoped-token-granted-approve"
	rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
	const name = "minted by a broker token granted approve"
	for _, c := range []struct{ what, method, path, body string }{
		{"the approve", http.MethodPost, "/api/v1/credential-requests/request-1/approve", `{}`},
		{"the deny", http.MethodPost, "/api/v1/credential-requests/request-1/deny", `{}`},
		{"the mint", http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"` + name + `"}`},
	} {
		before := credentials.calls
		response := rig.serve(caller.presenting(t, c.method, c.path, token, json.RawMessage(c.body)))
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired ||
			!strings.Contains(refusal.Error, "a broker-issued token never mints a token or decides a credential request") {
			t.Errorf("%s by a broker token granted approve = %d %s, want 403 %s from the handler's own refusal",
				c.what, response.Code, response.Body.String(), codeScopeRequired)
		}
		if challenge := response.Header().Get("WWW-Authenticate"); challenge != insufficientScopeChallenge(model.ScopeApprove) {
			t.Errorf("%s by a broker token granted approve carried the challenge %q, want %q", c.what, challenge, insufficientScopeChallenge(model.ScopeApprove))
		}
		if reached := credentials.calls - before; reached != 0 {
			t.Errorf("%s by a broker token granted approve reached the secrets broker %d times, want none", c.what, reached)
		}
	}
	var minted int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from agent_tokens where name = $1`, name).Scan(&minted); err != nil {
		t.Fatal(err)
	}
	if minted != 0 {
		t.Errorf("the mint by a broker token granted approve wrote %d token rows, want none", minted)
	}
}

// TestNoJobTokenReachesARowThatNeedsAPerson: a job's token names its identity and no person, so it
// holds no OAuth scope, and each route whose declaration's OAuthScopes name person refuses both of
// a job's tokens 403 OAUTH_SCOPE_REQUIRED naming person before its handler runs; a route that needs
// approve as well names person, the first scope of its set. The walk is over every route the
// server mounts, on a server that mounts the test hooks, so every declaration is probed.
func TestNoJobTokenReachesARowThatNeedsAPerson(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRigOn(t, testServerOptions{testHooks: true})
	caller := newBrokerCaller(t)
	tokens := []struct {
		name, token string
		answer      ciphertoken.Introspection
	}{
		{"a job's named token", "a-job-named-token", liveJobNamedToken(caller.jkt)},
		{"a job's public token", "a-job-public-token", liveJobPublicToken(caller.jkt, "PRIV")},
	}
	for _, token := range tokens {
		rig.broker.Says(token.token, token.answer)
	}
	probed := 0
	for _, route := range rig.served.routes() {
		if !slices.Contains(route.OAuthScopes, model.ScopePerson) {
			continue
		}
		name := route.Method + " " + route.Pattern
		probed++
		t.Run(name, func(t *testing.T) {
			path := routeProbePath(route.Pattern)
			var body any
			if route.Method != http.MethodGet {
				body = json.RawMessage(routeProbeBody(route))
			}
			for _, token := range tokens {
				response := rig.serve(caller.presenting(t, route.Method, path, token.token, body))
				refusal := decodeBody[refusalAnswer](t, response)
				if response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired {
					t.Fatalf("%s under %s = %d %s, want 403 %s", name, token.name, response.Code, response.Body.String(), codeScopeRequired)
				}
				if challenge := response.Header().Get("WWW-Authenticate"); !strings.Contains(refusal.Error, model.ScopePerson) ||
					challenge != insufficientScopeChallenge(model.ScopePerson) {
					t.Fatalf("%s under %s refused %q with challenge %q, want the %s scope named in both",
						name, token.name, refusal.Error, challenge, model.ScopePerson)
				}
			}
		})
	}
	if probed == 0 {
		t.Fatal("no route's OAuthScopes name person: the walk held nothing to the rule")
	}
	t.Logf("%d routes' OAuthScopes name person", probed)
}

// The tests below are the approve-only token's decision of a credential request, a port of the
// approve-only token (main's #5239) into the account binding: it approves and denies as its
// person, and where the account binding holds it is bound to the Google account its credential
// records, which approverStanding compares with the one people records for the address after the
// address's lock. They run the real routed server, a real Postgres and a fake secrets broker that
// records what Dispatch relays to it.

const (
	approveOnlyDecisionIssuer  = "https://accounts.google.com"
	approveOnlyDecisionPerson  = "carol@example.com"
	approveOnlyDecisionAccount = "carol-account"
	approveOnlyDecisionRefresh = "carol-refresh"
	approveOnlyDecisionCode    = "ABCD-1234"
)

// approveOnlyDecisionRig is the routed server a decision is sent to: its deployment holds a Google
// rule (Deps.Standing) and records accounts (Deps.SignInIssuer), but for one
// newApproveOnlyDecisionRigOn builds with no sign-in issuer; its relay is a fake secrets broker
// that takes the approver fields, and carol is signed in under approveOnlyDecisionAccount.
type approveOnlyDecisionRig struct {
	handler  http.Handler
	database *store.Store
	broker   *ciphertokentest.StubBroker
	relay    *fakeBrokerRig
	caller   *brokerCaller
}

func newApproveOnlyDecisionRig(t *testing.T) *approveOnlyDecisionRig {
	t.Helper()
	return newApproveOnlyDecisionRigOn(t, approveOnlyDecisionIssuer)
}

// newApproveOnlyDecisionRigOn is newApproveOnlyDecisionRig on a deployment whose sign-in issuer is
// signInIssuer: "" is one whose sign-in records no accounts, where the account binding never holds
// (accountsHeld).
func newApproveOnlyDecisionRigOn(t *testing.T, signInIssuer string) *approveOnlyDecisionRig {
	t.Helper()
	relay := newFakeBrokerRig(t)
	broker := ciphertokentest.NewStubBroker(t)
	_, database, deps := newTestServer(t, testServerOptions{
		brokerTokens: stubBrokerTokens(t, broker), signInIssuer: signInIssuer, agentSecretsURL: relay.server.URL,
	})
	deps.Standing = &standingFake{}
	seedPeople(t, database, approveOnlyDecisionPerson)
	signInFor(t, database, approveOnlyDecisionPerson, approveOnlyDecisionAccount, approveOnlyDecisionRefresh, model.MemberFull, time.Minute)
	return &approveOnlyDecisionRig{handler: servedWith(t, deps), database: database, broker: broker, relay: relay, caller: newBrokerCaller(t)}
}

// approveOnlyDecisionAnswer is the broker's answer for carol's approve-only token, which records
// subject as its account (none for "").
func approveOnlyDecisionAnswer(jkt, subject string) ciphertoken.Introspection {
	answer := approveOnlyAnswer(jkt)
	answer.Person = approveOnlyDecisionPerson
	if subject != "" {
		answer.AccountIssuer, answer.AccountSubject = approveOnlyDecisionIssuer, subject
	}
	return answer
}

// decisionRequest is the request of action ("approve" or "deny") for credential request rec1 that
// carries token under DPoP, with body.
func (d *approveOnlyDecisionRig) decisionRequest(t *testing.T, action, token string, body any) *http.Request {
	t.Helper()
	return d.caller.presenting(t, http.MethodPost, "/api/v1/credential-requests/rec1/"+action, token, body)
}

// decide sends carol's decision of action under token, with the code she typed.
func (d *approveOnlyDecisionRig) decide(t *testing.T, action, token string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	d.handler.ServeHTTP(response, d.decisionRequest(t, action, token, map[string]string{"code": approveOnlyDecisionCode}))
	return response
}

// decideWhileBodyHeld sends the same decision as decide, with its client holding the body back
// until the token has been admitted and the handler waits on it, and runs whileHeld then: the
// decision's re-read of its caller comes after whileHeld.
func (d *approveOnlyDecisionRig) decideWhileBodyHeld(t *testing.T, action, token string, whileHeld func()) *httptest.ResponseRecorder {
	t.Helper()
	body := newHeldRequestBody(`{"code":"` + approveOnlyDecisionCode + `"}`)
	request := d.decisionRequest(t, action, token, nil)
	request.Header.Set("Content-Type", "application/json")
	request.Body, request.ContentLength = io.NopCloser(body), -1
	return serveWithBodyHeld(t, d.handler, request, body, whileHeld)
}

// relayedDecision is the body Dispatch relays for carol's decision that carries the approver and
// the code alone: every deny, and an approve to a broker whose settings answer without the approver
// fields.
func relayedDecision() string {
	return fmt.Sprintf(`{"approver":%q,"code":%q}`, approveOnlyDecisionPerson, approveOnlyDecisionCode)
}

// relayedApproval is the body Dispatch relays for carol's approve to a broker that takes the
// approver fields: the approver, the code, account under the issuer it was recorded with (neither
// for "", an approve bound to no account), and carol's session generation now.
func (d *approveOnlyDecisionRig) relayedApproval(t *testing.T, account string) string {
	t.Helper()
	var generation int64
	if err := d.database.Pool.QueryRow(t.Context(),
		`select coalesce((select generation from user_sessions where login = $1), 0)`, approveOnlyDecisionPerson).Scan(&generation); err != nil {
		t.Fatalf("read carol's session generation: %v", err)
	}
	if account == "" {
		return fmt.Sprintf(`{"approver":%q,"code":%q,"approver_session_generation":%d}`,
			approveOnlyDecisionPerson, approveOnlyDecisionCode, generation)
	}
	return fmt.Sprintf(`{"approver":%q,"code":%q,"approver_issuer":%q,"approver_subject":%q,"approver_session_generation":%d}`,
		approveOnlyDecisionPerson, approveOnlyDecisionCode, approveOnlyDecisionIssuer, account, generation)
}

// wantRelayed fails the test unless response is the secrets broker's 200 and the decision reached
// the broker once (before was its call count before the decision), as action's route, carrying
// want.
func (d *approveOnlyDecisionRig) wantRelayed(t *testing.T, response *httptest.ResponseRecorder, before int, action, want, what string) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200 from the secrets broker", what, response.Code, response.Body.String())
	}
	if relayed := d.relay.calls - before; relayed != 1 || d.relay.routes[len(d.relay.routes)-1] != "POST /v1/credential-requests/rec1/"+action {
		t.Fatalf("%s reached the secrets broker %d times, last as %v, want once, as its %s", what, relayed, d.relay.routes, action)
	}
	if d.relay.lastBody != want {
		t.Errorf("the secrets broker received %s for %s, want %s", d.relay.lastBody, what, want)
	}
}

// wantRefused fails the test unless response is the refusal status and code, and the decision
// reached the secrets broker neither as a call (before was its call count before the decision) nor
// as a read of its settings.
func (d *approveOnlyDecisionRig) wantRefused(t *testing.T, response *httptest.ResponseRecorder, before, status int, code, what string) {
	t.Helper()
	if refusal := decodeBody[refusalAnswer](t, response); response.Code != status || refusal.Code != code {
		t.Errorf("%s = %d %s, want %d %s", what, response.Code, response.Body.String(), status, code)
	}
	if d.relay.calls != before || d.relay.settingsReads != 0 {
		t.Errorf("%s reached the secrets broker %d times and read its settings %d times, want neither", what, d.relay.calls-before, d.relay.settingsReads)
	}
}

// TestAnApproveOnlyTokenApprovesAndDeniesAsItsPersonBoundToItsAccount (a port of #5239 into the
// account binding): carol's approve-only token, which records the account people records for her,
// approves and denies a machine login as carol. Dispatch relays the approve to the secrets broker
// with the approver carol, the code she typed, her account's issuer and `sub` and the session
// generation its re-read saw, which a broker that takes the approver fields records and checks,
// and the deny with the approver and the code alone, as it relays every deny.
//
// Controls (scratch only), each on its own: the first condition of refuseUnboundCredential without
// its `&& approveOnly == nil`, the refusal the mint and every other broker token get, makes each
// decision 403 OAUTH_SCOPE_REQUIRED with nothing reaching the broker, and this test fails on the
// status; approverStanding's `account = approveOnly.account` removed leaves the approve naming no
// account, and it fails on the approve's body.
func TestAnApproveOnlyTokenApprovesAndDeniesAsItsPersonBoundToItsAccount(t *testing.T) {
	t.Parallel()
	d := newApproveOnlyDecisionRig(t)
	const token = "carols-approve-only-token"
	d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
	for _, action := range []string{"approve", "deny"} {
		want := relayedDecision()
		if action == "approve" {
			want = d.relayedApproval(t, approveOnlyDecisionAccount)
		}
		before := d.relay.calls
		d.wantRelayed(t, d.decide(t, action, token), before, action, want, "carol's "+action+" by her approve-only token")
	}
}

// TestAnApproveOnlyTokenOfAnotherAccountDecidesNothing: where the account binding holds, an
// approve-only token whose account is not the one people records for carol, or records none,
// approves and denies nothing, and nothing reaches the secrets broker, not even a read of its
// settings. Six rows, each for the approve and the deny:
//
//   - a token recording another account is refused at its admission (401
//     BROKER_ACCOUNT_NOT_SIGNED_IN);
//   - a token recording no account, made long after people recorded carol's (so its admission, which
//     holds a token of no account to its creation time, lets it act), is refused by the decision
//     (401 BROKER_TOKEN_RECORDS_NO_ACCOUNT): no account Dispatch can name is no account to bind;
//   - a token of carol's own account, admitted, whose body is held back while another account signs
//     in under carol's address through a people store that records no broker revocation
//     (peopleOf), is refused 401 BROKER_ACCOUNT_NOT_SIGNED_IN when its body arrives: the decision's
//     re-read, under the address's lock, no longer finds its account recorded, and no revocation
//     names it;
//   - the same, with the replacing sign-in through a people store that records the broker
//     revocation of the account it replaces (store.WithBrokerRevocations under the rig's issuer, as
//     every process of a deployment with a broker builds it), is refused 401
//     BROKER_TOKEN_NOT_ACTIVE: that revocation names the token's account (brokerAccountNamed);
//   - the same token, held back while people comes to record carol's `sub` under another issuer,
//     is refused 401 BROKER_ACCOUNT_NOT_SIGNED_IN: the re-read compares the issuer too, and the
//     same `sub` of another issuer is another account. people.sub_issuer moves only together with
//     sub (migration 20261120165538's guard_people_binding_columns), and no store write records a
//     chosen `sub` under another issuer (a passkey's enrollment records the passkey's random user
//     handle), so the fixture writes the row directly, twice: sub to an interim account, then back
//     to carol's with the outside issuer and no refresh token, which people_sign_in_stands asks of
//     an account of another issuer;
//   - the same token, held back while carol is held, is refused 401 MEMBERSHIP_HELD.
//
// Controls (scratch only), each on its own: approverStanding's `case boundGone:` removed relays the
// approve and the deny of the third, fourth and fifth rows' tokens, the approve naming their
// account, and fails them on the status; its `boundHeld` arm removed fails the sixth; its
// `approveOnly.account.subject == ""` check removed fails the second on its code (the statement's
// comparison finds no account to match, and the decision is refused BROKER_ACCOUNT_NOT_SIGNED_IN
// instead); brokerAccountNamed handed the issuer, `$7::text`, for the subject in approverStanding's
// statement finds no revocation naming the token's account, and fails the fourth on its code,
// BROKER_ACCOUNT_NOT_SIGNED_IN; the statement's comparison of the coalesced sub_issuer with
// `$7::text` replaced by `$7::text is not null`, which compares the `sub` alone, relays the fifth
// row's decisions, and fails it on the status.
func TestAnApproveOnlyTokenOfAnotherAccountDecidesNothing(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"approve", "deny"} {
		t.Run(action+" at the admission and by the decision", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			var recordedAt time.Time
			if err := d.database.Pool.QueryRow(t.Context(), `select sub_recorded_at from people where email = $1`, approveOnlyDecisionPerson).Scan(&recordedAt); err != nil {
				t.Fatalf("read when people recorded carol's account: %v", err)
			}
			noAccount := approveOnlyDecisionAnswer(d.caller.jkt, "")
			noAccount.CredentialCreatedAt = recordedAt.Add(time.Hour).Unix()
			for _, row := range []struct {
				name, token string
				answer      ciphertoken.Introspection
				code        string
			}{
				{"a token recording another account", "a-token-of-another-account", approveOnlyDecisionAnswer(d.caller.jkt, "another-account"), "BROKER_ACCOUNT_NOT_SIGNED_IN"},
				{"a token recording no account", "a-token-of-no-account", noAccount, "BROKER_TOKEN_RECORDS_NO_ACCOUNT"},
			} {
				d.broker.Says(row.token, row.answer)
				before := d.relay.calls
				response := d.decide(t, action, row.token)
				d.wantRefused(t, response, before, http.StatusUnauthorized, row.code, "carol's "+action+" by "+row.name)
			}
		})
		t.Run(action+" after the account was replaced", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			const token = "carols-approve-only-token-replaced-while-held"
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			response := d.decideWhileBodyHeld(t, action, token, func() {
				signInFor(t, d.database, approveOnlyDecisionPerson, "replacing-account", "replacing-refresh", model.MemberFull, time.Minute)
			})
			d.wantRefused(t, response, 0, http.StatusUnauthorized, "BROKER_ACCOUNT_NOT_SIGNED_IN",
				"carol's "+action+" by her approve-only token, whose account was replaced while its body was held back")
		})
		t.Run(action+" after the account was replaced and its revocation recorded", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			const token = "carols-approve-only-token-revoked-while-held"
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			response := d.decideWhileBodyHeld(t, action, token, func() {
				revoking := store.NewPgPeopleStore(d.database.Pool, testSigningKey, nil, store.WithBrokerRevocations(approveOnlyDecisionIssuer))
				if err := revoking.SignInSubject(t.Context(), approveOnlyDecisionPerson, "replacing-account", "replacing-refresh", "", model.MemberFull); err != nil {
					t.Fatalf("sign carol in under the replacing account, recording the revocation of hers: %v", err)
				}
			})
			d.wantRefused(t, response, 0, http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE",
				"carol's "+action+" by her approve-only token, whose account was replaced and its revocation recorded while its body was held back")
		})
		t.Run(action+" after the address's sub moved to another issuer", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			const token = "carols-approve-only-token-of-another-issuer-while-held"
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			response := d.decideWhileBodyHeld(t, action, token, func() {
				if _, err := d.database.Pool.Exec(t.Context(), `update people set sub = 'carol-interim-account' where email = $1`, approveOnlyDecisionPerson); err != nil {
					t.Fatalf("record an interim account for carol: %v", err)
				}
				if _, err := d.database.Pool.Exec(t.Context(), `update people set sub = $2, sub_issuer = $3, refresh_token = null where email = $1`,
					approveOnlyDecisionPerson, approveOnlyDecisionAccount, "https://outside.example"); err != nil {
					t.Fatalf("record carol's sub under another issuer: %v", err)
				}
			})
			d.wantRefused(t, response, 0, http.StatusUnauthorized, "BROKER_ACCOUNT_NOT_SIGNED_IN",
				"carol's "+action+" by her approve-only token, whose sub people came to record under another issuer while its body was held back")
		})
		t.Run(action+" after carol was held", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			const token = "carols-approve-only-token-held-while-held"
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			response := d.decideWhileBodyHeld(t, action, token, func() {
				holdPerson(t, d.database, approveOnlyDecisionPerson, approveOnlyDecisionAccount, approveOnlyDecisionRefresh)
			})
			wantHeldRefusal(t, response, membershipHeldText, "carol's "+action+" by her approve-only token, held while its body was held back")
			if d.relay.calls != 0 || d.relay.settingsReads != 0 {
				t.Errorf("carol's %s, held while its body was held back, reached the secrets broker %d times and read its settings %d times, want neither",
					action, d.relay.calls, d.relay.settingsReads)
			}
		})
	}
}

// TestAnApproveOnlyTokenDecidesNamingNoAccountWhereTheBindingDoesNotHold: on a deployment whose
// sign-in records no accounts (Deps.SignInIssuer empty), where the account binding never holds
// (accountsHeld), carol's approve-only token approves and denies as carol, though its credential
// records her account, and Dispatch relays each as it relays a browser's and a personal token's
// there: the approve, to a broker that takes the approver fields, with the approver, the code and
// carol's session generation and no account, and the deny with the approver and the code alone.
// The same token, held back while carol is held, is refused 401 MEMBERSHIP_HELD all the same: the
// recorded hold is read whether or not the binding holds.
//
// Controls (scratch only), each on its own: approverStanding refusing an approve-only token where
// held is false (`if approveOnly != nil && !held` answering 403 OAUTH_SCOPE_REQUIRED, the refusal
// this replaced) fails the first row on its status; the hold column guarded by `$6::text is not
// null` again, where `$8::boolean` guards it, relays the held decisions, and fails the second row
// on its status.
func TestAnApproveOnlyTokenDecidesNamingNoAccountWhereTheBindingDoesNotHold(t *testing.T) {
	t.Parallel()
	const token = "carols-approve-only-token-where-no-account-is-recorded"
	t.Run("approve and deny", func(t *testing.T) {
		t.Parallel()
		d := newApproveOnlyDecisionRigOn(t, "")
		d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
		for _, action := range []string{"approve", "deny"} {
			want := relayedDecision()
			if action == "approve" {
				want = d.relayedApproval(t, "")
			}
			before := d.relay.calls
			d.wantRelayed(t, d.decide(t, action, token), before, action, want, "carol's "+action+" by her approve-only token on a deployment that records no accounts")
		}
	})
	for _, action := range []string{"approve", "deny"} {
		t.Run(action+" after carol was held", func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRigOn(t, "")
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			response := d.decideWhileBodyHeld(t, action, token, func() {
				holdPerson(t, d.database, approveOnlyDecisionPerson, approveOnlyDecisionAccount, approveOnlyDecisionRefresh)
			})
			wantHeldRefusal(t, response, membershipHeldText,
				"carol's "+action+" by her approve-only token on a deployment that records no accounts, held while its body was held back")
			if d.relay.calls != 0 || d.relay.settingsReads != 0 {
				t.Errorf("carol's %s, held while its body was held back, reached the secrets broker %d times and read its settings %d times, want neither",
					action, d.relay.calls, d.relay.settingsReads)
			}
		})
	}
}

// TestAnApproveOnlyTokensDecisionMeetsTheBrokersSettingsAsEveryCallersDoes: an approve-only
// token's approve names its account and generation only to a broker whose settings say it takes
// the approver fields, as every caller's approve does, and its deny reads no settings at all. Two
// rows, each a broker answering its settings as the row says, to which carol's token denies and
// then approves:
//
//   - settings that answer 200 without approver_fields, as a broker from before them answers: the
//     approve is relayed with the approver and the code alone, 200;
//   - settings that cannot be read (500): the approve is relayed to no broker, 503
//     AGENT_SECRETS_SETTINGS_UNREAD, as an approve by any other credential is;
//
// and under both the deny is relayed with the approver and the code alone, 200, reading no
// settings.
//
// Controls (scratch only), each on its own: approveCredentialRecord refusing an approve-only
// token's approve where fields is false (`if !fields && approveOnlyBindingOf(r, actor) != nil`
// answering 403 OAUTH_SCOPE_REQUIRED, the branch this replaced) fails the first row's approve on
// its status; approveCredentialRecord relaying where the settings read failed (its `if err != nil`
// branch after TakesApproverFields removed, so fields is false) relays the second row's approve,
// 200, and fails it on the status; denyCredentialRecord reading the settings first, answering
// writeApproverFieldsUnread where that fails, as the approve does, fails the second row's deny on
// its status, 503, and the first row's on the settings it read.
func TestAnApproveOnlyTokensDecisionMeetsTheBrokersSettingsAsEveryCallersDoes(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name           string
		settingsStatus int
		settingsBody   string
		status         int
		code           string
	}{
		{"a broker whose settings answer without approver_fields", 0, `{}`, http.StatusOK, ""},
		{"a broker whose settings cannot be read", http.StatusInternalServerError, ``, http.StatusServiceUnavailable, "AGENT_SECRETS_SETTINGS_UNREAD"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			d := newApproveOnlyDecisionRig(t)
			d.relay.settingsStatus, d.relay.settingsBody = row.settingsStatus, row.settingsBody
			const token = "carols-approve-only-token-for-a-broker-without-the-fields"
			d.broker.Says(token, approveOnlyDecisionAnswer(d.caller.jkt, approveOnlyDecisionAccount))
			d.wantRelayed(t, d.decide(t, "deny", token), 0, "deny", relayedDecision(), "carol's deny by her approve-only token, to "+row.name)
			if d.relay.settingsReads != 0 {
				t.Errorf("carol's deny by her approve-only token read the settings of %s %d times, want none", row.name, d.relay.settingsReads)
			}
			before := d.relay.calls
			approved := d.decide(t, "approve", token)
			if row.code == "" {
				d.wantRelayed(t, approved, before, "approve", relayedDecision(), "carol's approve by her approve-only token, to "+row.name)
			} else {
				if refusal := decodeBody[refusalAnswer](t, approved); approved.Code != row.status || refusal.Code != row.code {
					t.Errorf("carol's approve by her approve-only token, to %s = %d %s, want %d %s",
						row.name, approved.Code, approved.Body.String(), row.status, row.code)
				}
				if d.relay.calls != before {
					t.Errorf("carol's approve by her approve-only token reached %s %d times, want none", row.name, d.relay.calls-before)
				}
			}
			if d.relay.settingsReads == 0 {
				t.Errorf("carol's approve by her approve-only token read no settings of %s", row.name)
			}
		})
	}
}

// TestABrokerTokenThatIsNotApproveOnlyDecidesNothingWhateverItRecords: the account a token records
// does not make it an approver. A person-scoped broker token of carol's, which records her account
// as her sign-in's does, is given approve (brokerPersonScopes, as
// TestABrokerTokenGrantedApproveStillMintsAndDecidesNothing gives it) so the scope gate lets it
// through, and its approve and its deny are still 403 OAUTH_SCOPE_REQUIRED from the handler
// (refuseUnboundCredential), relayed to nobody: only the shape the verifier marks approve-only
// (ciphertoken.Caller.ApproveOnly) carries a binding to the decision, never a scope, an account or
// the presence of a bearer. It changes a package variable, so it does not run in parallel.
//
// Control (scratch only): brokerTokenActorBinding binding any token that holds approve (its
// `caller.ApproveOnly &&` removed) and approveOnlyBindingOf taking the binding for any credential
// (its `!approveOnlyRequest(r) ||` removed): the decisions are then relayed, and this test fails.
// Either change alone leaves the token refused, since each check stands behind the other.
func TestABrokerTokenThatIsNotApproveOnlyDecidesNothingWhateverItRecords(t *testing.T) {
	granted := brokerPersonScopes
	brokerPersonScopes = []string{model.ScopePerson, model.ScopeApprove}
	t.Cleanup(func() { brokerPersonScopes = granted })
	d := newApproveOnlyDecisionRig(t)
	const token = "carols-person-scoped-token-granted-approve"
	answer := personScopedTokenAnswer(d.caller.jkt)
	answer.Person = approveOnlyDecisionPerson
	answer.AccountIssuer, answer.AccountSubject = approveOnlyDecisionIssuer, approveOnlyDecisionAccount
	d.broker.Says(token, answer)
	for _, action := range []string{"approve", "deny"} {
		response := d.decide(t, action, token)
		d.wantRefused(t, response, 0, http.StatusForbidden, codeScopeRequired, "carol's "+action+" by a person-scoped broker token granted approve")
		if !strings.Contains(response.Body.String(), "a broker-issued token never mints a token or decides a credential request") {
			t.Errorf("carol's %s by a person-scoped broker token granted approve = %s, want the handler's own refusal", action, response.Body.String())
		}
	}
}

// TestARestrictedMembersApproveOnlyTokenDecidesNothing: a restricted member's approve-only token
// approves and denies nothing, and no call reaches the secrets broker. Such a token names no
// project (an approve-only token's shape), and a restricted member's broker token must list exactly
// one (restrictedBrokerTokenList), so it is refused 403 RESTRICTED_MEMBER at its resolution, and
// restrictedDecisionAllowed's own rule, which lets a restricted member decide a machine login of
// their own at their browser alone, is not reached through a token. Their browser's decision is
// the existing tests'.
//
// What it cannot hold: brokerTokenActorBinding's `case admission.Restricted`, which gives a
// restricted member's token no scope, and the binding's
// `actor.Credential.Holds(model.ScopeApprove)` guard both lie behind that refusal, which every
// approve-only token of a restricted member meets, since the verifier admits the approve-only
// shape only with no project (ciphertoken's shapeApproveOnly). Removing either leaves this test
// passing, and no row reaches them without stubbing restrictedBrokerTokenList out; they are a
// second line behind it.
//
// Control (scratch only): restrictedBrokerTokenList answering nil. The token is then admitted
// holding no scope (case admission.Restricted), the route's scope gate refuses it 403
// OAUTH_SCOPE_REQUIRED, and this test fails on the code.
func TestARestrictedMembersApproveOnlyTokenDecidesNothing(t *testing.T) {
	t.Parallel()
	relay := newFakeBrokerRig(t)
	stub := ciphertokentest.NewStubBroker(t)
	served, _ := newAccessServerWith(t, testServerOptions{
		admitsRestricted: true, brokerTokens: stubBrokerTokens(t, stub), agentSecretsURL: relay.server.URL,
	})
	if listed := dispatchRequest(t, served, http.MethodPut, "/api/v1/projects/ACC/access/people/"+accessErin, nil, "alice"); listed.Code != http.StatusOK {
		t.Fatalf("alice names erin on ACC's list: status=%d body=%s", listed.Code, listed.Body.String())
	}
	caller := newBrokerCaller(t)
	const token = "erins-approve-only-token"
	answer := approveOnlyAnswer(caller.jkt)
	answer.Person = accessErin
	stub.Says(token, answer)
	for _, action := range []string{"approve", "deny"} {
		response := httptest.NewRecorder()
		served.ServeHTTP(response, caller.presenting(t, http.MethodPost, "/api/v1/credential-requests/rec1/"+action, token,
			map[string]string{"code": approveOnlyDecisionCode}))
		if refusal := decodeBody[refusalAnswer](t, response); response.Code != http.StatusForbidden || refusal.Code != codeRestrictedMember {
			t.Errorf("erin's %s by her approve-only token = %d %s, want 403 %s", action, response.Code, response.Body.String(), codeRestrictedMember)
		}
		if relay.calls != 0 || relay.settingsReads != 0 {
			t.Errorf("erin's %s by her approve-only token reached the secrets broker %d times and read its settings %d times, want neither", action, relay.calls, relay.settingsReads)
		}
	}
}
