// issuer_retry_after_test.go drives the answer to a token nobody judged, on the same real broker
// api_test.go mounts: 503 OIDC_ISSUER_UNAVAILABLE with Retry-After: 1, the Envoy listener's
// answer to the same fault, on every route that checks a ServiceAccount token or a listed caller's
// Google identity token, while a token the broker judged and refused is answered as it was, with
// no Retry-After.
package api_test

import (
	"context"
	"crypto/ecdsa"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// issuerUnavailableRetryAfter is the Retry-After a token nobody judged is answered with: one
// second, the Envoy listener's for the same fault.
const issuerUnavailableRetryAfter = "1"

// reqAnswer is req with the whole answer, its headers too, which req does not return.
func (ts *testServer) reqAnswer(t *testing.T, method, path string, headers map[string]string, body any) answered {
	t.Helper()
	all := map[string]string{}
	maps.Copy(all, headers)
	var reader io.Reader
	if body != nil {
		reader = jsonBody(t, body)
		all["Content-Type"] = "application/json"
	}
	return send(t, http.DefaultClient, request(t, method, ts.URL+path, reader, all))
}

// launcherAnswer is launcher with the whole answer.
func (ts *testServer) launcherAnswer(t *testing.T, key *ecdsa.PrivateKey, launcherID, method, path string, body any) answered {
	t.Helper()
	signed, err := proof.SignLauncher(key, launcherID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.SignLauncher: %v", err)
	}
	return ts.reqAnswer(t, method, path, map[string]string{"Proof": signed}, body)
}

// sessionAnswer is session with the whole answer, for a path that carries no query.
func (ts *testServer) sessionAnswer(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any) answered {
	t.Helper()
	signed, err := proof.Sign(key, enrollmentID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return ts.reqAnswer(t, method, path, map[string]string{"Proof": signed}, body)
}

// requireUnjudged fails the test unless got is the answer to a token nobody judged: 503
// OIDC_ISSUER_UNAVAILABLE with Dispatch's sentence and Retry-After: 1.
func requireUnjudged(t *testing.T, got answered) {
	t.Helper()
	refusal := decode[wireError](t, got.body)
	if got.status != http.StatusServiceUnavailable || refusal.Code != oidc.IssuerUnavailableCode || refusal.Error != issuerUnavailableMessage {
		t.Fatalf("answered %d %s, want 503 %s %q", got.status, got.body, oidc.IssuerUnavailableCode, issuerUnavailableMessage)
	}
	if retry := got.header.Get("Retry-After"); retry != issuerUnavailableRetryAfter {
		t.Fatalf("the 503 carries Retry-After %q, want %q, the Envoy listener's wait for the same fault", retry, issuerUnavailableRetryAfter)
	}
}

// requireJudged fails the test unless got is the refusal status and code of a token the broker
// judged, with no Retry-After: no wait changes that answer.
func requireJudged(t *testing.T, got answered, status int, code string) {
	t.Helper()
	if refusal := decode[wireError](t, got.body); got.status != status || refusal.Code != code {
		t.Fatalf("answered %d %s, want %d %s", got.status, got.body, status, code)
	}
	if retry := got.header.Get("Retry-After"); retry != "" {
		t.Fatalf("the refusal carries Retry-After %q, want none", retry)
	}
}

// TestATokenNobodyJudgedNamesItsWaitOnEveryRoute: while the broker cannot read the second issuer
// — its discovery document, or its key set — a token of that issuer is answered 503
// OIDC_ISSUER_UNAVAILABLE with Retry-After: 1 at enrollment, renewal, the token login, the
// pod-session registration's body token, a route of the listed caller, the service route and
// the edge listener's GET /v1/project-repositories. A token of the first issuer signed by a key
// that issuer never published is one the broker judged: each route refuses it as before, and
// names no wait.
func TestATokenNobodyJudgedNamesItsWaitOnEveryRoute(t *testing.T) {
	both := []string{"first", "second"}
	for _, fault := range []struct {
		name  string
		apply func(*oidctest.Issuer)
	}{
		{"discovery document", func(issuer *oidctest.Issuer) { issuer.FailDiscovery(http.StatusServiceUnavailable) }},
		{"key set", func(issuer *oidctest.Issuer) { issuer.FailKeys(http.StatusServiceUnavailable) }},
	} {
		t.Run("its "+fault.name+" cannot be read", func(t *testing.T) {
			second := oidctest.New(t)
			secondKey := second.PublishKey(t, "second-key")
			fault.apply(second)
			ts := newTestServerWith(t, func(d *api.Deps) {
				withNamedIssuers(second, both)(d)
				d.EdgeListeners = edgeListeners(t, "edge-listener/edge-a/PRIV="+edgeAccount+"=first,second", d.Enroll.Issuers)
			})
			stray := oidctest.New(t).PublishKey(t, "stray-key")
			credentialID, key := ts.mintServiceLauncherCredential(t)
			podUID := uuid.NewString()
			enrollmentID, podKey := ts.enrolledPod(t, key, credentialID, podUID, ts.podToken(t, workerAccount, podUID))
			registrar := ts.accountToken(t, daemonAccount, "", time.Hour)

			for _, route := range []struct {
				name string
				// call presents tokenOf(account, podUID), a token of that account, bound to that
				// pod where podUID is not "".
				call         func(tokenOf func(account, podUID string) string) answered
				status       int
				code         string
				refusalMeans string
			}{
				{"enrollment", func(tokenOf func(string, string) string) answered {
					uid := uuid.NewString()
					return ts.launcherAnswer(t, key, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
						"kind": "pod", "runtime_id": uid, "thumbprint": thumbprintOf(t, newSigningKey(t)), "pod_token": tokenOf(workerAccount, uid),
					})
				}, http.StatusForbidden, "POD_IDENTITY_MISMATCH", "a token that does not prove the pod"},
				{"renewal", func(tokenOf func(string, string) string) answered {
					return ts.sessionAnswer(t, podKey, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew",
						map[string]any{"pod_token": tokenOf(workerAccount, podUID)})
				}, http.StatusForbidden, "POD_IDENTITY_MISMATCH", "a token that does not prove the pod"},
				{"token login", func(tokenOf func(string, string) string) answered {
					return ts.reqAnswer(t, http.MethodPost, "/v1/launcher-credentials/token", nil,
						map[string]any{"token": tokenOf(daemonAccount, ""), "request": ts.daemonRequest(t, newSigningKey(t))})
				}, http.StatusUnauthorized, "TOKEN_INVALID", "a login token that does not verify"},
				{"pod-session registration", func(tokenOf func(string, string) string) answered {
					return ts.reqAnswer(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions", map[string]string{"Authorization": "Bearer " + registrar},
						map[string]any{"pod_token": tokenOf(workerAccount, uuid.NewString()), "session": "example-session"})
				}, http.StatusUnauthorized, "POD_TOKEN_INVALID", "a pod token that does not verify"},
				{"listed caller's route", func(tokenOf func(string, string) string) answered {
					return ts.reqAnswer(t, http.MethodGet, "/v1/pending?approver="+url.QueryEscape(testApprover),
						map[string]string{"Authorization": "Bearer " + tokenOf(dispatchAccount, "")}, nil)
				}, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "a caller's token that does not verify"},
				{"service route", func(tokenOf func(string, string) string) answered {
					return ts.reqAnswer(t, http.MethodPost, "/v1/oauth/exchange/pod-sessions", map[string]string{"Authorization": "Bearer " + tokenOf(daemonAccount, "")},
						map[string]any{"pod_token": ts.podToken(t, workerAccount, uuid.NewString()), "session": "example-session"})
				}, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "a caller's token that does not verify"},
				{"edge listener's route", func(tokenOf func(string, string) string) answered {
					return ts.reqAnswer(t, http.MethodGet, repositoriesPath+"?project=PRIV",
						map[string]string{"Authorization": "Bearer " + tokenOf(edgeAccount, "")}, nil)
				}, http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID", "a caller's token that does not verify"},
			} {
				t.Run(route.name+": a token of the second issuer", func(t *testing.T) {
					requireUnjudged(t, route.call(func(account, podUID string) string {
						return issuedToken(t, second, secondKey, account, podUID)
					}))
				})
				t.Run(route.name+": a token of the first issuer signed by a key it never published is "+route.refusalMeans, func(t *testing.T) {
					requireJudged(t, route.call(func(account, podUID string) string {
						return issuedToken(t, ts.podIssuer, stray, account, podUID)
					}), route.status, route.code)
				})
			}
		})
	}
}

// googleOutage is a broker whose listed caller and edge listener are Google accounts, verified
// against a stand-in Google issuer whose key set fails from the start, read again on every token
// rather than once per DefaultKeysFailureSpacing, so the test can mend it and read the next
// answer at once; token mints that issuer's identity token for an account and an audience.
type googleOutage struct {
	*testServer
	google *oidctest.Issuer
	key    *oidctest.Key
}

func newGoogleOutage(t *testing.T) *googleOutage {
	t.Helper()
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	keys, err := oidc.NewKeys(context.Background(), google.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys for Google: %v", err)
	}
	keys.FailureSpacing = 0
	google.FailKeys(http.StatusServiceUnavailable)
	dispatch := repositoriesDispatch(t)
	ts := newTestServerWith(t, func(d *api.Deps) {
		d.GoogleAPIAccounts = map[string]api.Account{listenerGoogleAccount: ciphertest.Caller(t, "listener")}
		d.GoogleAccountTokens = api.GoogleTokens{Verifier: oidc.NewWithKeys(google.URL(), machineAudience, keys)}
		d.EdgeListeners = edgeListeners(t, "edge-listener/edge-g/PRIV=google:"+edgeGoogleAccount, d.Enroll.Issuers)
		d.ProjectRepositories = projectrepos.New(dispatch.URL, dispatch.Account, nil)
	})
	return &googleOutage{testServer: ts, google: google, key: key}
}

// token is a Google identity token for account and audience, with a verified email, issued
// seconds ago and living an hour.
func (g *googleOutage) token(t *testing.T, account, audience string) string {
	t.Helper()
	now := time.Now()
	claims := g.google.Claims(account, audience)
	claims["email"], claims["email_verified"] = account, true
	claims["iat"], claims["nbf"], claims["exp"] = now.Add(-5*time.Second).Unix(), now.Add(-5*time.Second).Unix(), now.Add(time.Hour).Unix()
	return g.google.Mint(t, g.key, claims)
}

// introspectAs is POST /v1/oauth/introspect, a route of the listed caller, presenting bearer.
func (g *googleOutage) introspectAs(t *testing.T, bearer string) answered {
	t.Helper()
	return send(t, http.DefaultClient, request(t, http.MethodPost, g.URL+"/v1/oauth/introspect",
		strings.NewReader(url.Values{"token": {"a-token-nobody-issued"}}.Encode()),
		map[string]string{"Authorization": "Bearer " + bearer, "Content-Type": "application/x-www-form-urlencoded"}))
}

// repositoriesAs is the edge listener's GET /v1/project-repositories for PRIV, presenting bearer.
func (g *googleOutage) repositoriesAs(t *testing.T, bearer string) answered {
	t.Helper()
	return g.reqAnswer(t, http.MethodGet, repositoriesPath+"?project=PRIV", map[string]string{"Authorization": "Bearer " + bearer}, nil)
}

// TestAGoogleCallersTokenNobodyJudgedIsAnswered503: while Google's key set cannot be read, a
// listed caller's live Google identity token is answered 503 OIDC_ISSUER_UNAVAILABLE with
// Retry-After: 1 on a route of the listed caller and on the edge listener's route, as a cluster's
// token is, where it was answered 401 ACCOUNT_TOKEN_INVALID as a token that does not verify. A
// token for another audience is one the broker judges with no key: it is still 401
// ACCOUNT_TOKEN_INVALID, naming no wait. Once the key set answers, the same tokens are served.
func TestAGoogleCallersTokenNobodyJudgedIsAnswered503(t *testing.T) {
	g := newGoogleOutage(t)
	listener, edge := g.token(t, listenerGoogleAccount, machineAudience), g.token(t, edgeGoogleAccount, machineAudience)
	t.Run("the listed caller's live token", func(t *testing.T) {
		requireUnjudged(t, g.introspectAs(t, listener))
	})
	t.Run("the edge listener's live token", func(t *testing.T) {
		requireUnjudged(t, g.repositoriesAs(t, edge))
	})
	t.Run("the listed caller's token for another audience is a token that does not verify", func(t *testing.T) {
		requireJudged(t, g.introspectAs(t, g.token(t, listenerGoogleAccount, "https://elsewhere.example")), http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
	})
	t.Run("the edge listener's token for another audience is a token that does not verify", func(t *testing.T) {
		requireJudged(t, g.repositoriesAs(t, g.token(t, edgeGoogleAccount, "https://elsewhere.example")), http.StatusUnauthorized, "ACCOUNT_TOKEN_INVALID")
	})
	g.google.FailKeys(0)
	t.Run("once the key set answers, both are served", func(t *testing.T) {
		if got := g.introspectAs(t, listener); got.status != http.StatusOK {
			t.Fatalf("the listed caller's introspection = %d %s, want 200", got.status, got.body)
		}
		if got := g.repositoriesAs(t, edge); got.status != http.StatusOK {
			t.Fatalf("the edge listener's repositories = %d %s, want 200", got.status, got.body)
		}
	})
}
