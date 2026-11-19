package approval

// A gateway or a proxy in the sign-in provider's token endpoint's place can answer the approval
// sign-in's code exchange with a page of its own that shows the request back: the broker's client
// secret (on its Basic header, then in the form x/oauth2 retries with), the person's code and the
// PKCE verifier. x/oauth2 keeps that page whole in the *oauth2.RetrieveError the exchange returns,
// and the callback logged that error twice. This row runs a real callback against such a page
// and reads both log lines and the browser's answer.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// exchangeEcho is a stand-in for whatever answers in the token endpoint's place: it answers 502
// with an HTML page that shows each credential the request carried bare on a line of its own (the
// Authorization header's value after its scheme, the client secret, the code and the verifier in
// the form), then the request whole. It keeps every credential it showed, and the last request's
// apart, since x/oauth2 probes the client's auth style with a second request after a first fails.
type exchangeEcho struct {
	mu        sync.Mutex
	all, last []string
}

func (e *exchangeEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var bare []string
	for _, value := range r.Header.Values("Authorization") {
		if _, credential, ok := strings.Cut(value, " "); ok {
			bare = append(bare, credential)
		}
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media == "application/x-www-form-urlencoded" {
		form, _ := url.ParseQuery(string(body))
		for _, field := range []string{"client_secret", "code", "code_verifier"} {
			bare = append(bare, form[field]...)
		}
	}
	e.mu.Lock()
	e.all, e.last = append(e.all, bare...), bare
	e.mu.Unlock()
	var page strings.Builder
	page.WriteString("<html><head><title>502 Bad Gateway</title></head><body><h1>502 Bad Gateway</h1><pre>\n")
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
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, page.String())
}

// TestProxyEchoASignInCallbackLogsNoCredentialOfItsExchange: a callback whose exchange a gateway
// answers with an echo page is refused 403, and neither log line it writes nor the page it answers
// carries the client secret, its Basic pair, the code or the PKCE verifier; each log line names
// the gateway's page through the excerpt.
func TestProxyEchoASignInCallbackLogsNoCredentialOfItsExchange(t *testing.T) {
	const secret, code = "broker-signin-secret-RDX-4d3c", "person-code-RDX-9a8b7c6d"
	verifier := strings.Repeat("v", 20) + "-verifier-RDX-1e2f3a"
	stand := &exchangeEcho{}
	mux := http.NewServeMux()
	issuer := httptest.NewServer(mux)
	t.Cleanup(issuer.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token",
			"jwks_uri": issuer.URL + "/keys",
		})
	})
	mux.Handle("POST /token", stand)
	const public = "https://broker.example"
	page := &Page{
		cfg:    Config{PublicURL: public, SignIn: SignIn{Issuer: issuer.URL, ClientID: "broker-client", ClientSecret: secret, Domain: "example.com"}},
		origin: public, redirectURL: public + "/signin/callback",
		signIn: oidc.NewCodeFlow(issuer.URL, "broker-client", secret, "example.com"),
	}

	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	pending, err := json.Marshal(pendingSignIn{State: "a-state", Nonce: "a-nonce", Verifier: verifier, Next: approvePrefix + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, public+"/signin/callback?"+url.Values{"code": {code}, "state": {"a-state"}}.Encode(), nil)
	callback.AddCookie(&http.Cookie{Name: signInCookie, Value: base64.RawURLEncoding.EncodeToString(pending)})
	answer := httptest.NewRecorder()
	page.ServeCallback(answer, callback, Session{})
	if answer.Code != http.StatusForbidden {
		t.Fatalf("the callback = %d, want 403:\n%s", answer.Code, answer.Body.String())
	}
	stand.mu.Lock()
	all, last := append([]string(nil), stand.all...), append([]string(nil), stand.last...)
	stand.mu.Unlock()
	if len(last) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	printed := map[string]string{"the page": answer.Body.String()}
	for _, message := range []string{
		"approval: a failed sign-in that no status code names",
		"approval sign-in refused: the code exchange or the ID token failed",
	} {
		line := logged.Lines(message)
		if line == "" {
			t.Fatalf("no %q line was logged; the log holds:\n%s", message, logged.String())
		}
		printed[message] = line
		// The boundary names the page (redact.OAuth2): its words with the request's credentials
		// redacted, or, where the rule cannot read them, not at all.
		if n := strings.Count(line, "[redacted]"); n < len(last) && !strings.Contains(line, "(not printed: ") {
			t.Errorf("the %q line redacts %d places, want at least the %d bare credentials:\n%s", message, n, len(last), line)
		}
	}
	for where, text := range printed {
		t.Logf("%s: %s", where, text)
		for _, credential := range append(all, secret, code, verifier) {
			if strings.Contains(text, credential) {
				t.Errorf("%s carries the credential %q the exchange sent:\n%s", where, credential, text)
			}
		}
	}
}
