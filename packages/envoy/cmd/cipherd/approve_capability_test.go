// approve_capability_test.go drives the REAL compiled broker's approve capability end to end: a
// profiles file whose dpop block is named DISPATCH_APPROVE_TOKEN turns it on, a person's sign-in is
// traded for its placeholder, and a request through the proxy port with that placeholder reaches
// the private destination as a token the broker minted, bound to its pod's own key, acting for
// the person and for nothing but approve. The helpers are main_test.go's, token_exchange_test.go's
// and person_signin_test.go's.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// The approve capability's host in these rows, the line the boot names it in, and the route a
// review is sent to.
const (
	approveHost       = "dispatch.test"
	approveLine       = "broker: the approve capability"
	approveReviewPath = "/api/v1/artifacts/artifact-1/versions/1/reviews"
)

// approveProfiles is a profiles file of the one dpop block DISPATCH_APPROVE_TOKEN, binding
// approveHost in Authorization alone and allowing a document's reviews.
func approveProfiles() string {
	return `{"profiles": [{"name": "DISPATCH_APPROVE_TOKEN", "proxy": {"hosts": ["` + approveHost + `"], "headers": ["Authorization"],
	"allow": [{"method": "POST", "pathPrefix": "/api/v1/artifacts/"}], "kind": "dpop"}}]}`
}

// recordedRequest is what the stand-in destination keeps of one request it was sent.
type recordedRequest struct {
	method, path         string
	authorization, proof []string
}

// mintLine is the line the broker writes for each token it mints for a minted grant
// (oauth.Provider.MintGrantToken).
const mintLine = "broker: minted a grant's token"

// postThroughProxy sends one POST to approveHost's path through the proxy port, with the grant's
// placeholder as its one Authorization header, and answers the status, the body and the headers.
func postThroughProxy(t *testing.T, client *http.Client, path, held string) (int, string, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://"+approveHost+path, strings.NewReader(`{"state": "approved"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+held)
	request.Header.Set("Content-Type", "application/json")
	answer, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST %s through the proxy port: %v", path, err)
	}
	body, _ := io.ReadAll(answer.Body)
	_ = answer.Body.Close()
	return answer.StatusCode, string(body), answer.Header
}

// TestTheBootServesTheApproveCapabilityOfItsDPoPBlock: a broker whose profiles hold the dpop block
// DISPATCH_APPROVE_TOKEN, on the host BROKER_PROXY_PRIVATE_UPSTREAM sends to a stand-in Dispatch on
// loopback, logs the capability with the digest of that block and destination
// (config.MintedEntryDigest). A person's sign-in that asks for the proxy placeholder is answered
// it, with the proxy's URL and authority and the block's one host. A comment sent first through
// the proxy port with that placeholder, to a route the block's allow list does not name, is
// refused 403 REQUEST_NOT_ALLOWED with nothing minted: no access token is written, no mint line is
// logged, and the stand-in receives nothing. The review sent after it reaches the stand-in
// carrying a DPoP token other than the placeholder and a proof, the one token the broker minted;
// introspection reads that token as the person's, of the scope approve alone, the project scope
// person and the session, bound to the key the boot logged as the proxy's; and the broker logged
// the token's SHA-256 and that key's thumbprint, never the token.
func TestTheBootServesTheApproveCapabilityOfItsDPoPBlock(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	clusterKey := cluster.PublishKey(t, "cluster-key")
	googleKey := google.PublishKey(t, "google-key")
	apiToken := accountToken(t, cluster, clusterKey, apiAccount)

	var mu sync.Mutex
	var seen []recordedRequest
	dispatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, recordedRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Values("Authorization"), proof: r.Header.Values("DPoP")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"recorded": true}`)
	}))
	t.Cleanup(dispatch.Close)
	destination := approveHost + "=" + dispatch.URL
	profiles := approveProfiles()

	env := append(googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds), airlockEnv(t)...)
	env = append(env,
		"BROKER_PROFILES_FILE="+profilesFile(t, profiles),
		"BROKER_PROXY_PRIVATE_UPSTREAM="+destination,
		"BROKER_AIRLOCK_TEST_DIAL_ALLOW="+strings.TrimPrefix(dispatch.URL, "http://"),
	)
	broker := startBroker(t, brokerOptions{Env: env})
	requireHealthy(t, broker.Addr)

	parsed, err := policy.ParseProfiles([]byte(profiles))
	if err != nil {
		t.Fatal(err)
	}
	upstreams, err := config.ParsePrivateUpstreams(destination)
	if err != nil || len(upstreams) != 1 {
		t.Fatalf("ParsePrivateUpstreams(%q) = %v, %v", destination, upstreams, err)
	}
	wantDigest := config.MintedEntryDigest(parsed.Digest("DISPATCH_APPROVE_TOKEN"), upstreams[0])
	if line := broker.AwaitRecord(t, approveLine, nil); line["name"] != "DISPATCH_APPROVE_TOKEN" || line["host"] != approveHost || line["entry_digest"] != wantDigest {
		t.Fatalf("the %q line = %v; want the name DISPATCH_APPROVE_TOKEN, the host %s and the entry digest %s", approveLine, line, approveHost, wantDigest)
	}
	podThumbprint, _ := broker.AwaitRecord(t, dpopKeyLine, nil)["thumbprint"].(string)

	live := newLiveBroker(broker.Addr, proofKey(t))
	form := signInForm(
		mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
		mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName),
		"ada-approve-1")
	form.Set("requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
	status, body := live.exchange(t, form)
	held, _ := body["access_token"].(string)
	if status != http.StatusOK || body["issued_token_type"] != oauthwire.TokenTypeProxyPlaceholder || held == "" {
		t.Fatalf("the placeholder trade of %s's sign-in = %d %v; want 200 and a proxy placeholder", person, status, body)
	}
	release, _ := body[oauthwire.ExtraProxy].(map[string]any)
	names, _ := release["names"].(map[string]any)
	name, _ := names["DISPATCH_APPROVE_TOKEN"].(map[string]any)
	hosts, _ := name["hosts"].([]any)
	authority, _ := release["ca_certificate"].(string)
	if release["proxy_url"] != "http://127.0.0.1:13381" || release["proxy_password"] != held || len(hosts) != 1 || hosts[0] != approveHost || authority == "" {
		t.Fatalf("the proxy release = %v; want BROKER_AIRLOCK_PROXY_URL, the placeholder as the password, the host %s and the authority", release, approveHost)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(authority)) {
		t.Fatalf("the release's authority does not parse: %q", authority)
	}
	proxyURL, err := url.Parse("http://agent:" + held + "@" + airlockProxyAddr(t, broker))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}

	// The comment, before the grant's first allowed call: the mint of its token, had it run, logs
	// its line before the refusal's, through the one logger, so once the refusal's line is there
	// the log holds every mint line the request made.
	db := connectToBroker(t, databaseURL)
	accessTokens := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(context.Background(), `select count(*) from oauth_access_tokens`).Scan(&n); err != nil {
			t.Fatalf("count the broker's access tokens: %v", err)
		}
		return n
	}
	tokensBefore := accessTokens()
	status, answered, header := postThroughProxy(t, client, "/api/v1/issues/APV-1/comments", held)
	if status != http.StatusForbidden || header.Get("Cipher-Proxy-Code") != "REQUEST_NOT_ALLOWED" {
		t.Fatalf("the comment through the proxy port = %d %q (Cipher-Proxy-Code %q); want the proxy's 403 REQUEST_NOT_ALLOWED", status, answered, header.Get("Cipher-Proxy-Code"))
	}
	broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"answer_id": header.Get(answer.Header), "code": "REQUEST_NOT_ALLOWED"})
	if minted := recordsWith(broker.Log(), mintLine, nil); len(minted) != 0 {
		t.Fatalf("the refused comment minted a token: %v; want no %q line", minted, mintLine)
	}
	if tokens := accessTokens(); tokens != tokensBefore {
		t.Fatalf("access tokens after the refused comment = %d; want the %d there were", tokens, tokensBefore)
	}
	mu.Lock()
	sent := len(seen)
	mu.Unlock()
	if sent != 0 {
		t.Fatalf("the stand-in received %d requests for the refused comment; want none", sent)
	}

	status, answered, _ = postThroughProxy(t, client, approveReviewPath, held)
	if status != http.StatusCreated {
		t.Fatalf("the review through the proxy port = %d %s; want the stand-in's 201", status, answered)
	}
	mu.Lock()
	got := append([]recordedRequest(nil), seen...)
	mu.Unlock()
	if len(got) != 1 || got[0].method != http.MethodPost || got[0].path != approveReviewPath || len(got[0].authorization) != 1 || len(got[0].proof) != 1 {
		t.Fatalf("the stand-in received %+v; want the one review with one Authorization and one DPoP proof", got)
	}
	token, scheme := strings.CutPrefix(got[0].authorization[0], "DPoP ")
	if !scheme || token == "" || token == held {
		t.Fatalf("the stand-in received Authorization of %d characters; want DPoP and a token other than the placeholder", len(got[0].authorization[0]))
	}

	status, introspected := live.introspect(t, token, apiToken)
	for field, want := range map[string]any{
		"active": true, "person": person, "scope": oauthwire.ScopeApprove, "project_scope": oauthwire.ProjectScopePerson,
		"session": "ada-approve-1", "jkt": podThumbprint, "projects": nil,
	} {
		if status != http.StatusOK || introspected[field] != want {
			t.Errorf("introspection of the token the stand-in received answered %s %v (status %d); want %v", field, introspected[field], status, want)
		}
	}
	sum := sha256.Sum256([]byte(token))
	broker.AwaitRecord(t, mintLine, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "jkt": podThumbprint})
	if minted := recordsWith(broker.Log(), mintLine, nil); len(minted) != 1 {
		t.Fatalf("the broker logged %d %q lines; want 1, the review's", len(minted), mintLine)
	}
	if tokens := accessTokens(); tokens != tokensBefore+1 {
		t.Fatalf("access tokens after the review = %d; want %d, the one the review minted", tokens, tokensBefore+1)
	}
	if strings.Contains(broker.Log(), token) {
		t.Fatal("the broker's log holds the token it minted; it logs the token's SHA-256 alone")
	}
}

// TestABrokerWithoutTheApproveBlockRefusesThePlaceholder is the control of the test above: the
// same broker with no profiles file logs the capability off and refuses the same trade
// invalid_request, so the placeholder comes from the DISPATCH_APPROVE_TOKEN block.
func TestABrokerWithoutTheApproveBlockRefusesThePlaceholder(t *testing.T) {
	databaseURL := storetest.URL(t)
	cluster, github, google := oidctest.New(t), oidctest.New(t), oidctest.New(t)
	github.PublishKey(t, "github-key")
	cluster.PublishKey(t, "cluster-key")
	googleKey := google.PublishKey(t, "google-key")
	broker := startBroker(t, brokerOptions{Env: googleExchangeEnv(t, databaseURL, cluster, github, google, longAccessSeconds)})
	requireHealthy(t, broker.Addr)
	broker.AwaitRecord(t, approveLine, map[string]any{"state": "off"})
	live := newLiveBroker(broker.Addr, proofKey(t))
	form := signInForm(
		mintPersonToken(t, google, googleKey, person, deviceAudience, true, signInDomain),
		mintMachineToken(t, google, googleKey, pinnedFleetAccount, machineAudience, true, fleetCloudProject, fleetInstanceName),
		"ada-approve-2")
	form.Set("requested_token_type", oauthwire.TokenTypeProxyPlaceholder)
	if status, body := live.exchange(t, form); status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
		t.Fatalf("the placeholder trade on a broker with no approve block = %d %v; want 400 invalid_request", status, body)
	}
}
