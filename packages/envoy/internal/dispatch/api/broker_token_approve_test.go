// broker_token_approve_test.go holds that no token the secrets broker issued holds the approve
// scope, whatever its project scope, so every route that needs approve refuses each of them at the
// gate, while the browser session of the person a token names is answered there as before. A
// broker token is made from a file on the machine that holds it (a machine login or a person's
// sign-in), so whoever copies that file holds it; approve settles what only the person settles, so
// it needs the person's browser or a personal token they chose to give it. The answer to an
// approval request checks approve inside its handler, and its tests are the broker rows of
// TestAnsweringAnApprovalAskNeedsTheApproveScope, on /api/v1 and on /api/v2, and of
// TestTheV2NoOpAnswerOfAnApprovalAskNeedsTheApproveScope, for the same approval sent again.
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
// person, refused for approve itself; one an operator's entry mapped to one project and one of a
// machine nobody signed in on, which hold no scope and are refused for person, the row's first;
// and a read-only one, which the read-only gate answers before any scope is read.
func approveTestTokens(jkt string) []approveTestToken {
	return []approveTestToken{
		{"a person-scoped broker token", "a-person-scoped-token", personScopedTokenAnswer(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopeApprove},
		{"a named broker token", "a-named-token", liveNamedToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
		{"a service-only broker token", "a-service-only-token", serviceOnlyToken(jkt), apicode.OAuthScopeRequired.V1Name(), model.ScopePerson},
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
		arm, named := route.Body.Arms[rule.Value]
		if rule.Where.In != partBody || member == "" || rule.Where.Name != member || !named {
			t.Fatalf("%s %s: its rule %s needs %s, and the walk probes only a rule on an arm of a union",
				route.Method, route.Pattern, rule, model.ScopeApprove)
		}
		members := map[string]any{}
		written, err := json.Marshal(arm)
		if err == nil {
			err = json.Unmarshal(written, &members)
		}
		if err != nil {
			t.Fatalf("%s %s: the arm %s writes no JSON object: %v", route.Method, route.Pattern, rule.Value, err)
		}
		members[member] = rule.Value
		encoded, err := json.Marshal(members)
		if err != nil {
			t.Fatalf("%s %s: encode the arm %s: %v", route.Method, route.Pattern, rule.Value, err)
		}
		return encoded
	}
	t.Fatalf("%s %s names %s on no scope and no rule", route.Method, route.Pattern, model.ScopeApprove)
	return nil
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
