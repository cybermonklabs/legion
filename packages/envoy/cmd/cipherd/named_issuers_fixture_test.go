// named_issuers_fixture_test.go holds what the named-issuer tests (named_issuers_test.go) drive
// the broker with: the accounts and the session name they use, the environment of a broker that
// trusts named issuers (which natscallout_issuers_test.go starts too), a cluster's tokens, and
// namedClient, which calls the broker as the clusters' daemons, pods and listed callers do, with
// the routes a row calls and the answers it wants (namedRoute, expect).
package main

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const (
	// namedService is the service the two clusters' daemons log in as; namedDaemonA and
	// namedDaemonB are their accounts, one in each cluster; namedWorker is the account their pods
	// run as, which BROKER_SERVICES binds to that service; namedCaller is the listed caller of the
	// routes Dispatch's server calls; namedSession is the session name a pod writes under.
	namedService = "legion-daemon"
	namedDaemonA = "system:serviceaccount:legion-system:legion-daemon-a"
	namedDaemonB = "system:serviceaccount:legion-system:legion-daemon-b"
	namedWorker  = "system:serviceaccount:legion:legion-worker"
	namedCaller  = "system:serviceaccount:example-ns:example-dispatch"
	namedSession = "example-session"
	// issuerUnavailable is the code of the answer to a token whose issuer the broker cannot read.
	issuerUnavailable = "OIDC_ISSUER_UNAVAILABLE"
)

// namedIssuersEnv is the environment of a broker on databaseURL that trusts issuers, the value of
// BROKER_K8S_OIDC_ISSUER, for enrolmentAudience, binds namedWorker's pods to namedService, and
// stops at once when it is asked to; extra adds the settings a row is about.
func namedIssuersEnv(t *testing.T, databaseURL, issuers string, extra ...string) []string {
	t.Helper()
	fakeSecretsFile := filepath.Join(t.TempDir(), "fake-secrets.json")
	if err := os.WriteFile(fakeSecretsFile, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{
		"BROKER_DATABASE_URL=" + databaseURL,
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + fakeSecretsFile,
		"BROKER_K8S_OIDC_ISSUER=" + issuers,
		"BROKER_K8S_OIDC_AUDIENCE=" + enrolmentAudience,
		"BROKER_SERVICES=" + namedService + "=" + namedWorker,
		"BROKER_DRAIN_SECONDS=0",
	}, extra...)
}

// bothIssuers is the setting naming a and b, in that order, the broker's two clusters.
func bothIssuers(a, b *oidctest.Issuer) string {
	return "a=" + a.URL() + ",b=" + b.URL()
}

// clusterToken is account's projected token whose iss is cluster's, signed with key, for
// enrolmentAudience, issued a few seconds ago and living an hour, and bound to the pod podUID (""
// for a token bound to no pod, which a daemon's login and a listed caller present). A key that is
// not cluster's makes a token naming one issuer and carrying another's signature.
func clusterToken(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account, podUID string) string {
	t.Helper()
	return clusterTokenFor(t, cluster, key, account, podUID, enrolmentAudience)
}

// clusterTokenFor is clusterToken for audience in place of enrolmentAudience.
func clusterTokenFor(t *testing.T, cluster *oidctest.Issuer, key *oidctest.Key, account, podUID, audience string) string {
	t.Helper()
	claims := cluster.Claims(account, audience)
	claims["iat"] = time.Now().Add(-5 * time.Second).Unix()
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return cluster.Mint(t, key, claims)
}

// namedClient drives one running broker as the clusters' daemons, pods and listed callers do.
// Every call answers the broker's status and the code of its refusal, "" for an answer that is
// none, and logs the route and the answer, never a token.
type namedClient struct {
	t    *testing.T
	base string
}

// send sends body as JSON to path with headers and answers the status and the body.
func (c namedClient) send(method, path string, headers map[string]string, body any) (int, []byte) {
	c.t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.base+path, payload)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Logf("> %s %s\n< %d %s", method, path, response.StatusCode, bytes.TrimSpace(answer))
	return response.StatusCode, answer
}

// codeOf is the code of a refusal's JSON body, "" for a body that names none.
func codeOf(answer []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(answer, &refusal)
	return refusal.Code
}

// tokenLogin logs namedService in with token, its key key, and answers the credential it issued.
func (c namedClient) tokenLogin(token string, key *ecdsa.PrivateKey) (int, string, launcher) {
	c.t.Helper()
	return c.tokenLoginAs(namedService, token, key)
}

// tokenLoginAs logs service in with token, its key key, and answers the credential it issued.
func (c namedClient) tokenLoginAs(service, token string, key *ecdsa.PrivateKey) (int, string, launcher) {
	c.t.Helper()
	request, err := record.Sign(key, c.base, []record.AuthorizationDetail{
		{Type: record.KindLauncherCredential, Identifier: "example-host-" + service, Service: service},
	}, "", "", nil, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	status, answer := c.send(http.MethodPost, "/v1/launcher-credentials/token", nil, map[string]any{"token": token, "request": request})
	var login struct {
		CredentialID string `json:"credential_id"`
	}
	_ = json.Unmarshal(answer, &login)
	return status, codeOf(answer), launcher{key: key, credentialID: login.CredentialID}
}

// enroll has by's credential enroll the pod podUID, which proves itself with token, under a key of
// its own, and answers the enrollment.
func (c namedClient) enroll(by launcher, podUID, token string) (int, string, pod) {
	c.t.Helper()
	key := proofKey(c.t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		c.t.Fatal(err)
	}
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodPost, c.base+"/v1/enrollments", time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	status, answer := c.send(http.MethodPost, "/v1/enrollments", map[string]string{"Proof": signed}, map[string]any{
		"kind": "pod", "runtime_id": podUID, "thumbprint": thumbprint, "pod_token": token,
	})
	var enrolled struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	_ = json.Unmarshal(answer, &enrolled)
	return status, codeOf(answer), pod{runtimeID: podUID, key: key, enrollmentID: enrolled.EnrollmentID}
}

// renew renews p's enrollment with token.
func (c namedClient) renew(p pod, token string) (int, string) {
	c.t.Helper()
	path := "/v1/enrollments/" + p.enrollmentID + "/renew"
	signed, err := proof.Sign(p.key, p.enrollmentID, http.MethodPost, c.base+path, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	status, answer := c.send(http.MethodPost, path, map[string]string{"Proof": signed}, map[string]any{"pod_token": token})
	return status, codeOf(answer)
}

// register registers the pod podToken names for namedSession, as a registrar presenting its own
// login token, bearer, does.
func (c namedClient) register(bearer, podToken string) (int, string) {
	c.t.Helper()
	status, answer := c.registerFor(bearer, podToken, namedSession)
	return status, codeOf(answer)
}

// registerFor registers the pod podToken names for session, as a registrar presenting its own
// login token, bearer, does, and answers the status and the body.
func (c namedClient) registerFor(bearer, podToken, session string) (int, []byte) {
	c.t.Helper()
	return c.send(http.MethodPost, "/v1/oauth/exchange/pod-sessions", map[string]string{"Authorization": "Bearer " + bearer},
		map[string]any{"pod_token": podToken, "session": session})
}

// pending is GET /v1/pending, a route of the listed caller, presenting bearer.
func (c namedClient) pending(bearer string) (int, string) {
	c.t.Helper()
	status, answer := c.send(http.MethodGet, "/v1/pending?approver="+url.QueryEscape("ada@example.com"), map[string]string{"Authorization": "Bearer " + bearer}, nil)
	return status, codeOf(answer)
}

// authenticates is a launcher route by's credential reaches only while it authenticates: ending
// an enrollment no one made, which answers 204 to a credential the broker takes and 401
// LAUNCHER_INVALID to one it does not.
func (c namedClient) authenticates(by launcher) (int, string) {
	c.t.Helper()
	path := "/v1/enrollments/" + uuid.NewString()
	signed, err := proof.SignLauncher(by.key, by.credentialID, http.MethodDelete, c.base+path, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	status, answer := c.send(http.MethodDelete, path, map[string]string{"Proof": signed}, nil)
	return status, codeOf(answer)
}

// operatorOf is the operator GET /v1/enrollments/self names for p's enrollment, the person it runs
// for, "" for none.
func (c namedClient) operatorOf(p pod) string {
	c.t.Helper()
	const path = "/v1/enrollments/self"
	signed, err := proof.Sign(p.key, p.enrollmentID, http.MethodGet, c.base+path, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	status, answer := c.send(http.MethodGet, path, map[string]string{"Proof": signed}, nil)
	var self struct {
		Operator *string `json:"operator"`
	}
	if err := json.Unmarshal(answer, &self); status != http.StatusOK || err != nil {
		c.t.Fatalf("GET %s as enrollment %s answered %d %q, want 200 naming its operator", path, p.enrollmentID, status, codeOf(answer))
	}
	if self.Operator == nil {
		return ""
	}
	return *self.Operator
}

// namedRoute is one call a row makes and the answer it wants.
type namedRoute struct {
	name   string
	call   func() (int, string)
	status int
	code   string
}

// expect runs each route as a subtest of its own, failing it unless the broker gave the status and
// code it wants.
func expect(t *testing.T, prefix string, routes []namedRoute) {
	t.Helper()
	for _, route := range routes {
		t.Run(prefix+route.name, func(t *testing.T) {
			if status, code := route.call(); status != route.status || code != route.code {
				t.Fatalf("answered %d %q, want %d %q", status, code, route.status, route.code)
			}
		})
	}
}
