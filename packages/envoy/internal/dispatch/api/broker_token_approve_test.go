// broker_token_approve_test.go holds that no token the secrets broker issued holds the approve
// scope, whatever its project scope, so every route that needs approve refuses each of them, while
// the browser session of the person a token names, and a personal token that person gave approve,
// are answered there as before. A broker token is made from a file on the machine that holds it (a
// machine login or a person's sign-in), so whoever copies that file holds it; approve settles what
// only the person settles, so it needs the person's browser or a personal token they chose to give
// it.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
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

// personScopedTokenAnswer is the broker's answer for a dev box session's token: live, bound to jkt,
// acting as its person (the project scope person) and naming no project. It is what a person's
// sign-in and a person's machine login both trade for.
func personScopedTokenAnswer(jkt string) ciphertoken.Introspection {
	answer := liveNamedToken(jkt)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
	return answer
}

// approveTestTokens is every kind of token the broker mints for a session: one acting as its
// person, refused for approve itself; one an operator's entry mapped to one project and one of a
// machine nobody signed in on, which hold no scope and are refused for person, the row's first;
// and a read-only one, which the read-only gate answers before any scope is read.
func approveTestTokens(jkt string) []approveTestToken {
	return []approveTestToken{
		{"a person-scoped broker token", "a-person-scoped-token", personScopedTokenAnswer(jkt), codeScopeRequired, model.ScopeApprove},
		{"a named broker token", "a-named-token", liveNamedToken(jkt), codeScopeRequired, model.ScopePerson},
		{"a service-only broker token", "a-service-only-token", serviceOnlyToken(jkt), codeScopeRequired, model.ScopePerson},
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
	return credentials, newBrokerTokenRigOn(t, testServerOptions{
		agentSecretsURL:       credentials.server.URL,
		agentSecretsTokenFile: accountTokenFile(t, credentialTestAccountToken),
	})
}

// TestNoBrokerTokenGetsApprove: each row of the route table that needs approve refuses every kind
// of broker token 403 before its handler runs, the token that acts as its person refused for
// approve itself, and nothing reaches the secrets broker's decision routes. The same person's
// browser session is not refused for its credential on any row. The rows are the table's own; a
// walk that found none, or missed a row a stolen machine file would use (a credential approve, a
// machine login's revoke, a personal token's mint, a decision's Keep), fails rather than passes.
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

// approveCall is one call a stolen machine file would make with the approve scope, and what it
// answers a credential that holds that scope.
type approveCall struct {
	name    string
	method  string
	path    string
	body    map[string]any
	success int
	// relayed is the body Dispatch sends the secrets broker for a credential holding approve, the
	// test's person named as the approver; empty for a call Dispatch answers itself.
	relayed string
}

// approveCalls is a credential approve, a machine login's revoke and a personal token's mint.
var approveCalls = []approveCall{
	{
		name: "a credential approve", method: http.MethodPost, path: "/api/v1/credential-requests/rec1/approve",
		body: map[string]any{"code": "ABCD-1234"}, success: http.StatusOK,
		relayed: `{"approver":"alice","code":"ABCD-1234"}`,
	},
	{
		name: "a machine login's revoke", method: http.MethodPost, path: "/api/v1/machine-logins/cred1/revoke",
		body: map[string]any{}, success: http.StatusOK, relayed: `{"approver":"alice"}`,
	},
	{
		name: "a personal token's mint", method: http.MethodPost, path: "/api/v1/me/agent-tokens",
		body: map[string]any{"name": "minted"}, success: http.StatusCreated,
	},
}

// personalTokenCount is how many personal tokens login holds.
func personalTokenCount(t *testing.T, rig *brokerTokenRig, login string) int {
	t.Helper()
	var count int
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select count(*) from agent_tokens where owner = $1`, login).Scan(&count); err != nil {
		t.Fatalf("count %s's personal tokens: %v", login, err)
	}
	return count
}

// TestAPersonScopedBrokerTokenCannotApproveRevokeOrMint: the token a dev box session acts as its
// person with is refused 403 OAUTH_SCOPE_REQUIRED, naming approve, on a credential approve, a
// machine login's revoke and a personal token's mint. The secrets broker is asked nothing and no
// personal token is made.
func TestAPersonScopedBrokerTokenCannotApproveRevokeOrMint(t *testing.T) {
	credentials, rig := newCredentialRelayRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-token-that-would-approve"
	rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
	held := personalTokenCount(t, rig, brokerTestPerson)

	for _, call := range approveCalls {
		t.Run(call.name, func(t *testing.T) {
			response := rig.serve(caller.presenting(t, call.method, call.path, token, call.body))
			refusal := decodeBody[refusalAnswer](t, response)
			if response.Code != http.StatusForbidden || refusal.Code != codeScopeRequired || !strings.Contains(refusal.Error, model.ScopeApprove) {
				t.Fatalf("%s under a person-scoped broker token = %d %s, want 403 %s naming approve",
					call.name, response.Code, response.Body.String(), codeScopeRequired)
			}
		})
	}
	if credentials.calls != 0 {
		t.Fatalf("the secrets broker was asked %d times, want none: the refusal comes before any relay", credentials.calls)
	}
	if now := personalTokenCount(t, rig, brokerTestPerson); now != held {
		t.Fatalf("%s holds %d personal tokens after the refused mint, want %d", brokerTestPerson, now, held)
	}
}

// TestTheBrowserOfABrokerTokensPersonStillApprovesRevokesAndMints: the person the token above acts
// for makes the same three calls from their browser session and is answered as before: the
// approve and the revoke relayed to the secrets broker naming that person as the approver, and the
// mint answered with its token.
func TestTheBrowserOfABrokerTokensPersonStillApprovesRevokesAndMints(t *testing.T) {
	credentials, rig := newCredentialRelayRig(t)
	for _, call := range approveCalls {
		t.Run(call.name, func(t *testing.T) {
			response := rig.asPerson(t, call.method, call.path, call.body)
			requireApproveCallAnswered(t, credentials, call, response)
		})
	}
}

// TestAPersonalTokenWithApproveStillApprovesWhereABrokerTokenCannot: a personal token its person
// gave approve makes the same three calls and is answered as the browser is.
func TestAPersonalTokenWithApproveStillApprovesWhereABrokerTokenCannot(t *testing.T) {
	credentials, rig := newCredentialRelayRig(t)
	personal := personalTokenOwnedBy(t, rig.database, brokerTestPerson, model.ScopePerson, model.ScopeApprove)
	for _, call := range approveCalls {
		t.Run(call.name, func(t *testing.T) {
			response := agentRequest(t, rig.handler, call.method, call.path, call.body, personal)
			requireApproveCallAnswered(t, credentials, call, response)
		})
	}
}

// requireApproveCallAnswered fails t unless response is call's success, and a relayed call reached
// the secrets broker naming the test's person as the approver.
func requireApproveCallAnswered(t *testing.T, credentials *fakeBrokerRig, call approveCall, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != call.success {
		t.Fatalf("%s = %d %s, want %d", call.name, response.Code, response.Body.String(), call.success)
	}
	if call.relayed != "" && credentials.lastBody != call.relayed {
		t.Fatalf("%s sent the secrets broker %s, want %s", call.name, credentials.lastBody, call.relayed)
	}
}

// TestABrokerTokenCannotAnswerAnApprovalRequest: answering an approval request approves the
// document it names, which needs approve beside the person scope the answer route declares
// (answerAsk), so the token that acts as its person is refused 403 OAUTH_SCOPE_REQUIRED naming
// approve and the request stays open, while the same person's browser session answers it. The same
// token still answers an ordinary question, which needs the person scope alone.
func TestABrokerTokenCannotAnswerAnApprovalRequest(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const token = "a-dev-box-token-that-would-answer-an-approval"
	rig.broker.Says(token, personScopedTokenAnswer(caller.jkt))
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "Approved only by its person", "spec": "# Spec\n\nA passage.",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	requested := sessionRequest(t, rig.handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	approval := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	answer := map[string]any{"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": nil}

	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+approval+"/answer", token, answer))
	if refusal := decodeBody[refusalAnswer](t, refused); refused.Code != http.StatusForbidden ||
		refusal.Code != codeScopeRequired || !strings.Contains(refusal.Error, model.ScopeApprove) {
		t.Fatalf("a person-scoped broker token answering an approval request = %d %s, want 403 %s naming approve",
			refused.Code, refused.Body.String(), codeScopeRequired)
	}
	var state string
	if err := rig.database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, approval).Scan(&state); err != nil {
		t.Fatalf("read the approval request's state: %v", err)
	}
	if state != "open" {
		t.Fatalf("the approval request is %q after the refusal, want it still open", state)
	}

	question := openAskAs(t, rig.handler, issue.Key, "session-asking", "Which option?")
	if answered := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/asks/"+question+"/answer", token, map[string]any{
		"selected": []string{"Yes"}, "expected_edited_at": nil,
	})); answered.Code != http.StatusOK {
		t.Fatalf("the same token answering an ordinary question = %d %s, want 200", answered.Code, answered.Body.String())
	}

	if approved := rig.asPerson(t, http.MethodPost, "/api/v1/asks/"+approval+"/answer", answer); approved.Code != http.StatusOK {
		t.Fatalf("%s's browser answering the approval request = %d %s, want 200", brokerTestPerson, approved.Code, approved.Body.String())
	}
}
