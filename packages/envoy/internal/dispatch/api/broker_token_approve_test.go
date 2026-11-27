// broker_token_approve_test.go holds that no token the secrets broker issued for a machine to hold
// holds the approve scope, whatever its project scope, so every route that needs approve refuses
// each of them at the gate, while the browser session of the person a token names is answered
// there as before. The one broker-issued token that holds approve, the approve-only token the
// broker's proxy spends and no machine holds, is approve_only_token_test.go's. A broker token is
// made from a file on the machine that holds it (a machine login or a person's sign-in), so
// whoever copies that file holds it; approve settles what only the person settles, so it needs
// the person's browser, a personal token they chose to give it, or that approve-only token. The
// answer to an approval request checks approve inside its handler, and its tests are the broker
// rows of TestAnsweringAnApprovalAskNeedsTheApproveScope, on /api/v1 and on /api/v2, and of
// TestTheV2NoOpAnswerOfAnApprovalAskNeedsTheApproveScope, for the same approval sent again. A
// job's token does not hold person either, since it names no person, so every route that needs
// person refuses it at the same gate.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
		{"a person-scoped broker token", "a-person-scoped-token", personScopedTokenAnswer(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopeApprove},
		{"a named broker token", "a-named-token", liveNamedToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
		{"a service-only broker token", "a-service-only-token", serviceOnlyToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
		{"a job's named broker token", "a-job-named-token", liveJobNamedToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
		{"a job's public broker token", "a-job-public-token", liveJobPublicToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
		{"a read-only broker token", "a-read-only-token", readOnlyToken(jkt), contracts.DispatchReadOnlyToken, ""},
	}
}

// approveRows is every row of the server's own route table that needs approve: its OAuth scopes
// name it, or, on /api/v2, a rule of its Authorities does, which one value of the request needs
// (authorities_v2.go). The rows are read from the table, never listed here, so a row added later
// is held to the rule too.
func approveRows(served *server) []apiRoute {
	var rows []apiRoute
	for _, route := range served.routes() {
		if slices.Contains(route.OAuthScopes, model.ScopeApprove) || slices.ContainsFunc(route.Authorities, func(rule authorityRule) bool {
			return slices.Contains(rule.Scopes, model.ScopeApprove)
		}) {
			rows = append(rows, route)
		}
	}
	return rows
}

// approveProbeBody is the body a probe of route sends. A row whose own scopes name approve refuses
// before its body is read, so routeProbeBody's serves it. A /api/v2 row where one value needs
// approve refuses only once its decoder has read that value (gateV2Authorities), so its probe is
// the arm of its union the first such rule names, as that arm's type writes its zero value. A rule
// of any other kind fails t rather than leave the row probed with a body that never meets it.
func approveProbeBody(t *testing.T, route apiRoute) json.RawMessage {
	t.Helper()
	if slices.Contains(route.OAuthScopes, model.ScopeApprove) {
		return json.RawMessage(routeProbeBody(route))
	}
	for _, rule := range route.Authorities {
		if !slices.Contains(rule.Scopes, model.ScopeApprove) {
			continue
		}
		member := unionMember(route.Body)
		if _, named := route.Body.Arms[rule.Value]; rule.Where.In != partBody || member == "" || rule.Where.Name != member || !named {
			t.Fatalf("%s %s: its rule %s needs %s, and the walk probes only a rule on an arm of a union",
				route.Method, route.Pattern, rule, model.ScopeApprove)
		}
		return unionArmBody(t, route, rule.Value)
	}
	t.Fatalf("%s %s names %s on no scope and no rule", route.Method, route.Pattern, model.ScopeApprove)
	return nil
}

// unionArmBody is the body the decoder of route's union reads as the arm named arm: that arm's type
// written as its zero value, so every member the arm declares, its required ones among them, is
// there, with the member the union is picked by naming the arm.
func unionArmBody(t *testing.T, route apiRoute, arm string) json.RawMessage {
	t.Helper()
	members := map[string]any{}
	written, err := json.Marshal(route.Body.Arms[arm])
	if err == nil {
		err = json.Unmarshal(written, &members)
	}
	if err != nil {
		t.Fatalf("%s %s: the arm %s writes no JSON object: %v", route.Method, route.Pattern, arm, err)
	}
	members[unionMember(route.Body)] = arm
	encoded, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("%s %s: encode the arm %s: %v", route.Method, route.Pattern, arm, err)
	}
	return encoded
}

// newCredentialRelayRig is newBrokerTokenRig with the credential routes relaying to a fake secrets
// broker, which records every call Dispatch relays to it.
func newCredentialRelayRig(t *testing.T) (*fakeBrokerRig, *brokerTokenRig) {
	t.Helper()
	credentials := newFakeBrokerRig(t)
	return credentials, newBrokerTokenRigOn(t, testServerOptions{agentSecretsURL: credentials.server.URL})
}

// TestNoBrokerTokenGetsApprove: each row of the route table that needs approve, by its own scopes
// or, on /api/v2, by the one value of its body that needs it, refuses every kind of broker token
// 403 before its handler runs, the token that acts as its person refused for approve itself, and
// nothing reaches the secrets broker's decision routes. The same person's browser session is not
// refused for its credential on any row. The rows are the table's own; a walk that found none, or
// missed a row a stolen machine file would use (a credential approve, a machine login's revoke, a
// personal token's mint, a decision's Keep, each at /api/v1 and at the /api/v2 route that answers
// it), fails rather than passes: /api/v2 grants nothing /api/v1 refuses.
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
		"PUT /api/v2/credential-requests/{id}/state",
		"POST /api/v1/machine-logins/{id}/revoke",
		"DELETE /api/v2/machine-logins/{id}",
		"POST /api/v1/me/agent-tokens",
		"POST /api/v2/me/agent-tokens",
		"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
		"PUT /api/v2/issues/{key}/decisions/{block_id}/state",
	} {
		if !slices.Contains(names, want) {
			t.Fatalf("the route table's approve rows %v do not hold %s: the walk below cannot hold it to the rule", names, want)
		}
	}
	t.Logf("the route table names approve on %d rows: %v", len(rows), names)

	for _, route := range rows {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			path := routeProbePath(route.Pattern)
			body := approveProbeBody(t, route)
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
				if response.Code != http.StatusForbidden || refusal.Code != apicode.OAuthScopeRequired.V1Name() {
					t.Fatalf("%s under %s = %d %s, want 403 %s", name, token.name, response.Code, response.Body.String(), apicode.OAuthScopeRequired.V1Name())
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
