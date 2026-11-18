package oidc

// A gateway or a proxy in the sign-in issuer's token endpoint's place can answer the broker's
// client check with a page of its own that shows the request back: the client's Basic header or
// its client_secret, and the code. x/oauth2 keeps that page whole in the *oauth2.RetrieveError it
// returns, whose text prints it when the page names no OAuth `error` field, and the broker logs
// the check's error (cmd/broker's checkSignInClient). This row sends the real check to such a page
// and reads the error. (The code flow's own exchange and refresh, codeflow.go, are PR #4368's
// file; their printers wrap the error at the call site and have rows of their own.)

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// echoedCredentialFields are the form fields where the check's token request carries a credential.
var echoedCredentialFields = []string{"client_secret", "code"}

// tokenEcho is a stand-in for whatever answers in the token endpoint's place: it answers 502 with
// an HTML page that shows each credential the request carried bare on a line of its own (the
// Authorization header's value after its scheme, a credential field's value), so only a
// redaction of that very token can hide it, and then the request whole. It keeps the credentials
// of every request and of the last one apart, since x/oauth2 probes a client's auth style with a
// second request after a first one fails, and the error names the second's answer.
type tokenEcho struct {
	mu        sync.Mutex
	all, last []string
}

func (e *tokenEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	e.all, e.last = append(e.all, bare...), bare
	e.mu.Unlock()
	var page strings.Builder
	page.WriteString("<!DOCTYPE html><html><head><title>502</title></head><body><h1>Bad Gateway</h1><pre>\n")
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
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, page.String())
}

// credentials is every credential the stand-in showed, and those of the last request alone.
func (e *tokenEcho) credentials() (all, last []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.all...), append([]string(nil), e.last...)
}

// echoIssuer is a sign-in issuer whose discovery document is its own and whose token endpoint is
// a tokenEcho.
func echoIssuer(t *testing.T) (string, *tokenEcho) {
	t.Helper()
	stand := &tokenEcho{}
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token",
			"revocation_endpoint": server.URL + "/revoke", "jwks_uri": server.URL + "/keys",
		})
	})
	mux.Handle("POST /token", stand)
	return server.URL, stand
}

// assertNamedWithoutCredentials fails the row when err's text holds any credential the stand-in
// showed or any of others, or, where the boundary printed the page's words (redact.OAuth2), does
// not redact each of that request's bare credentials in them.
func assertNamedWithoutCredentials(t *testing.T, err error, stand *tokenEcho, others ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the call through the stand-in succeeded")
	}
	all, last := stand.credentials()
	if len(last) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	printed := err.Error()
	t.Logf("the error: %s", printed)
	for _, credential := range append(others, all...) {
		if strings.Contains(printed, credential) {
			t.Errorf("the error carries the credential %q the request sent:\n%s", credential, printed)
		}
	}
	if n := strings.Count(printed, "[redacted]"); n < len(last) && !strings.Contains(printed, "(not printed: ") {
		t.Errorf("the error redacts %d places, want at least the %d bare credentials:\n%s", n, len(last), printed)
	}
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil || re.Response.StatusCode != http.StatusBadGateway {
		t.Errorf("the error %v no longer unwraps to the token endpoint's RetrieveError", err)
	}
}

// echoSecret is the client secret the check sends.
const echoSecret = "issuer-client-secret-RDX-71c3"

// TestProxyEchoTheClientCheckNamesThePageWithoutItsCredentials: the check's token request carries
// the client secret and a code, and a page that is no OAuth error says nothing about the client.
func TestProxyEchoTheClientCheckNamesThePageWithoutItsCredentials(t *testing.T) {
	issuer, stand := echoIssuer(t)
	check, err := CheckClient(context.Background(), issuer, flowClientID, echoSecret)
	if check != ClientCheckIssuerUnreachable {
		t.Errorf("the check = %q, want %q: a gateway's page says nothing about the client", check, ClientCheckIssuerUnreachable)
	}
	assertNamedWithoutCredentials(t, err, stand, echoSecret)
}
