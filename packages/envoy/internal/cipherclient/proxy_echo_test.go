// packages/envoy/internal/cipherclient/proxy_echo_test.go
//
// A gateway or a proxy in the place of Google's security token service, its service-account
// impersonation call or a sign-in issuer's token endpoint can answer with a page of its own that
// shows the request back: its Authorization header and its form, the credential this machine
// trades among them. Every error these identities return reaches a person (agent-secrets prints
// it, the helper logs it), so these rows send the real requests to such a page and read the error
// each one returns: no credential the request carried is in it (redact.OAuth2's answer and
// redact.RefusalClient for the external_account exchange, and redact.Class, which prints the
// sign-in issuer's answer as its class alone).

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// echoedCredentialFields are the form fields where these requests carry a credential.
var echoedCredentialFields = []string{"client_secret", "refresh_token", "device_code", "subject_token"}

// proxyEcho is a stand-in for whatever answers in a Google service's or a token endpoint's place:
// it answers status with an HTML page that names itself (echoPage), shows each credential the
// request carried bare on a line of its own (the Authorization header's value after its scheme, a
// form field's value), so only a redaction of that very token can hide it, and then the request
// whole: its request line, its headers and its body. It keeps each credential it showed.
type proxyEcho struct {
	status int
	mu     sync.Mutex
	shown  []string
}

// echoPage is the line a proxyEcho's page opens its text with, which an error that quotes the page
// holds and one that prints none of it does not.
const echoPage = "this page shows the request it answers"

func (e *proxyEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var bare []string
	for _, value := range r.Header.Values("Authorization") {
		if _, credential, ok := strings.Cut(value, " "); ok {
			bare = append(bare, credential)
		}
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media == "application/x-www-form-urlencoded" {
		form, _ := url.ParseQuery(string(body))
		for _, field := range echoedCredentialFields {
			bare = append(bare, form[field]...)
		}
	}
	e.mu.Lock()
	e.shown = append(e.shown, bare...)
	e.mu.Unlock()
	var page strings.Builder
	page.WriteString("<html><head><title>Bad Gateway</title></head><body><h1>Bad Gateway</h1><pre>\n" + echoPage + "\n")
	for _, credential := range bare {
		page.WriteString(credential + "\n")
	}
	page.WriteString(r.Method + " " + r.URL.RequestURI() + " " + r.Proto + "\n")
	for name, values := range r.Header {
		for _, value := range values {
			page.WriteString(name + ": " + value + "\n")
		}
	}
	page.WriteString("\n" + string(body) + "\n</pre></body></html>\n")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(e.status)
	_, _ = io.WriteString(w, page.String())
}

// credentials is every credential the stand-in has shown so far.
func (e *proxyEcho) credentials() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.shown...)
}

// assertNamedWithoutCredentials fails the row when err's text holds any of bare (the credentials
// the page showed on lines of their own) or of others, or, where the error quotes the page (it holds
// echoPage) and the rule printed it (not redact.WordsNotReadable in its place), redacts fewer
// places than bare holds. An error that prints none of the page, as a refusal printed by its status
// and registered code alone does (redact.Answered), has no place to redact.
// bare must not be empty: a row that saw none could not have failed. It captures err's text under
// capture and fails a credential it holds with `REDACT-ASSERT <capture>`, the check's designated
// assertion (redacttest.Capture); each credential carries -RDX-, the check's marker for them.
func assertNamedWithoutCredentials(t *testing.T, capture string, err error, bare []string, others ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call through the stand-in succeeded")
	}
	if len(bare) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	printed := err.Error()
	redacttest.Capture(t, capture, printed)
	for _, credential := range append(others, bare...) {
		if strings.Contains(printed, credential) {
			t.Errorf("REDACT-ASSERT %s: the error carries the credential %q the request sent:\n%s", capture, credential, printed)
		}
	}
	if n := strings.Count(printed, "[redacted]"); strings.Contains(printed, echoPage) && n < len(bare) && !strings.Contains(printed, redact.WordsNotReadable) {
		t.Errorf("the error redacts %d places, want at least the %d bare credentials:\n%s", n, len(bare), printed)
	}
}

// writeSubject writes token to a file of its own and returns the path.
func writeSubject(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subject")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// workforceSecret is the external_account credential's client secret, which its exchange sends on
// a Basic header.
const workforceSecret = "workforce-client-secret-RDX-0a7c"

// confidentialExternalAccountJSON is an external_account credential whose subject token is
// subject (in a file of its own), whose client is a confidential one (so the exchange carries its
// secret on a Basic header), and whose exchange and impersonation run at sts and impersonation.
func confidentialExternalAccountJSON(t *testing.T, subject, sts, impersonation string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
		"token_url":                         sts + "/v1/token",
		"service_account_impersonation_url": impersonation + "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken",
		"credential_source":                 map[string]any{"file": writeSubject(t, subject)},
		"client_id":                         "workforce-client",
		"client_secret":                     workforceSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestProxyEchoExternalAccountMintNamesThePageWithoutItsCredentials: x/oauth2's own exchange for
// an external_account credential prints `status code <n>: <body>` for any refusal; the page a
// stand-in for the security token service answers echoes the subject token and the client's Basic
// header, and the one a stand-in for the impersonation call answers echoes the access token the
// exchange minted, which nothing but the request ever held. The mint's error names the service
// that refused and its status (externalAccountFailure), and none of those credentials.
func TestProxyEchoExternalAccountMintNamesThePageWithoutItsCredentials(t *testing.T) {
	const subject, secret = "workload-subject-RDX-3b8f4d21", workforceSecret
	t.Run("security token service", func(t *testing.T) {
		stand := &proxyEcho{status: http.StatusBadGateway}
		sts := httptest.NewServer(stand)
		t.Cleanup(sts.Close)
		identity := &ExternalAccountIdentity{
			CredentialsJSON: confidentialExternalAccountJSON(t, subject, sts.URL, "http://127.0.0.1:1"),
			ServiceAccount:  "robot@example.iam.gserviceaccount.com", Audience: testAudience, IAMCredentialsURL: "http://127.0.0.1:1",
		}
		_, _, err := identity.Token(context.Background())
		assertNamedWithoutCredentials(t, "brokerclient.proxy-echo-sts", err, stand.credentials(), subject, secret)
	})
	t.Run("impersonation", func(t *testing.T) {
		const minted = "sts-minted-access-RDX-6e2a90c4"
		sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": minted, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token", "token_type": "Bearer", "expires_in": 3600})
		}))
		t.Cleanup(sts.Close)
		stand := &proxyEcho{status: http.StatusForbidden}
		impersonation := httptest.NewServer(stand)
		t.Cleanup(impersonation.Close)
		identity := &ExternalAccountIdentity{
			CredentialsJSON: confidentialExternalAccountJSON(t, subject, sts.URL, impersonation.URL),
			ServiceAccount:  "robot@example.iam.gserviceaccount.com", Audience: testAudience, IAMCredentialsURL: "http://127.0.0.1:1",
		}
		_, _, err := identity.Token(context.Background())
		assertNamedWithoutCredentials(t, "brokerclient.proxy-echo-impersonation", err, stand.credentials(), minted, subject, secret)
	})
}

// signInStandIn is a sign-in issuer whose discovery document is its own and whose token endpoint
// is a proxyEcho answering 502.
func signInStandIn(t *testing.T) (string, *proxyEcho) {
	t.Helper()
	stand := &proxyEcho{status: http.StatusBadGateway}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token",
			"device_authorization_endpoint": server.URL + "/device", "jwks_uri": server.URL + "/keys",
		})
	})
	mux.Handle("POST /token", stand)
	return server.URL, stand
}

// TestProxyEchoDeviceSignInPrintsTheClassAndNoCredential: the device grant's poll carries the
// client secret and the device code, and a renewal the client secret and the person's refresh
// token; a stand-in for the token endpoint echoes them. The sign-in issuer is a server other than
// the broker, so its answer prints as its class (redact.Class), the page's status, and none of it.
func TestProxyEchoDeviceSignInPrintsTheClassAndNoCredential(t *testing.T) {
	const secret = "device-client-secret-RDX-c41f"
	t.Run("Wait", func(t *testing.T) {
		issuer, stand := signInStandIn(t)
		flow := &DeviceFlow{Issuer: issuer, ClientID: "device-client", ClientSecret: secret}
		const deviceCode = "device-code-RDX-8d7e6f50"
		_, err := flow.Wait(context.Background(), &oauth2.DeviceAuthResponse{DeviceCode: deviceCode, Interval: 1, Expiry: time.Now().Add(time.Minute)})
		assertClassWithoutCredentials(t, "brokerclient.proxy-echo-wait", err, stand.credentials(), secret, deviceCode)
		var re *oauth2.RetrieveError
		if !errors.As(err, &re) || re.Response.StatusCode != http.StatusBadGateway {
			t.Errorf("Wait = %v, which no longer unwraps to the token endpoint's RetrieveError", err)
		}
	})
	t.Run("Refresh", func(t *testing.T) {
		issuer, stand := signInStandIn(t)
		flow := &DeviceFlow{Issuer: issuer, ClientID: "device-client", ClientSecret: secret}
		const refresh = "person-refresh-RDX-2f9c0b77"
		_, err := flow.Refresh(context.Background(), refresh)
		assertClassWithoutCredentials(t, "brokerclient.proxy-echo-refresh", err, stand.credentials(), secret, refresh)
		if errors.Is(err, ErrSignInEnded) {
			t.Error("a gateway's page reads as a sign-in the issuer ended")
		}
	})
}

// assertClassWithoutCredentials fails the row when err's text holds any of bare (the credentials
// the page showed on lines of their own) or of others, or does not name the page's status as its
// class (redact.Class). bare must not be empty: a row that saw none could not have failed. It
// captures err's text under capture and fails a credential it holds with `REDACT-ASSERT
// <capture>`, as assertNamedWithoutCredentials does.
func assertClassWithoutCredentials(t *testing.T, capture string, err error, bare []string, others ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call through the stand-in succeeded")
	}
	if len(bare) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	printed := err.Error()
	redacttest.Capture(t, capture, printed)
	for _, credential := range append(others, bare...) {
		if strings.Contains(printed, credential) {
			t.Errorf("REDACT-ASSERT %s: the error carries the credential %q the request sent:\n%s", capture, credential, printed)
		}
	}
	if !strings.HasSuffix(printed, ": the server answered HTTP 502") {
		t.Errorf("the error does not end in the page's status as its class:\n%s", printed)
	}
}
