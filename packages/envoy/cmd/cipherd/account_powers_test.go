// account_powers_test.go drives the REAL compiled broker with four accounts in
// BROKER_API_ACCOUNTS — one calling as dispatch, one as listener, one as access-review, one as
// session-watch — over HTTP, on a real Postgres, against a local OpenID issuer standing in for the
// cluster's. Being listed is not being allowed: the caller's name says which of the broker's
// fifteen named-caller routes that account reaches, so the introspection-only caller is refused
// every route Dispatch's server relays a person's decisions through, the review caller reads the
// grant log and is refused the other fourteen, the session watch reads the private list and is
// refused the other fourteen, and the dispatch caller reaches all thirteen of its own and is
// refused the review's and the watch's. Nothing here is a unit test — the binary is the one
// cmd/cipherd builds and every request crosses a socket.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// listenerAccount is the second listed ServiceAccount, calling as listener: the Envoy
	// listener, which reads one of the broker's access tokens back and relays no person's
	// decision. reviewAccount is the third, calling as access-review: the identity a quarterly
	// access review reads the broker's audit trail as, which decides nothing. watchAccount is the
	// fourth, calling as session-watch: the shared session watch, which reads the broker's list of
	// private projects and nothing else. apiAccount (token_exchange_test.go) is the first, calling
	// as dispatch.
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	reviewAccount   = "system:serviceaccount:example-ns:example-access-review"
	watchAccount    = "system:serviceaccount:example-ns:example-session-watch"
	// The four caller names those accounts are listed under, which the broker names in a
	// refusal and this test drives every route from.
	dispatchCaller = "dispatch"
	listenerCaller = "listener"
	reviewCaller   = "access-review"
	watchCaller    = "session-watch"
	// introspection is the one route two names carry: the listener's own, which dispatch
	// reaches too, since the name that relays a person's decisions reads a token back as well.
	// grantLogRoute is the review's own, which carries a closed period because the review
	// caller's answer there is a page and not a refusal. privateListRoute is the watch's own.
	introspection    = "/v1/oauth/introspect"
	grantLogRoute    = "/v1/grant-log"
	grantLogQuery    = grantLogRoute + "?since=2026-01-01T00:00:00Z&until=2026-01-02T00:00:00Z"
	privateListRoute = "/v1/private-projects"
	// powersApprover is the person a relayed decision would name, and unknownID an id no record,
	// machine login or grant has: the routes under test are reached, never satisfied, so the
	// answer this test reads is the authority's and not a decision's.
	powersApprover = "ada@example.com"
	unknownID      = "00000000-0000-4000-8000-000000000000"
	// unknownRecord is that same id nothing has, in a record id's own shape: a lowercase-hex
	// sha256 hash, so the record routes the caller reaches answer for the record (404) rather than
	// refusing the id's shape (400); every route refuses a caller without its power first, and
	// takes either shape for that.
	unknownRecord = "0000000000000000000000000000000000000000000000000000000000000000"
	// lacksPower is the refusal a listed caller gets on a route its name does not carry, as
	// against ACCOUNT_NOT_LISTED for an account the broker does not list at all.
	lacksPower = "ACCOUNT_LACKS_POWER"
)

// accountRoute is one route accountAuth authenticates, with a request that reaches its handler:
// pattern is what the broker names in a refusal, path what the test calls, and status, key and
// value the answer the route's own caller gets.
type accountRoute struct {
	method, pattern, path, contentType, body string
	// caller is the listed caller the route belongs to: the name whose own powers carry it.
	// Every other listed caller is refused it, except that dispatch reaches introspection
	// too, since the name that relays a person's decisions reads a token back as well.
	caller string
	// status is the answer's status for the route's own caller, and key and value one member
	// of its body that only that answer carries: a refusal's code, an OAuth error's error, or
	// a list.
	status int
	key    string
	value  any
}

// emptyList is a list route's answer on a broker that holds nothing, as JSON decodes it.
var emptyList = []any{}

// accountRoutes is one row for each route the broker's route table authenticates with
// accountAuth (TestEveryAccountRouteHasARow holds the two to each other). This broker holds no
// record, machine login, grant or sign-in, and asks no Dispatch, so each answer is the route's own
// for nothing found: a list route's empty list, a decision's 404 for an id nothing has. Holding
// each row to its own answer, rather than to "not the authority's refusal", is what makes a 401
// or a 500 on the way to the handler a failure here.
var accountRoutes = []accountRoute{
	// Introspection answers about the token, and the token nobody issued is simply not live.
	{method: http.MethodPost, pattern: introspection, path: introspection,
		contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued", caller: listenerCaller,
		status: http.StatusOK, key: "active", value: false},
	// The revocation body names no client, so the dispatch caller it reaches is refused by
	// the provider's own client authentication (RFC 6749's invalid_client) rather than
	// answered 200 with an empty body: what this walk reads is the authority, and callBroker
	// reads a JSON answer.
	{method: http.MethodPost, pattern: "/v1/oauth/revoke", path: "/v1/oauth/revoke",
		contentType: "application/x-www-form-urlencoded", body: "token=a-token-nobody-issued", caller: dispatchCaller,
		status: http.StatusUnauthorized, key: "error", value: "invalid_client"},
	{method: http.MethodGet, pattern: "/v1/pending", path: "/v1/pending?approver=" + powersApprover, caller: dispatchCaller,
		status: http.StatusOK, key: "pending", value: emptyList},
	// The three decisions on a record id nothing has: writeNoSuchRecord.
	{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/approve", path: "/v1/credential-requests/" + unknownRecord + "/approve",
		contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/deny", path: "/v1/credential-requests/" + unknownRecord + "/deny",
		contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	{method: http.MethodPost, pattern: "/v1/credential-requests/{record}/dismiss", path: "/v1/credential-requests/" + unknownRecord + "/dismiss",
		contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	// No pending machine login carries the code.
	{method: http.MethodPost, pattern: "/v1/machine-logins/lookup", path: "/v1/machine-logins/lookup",
		contentType: "application/json", body: `{"code":"EXAM-PLE1"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NO_SUCH_CODE"},
	{method: http.MethodGet, pattern: "/v1/launcher-credentials", path: "/v1/launcher-credentials?approver=" + powersApprover, caller: dispatchCaller,
		status: http.StatusOK, key: "credentials", value: emptyList},
	// enroll.ErrNoCredential: no machine login has the id.
	{method: http.MethodPost, pattern: "/v1/launcher-credentials/{id}/revoke-by-approver", path: "/v1/launcher-credentials/" + unknownID + "/revoke-by-approver",
		contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	{method: http.MethodGet, pattern: "/v1/grants", path: "/v1/grants?approver=" + powersApprover, caller: dispatchCaller,
		status: http.StatusOK, key: "grants", value: emptyList},
	{method: http.MethodPost, pattern: "/v1/grants/{id}/revoke-by-approver", path: "/v1/grants/" + unknownID + "/revoke-by-approver",
		contentType: "application/json", body: `{"approver":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	{method: http.MethodGet, pattern: "/v1/person-signins", path: "/v1/person-signins?person=" + powersApprover, caller: dispatchCaller,
		status: http.StatusOK, key: "signins", value: emptyList},
	// store.ErrNoSignIn: the person has no sign-in of that id.
	{method: http.MethodPost, pattern: "/v1/person-signins/{id}/revoke", path: "/v1/person-signins/" + unknownID + "/revoke",
		contentType: "application/json", body: `{"person":"` + powersApprover + `"}`, caller: dispatchCaller,
		status: http.StatusNotFound, key: "code", value: "NOT_FOUND"},
	// The review's own route, called for a period that closed long ago, so the answer the
	// review gets here is a page of the grant log, empty on a broker nothing has happened on
	// yet, rather than a refusal of its period.
	{method: http.MethodGet, pattern: grantLogRoute, path: grantLogQuery, caller: reviewCaller,
		status: http.StatusOK, key: "events", value: emptyList},
	// The watch's own route, on a broker that wires no list to it: a list it cannot read is no
	// list, which is the route's own answer and not a refusal of the caller.
	{method: http.MethodGet, pattern: privateListRoute, path: privateListRoute, caller: watchCaller,
		status: http.StatusServiceUnavailable, key: "code", value: "PRIVATE_LIST_UNKNOWN"},
}

// TestAListedAccountReachesOnlyItsOwnRoutes lists four callers and drives every route
// accountAuth authenticates from each of them. The listener caller, listed for introspection
// alone, is refused each of the twelve relay routes — approve, deny, dismiss, both
// revoke-by-approver, the RFC 7009 token revocation and both person sign-in routes among them —
// the grant log and the private list too, 403 ACCOUNT_LACKS_POWER naming itself and the route, and
// still reads a token back. The access-review caller reads the grant log and is refused the other
// fourteen the same way, and the session-watch caller reaches the private list, on a broker that
// wires no list to it its 503 PRIVATE_LIST_UNKNOWN, and is refused the other fourteen.
// The dispatch caller reaches all thirteen of its own, the introspection route included, and is
// refused the grant log and the private list: neither the broker's audit trail nor the list of
// private projects is another of the powers the name that decides already holds.
func TestAListedAccountReachesOnlyItsOwnRoutes(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster := oidctest.New(t)
	clusterKey := cluster.PublishKey(t, "signing-key")
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + cluster.URL(),
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_API_ACCOUNTS=dispatch=" + apiAccount + " listener=" + listenerAccount + " access-review=" + reviewAccount + " session-watch=" + watchAccount,
		// No Dispatch, whatever the environment the test runs in sets, so each answer below is the
		// one a broker that asks no Dispatch gives. Each is the last of its key, which is the value
		// exec gives the child.
		"BROKER_PROJECT_ACCESS_URL=",
		"BROKER_PROJECT_ACCESS_TOKEN_FILE=",
	}})
	live := newPlainBroker(broker.Addr)
	// Each listed caller: the name the broker knows it by and that account's own token.
	callers := []struct{ name, token string }{
		{dispatchCaller, accountToken(t, cluster, clusterKey, apiAccount)},
		{listenerCaller, accountToken(t, cluster, clusterKey, listenerAccount)},
		{reviewCaller, accountToken(t, cluster, clusterKey, reviewAccount)},
		{watchCaller, accountToken(t, cluster, clusterKey, watchAccount)},
	}

	for _, route := range accountRoutes {
		for _, who := range callers {
			t.Run("as "+who.name+": "+route.method+" "+route.pattern, func(t *testing.T) {
				status, body := live.callAs(t, route.method, route.path, who.token, route.contentType, route.body)
				if who.name != route.caller && !(route.pattern == introspection && who.name == dispatchCaller) {
					if status != http.StatusForbidden || body["code"] != lacksPower {
						t.Fatalf("%s %s as %s = %d %v, want 403 %s: a listed caller reaches the routes its own name carries and no others",
							route.method, route.pattern, who.name, status, body, lacksPower)
					}
					refusal, _ := body["error"].(string)
					for _, want := range []string{who.name, route.method + " " + route.pattern} {
						if !strings.Contains(refusal, want) {
							t.Errorf("the refusal reads %q, which does not name %q: it must say which caller was refused which route", refusal, want)
						}
					}
					if strings.Contains(refusal, who.token) {
						t.Error("the refusal repeats the caller's ServiceAccount token")
					}
					return
				}
				if status != route.status || !reflect.DeepEqual(body[route.key], route.value) {
					t.Fatalf("%s %s as %s = %d %v, want %d with %s %v: the route's own answer for its own caller",
						route.method, route.pattern, who.name, status, body, route.status, route.key, route.value)
				}
			})
		}
	}
}

// callAs sends one request to the broker as the caller whose ServiceAccount token is bearer, and
// answers the status and the decoded body, the caller's token named in the transcript rather than
// repeated.
func (l *liveBroker) callAs(t *testing.T, method, path, bearer, contentType, body string) (int, map[string]any) {
	t.Helper()
	status, answer := l.send(t, l.plain, brokerCall{
		method: method, path: path, contentType: contentType,
		body: body, bearer: bearer, shown: body + " (as <account-token>)",
	})
	var decoded map[string]any
	if err := json.Unmarshal(answer, &decoded); err != nil {
		t.Fatalf("%s %s answered %d %s, which is not JSON: %v", method, path, status, answer, err)
	}
	return status, decoded
}
