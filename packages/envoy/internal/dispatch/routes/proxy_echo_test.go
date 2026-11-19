package routes

// A gateway or a proxy in the sign-in issuer's token endpoint's place can answer the callback's
// code exchange with a page of its own that shows the request back: Dispatch's client secret (on
// its Basic header, then in the form x/oauth2 retries with) and the person's code. The callback
// sent the exchange's error text to the browser as `sign-in failed: <error>` and logged it, and
// that text held the page whole. This row runs a real sign-in against such a page and reads the
// browser's answer and the log line.

import (
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

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// exchangeEcho is a stand-in for whatever answers in the token endpoint's place: it answers 502
// with an HTML page that shows each credential the request carried bare on a line of its own (the
// Authorization header's value after its scheme, the client secret and the code in the form),
// then the request whole. It keeps every credential it showed, and the last request's apart.
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
		bare = append(append(bare, form["client_secret"]...), form["code"]...)
	}
	e.mu.Lock()
	e.all, e.last = append(e.all, bare...), bare
	e.mu.Unlock()
	var page strings.Builder
	page.WriteString("<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center><pre>\n")
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

// TestProxyEchoASignInCallbackAnswersTheBrowserNoCredential: a callback whose exchange a gateway
// answers with an echo page answers the browser 502 naming the page through the excerpt, and
// neither that answer nor the log line carries the client secret, its Basic header or the code.
func TestProxyEchoASignInCallbackAnswersTheBrowserNoCredential(t *testing.T) {
	const secret, code = "dispatch-client-secret-RDX-9e8d", "person-code-RDX-1f2e3d4c"
	stand := &exchangeEcho{}
	mux := http.NewServeMux()
	issuer := httptest.NewServer(mux)
	t.Cleanup(issuer.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token",
			"revocation_endpoint": issuer.URL + "/revoke", "jwks_uri": issuer.URL + "/keys",
		})
	})
	mux.Handle("POST /token", stand)

	flow := oidc.NewCodeFlow(issuer.URL, rigClientID, secret, "")
	rule := identity.GroupRule{Group: rigGroup}
	rig := &signInRig{people: newMemoryPeople(), sessions: &memorySessionStore{generations: map[string]int64{}}}
	rig.people.sessions = rig.sessions
	membership := &identity.Membership{People: rig.people, Sessions: rig.sessions, SignIn: flow, Rule: rule}
	var err error
	rig.ctx, err = BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey, People: rig.people, Sessions: rig.sessions,
		Identity: identity.CookieIdentity{SigningKey: testSigningKey, Sessions: rig.sessions, Membership: membership},
		SignIn:   flow, SignInRule: rule, Lifetime: t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(rig.ctx, 0)
	rig.handler, _ = New(rig.ctx)

	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	location, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
	response := callbackOn(rig.handler, code, location.Query().Get("state"), stateCookie)
	if response.Code != http.StatusBadGateway || sessionCookie(response) != nil {
		t.Fatalf("callback = %d %s, want 502 and no session", response.Code, response.Body.String())
	}
	stand.mu.Lock()
	all, last := append([]string(nil), stand.all...), append([]string(nil), stand.last...)
	stand.mu.Unlock()
	if len(last) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	line := logged.Lines("dispatch: sign-in code exchange failed")
	if line == "" {
		t.Fatalf("the exchange's failure was not logged; the log holds:\n%s", logged.String())
	}
	for where, printed := range map[string]string{"the browser's answer": response.Body.String(), "the log line": line} {
		t.Logf("%s: %s", where, printed)
		for _, credential := range append(all, secret, code) {
			if strings.Contains(printed, credential) {
				t.Errorf("%s carries the credential %q the exchange sent:\n%s", where, credential, printed)
			}
		}
		// The boundary names the page (redact.OAuth2): its words with the request's credentials
		// redacted, or, where the rule cannot read them, not at all.
		if n := strings.Count(printed, "[redacted]"); n < len(last) && !strings.Contains(printed, "(not printed: ") {
			t.Errorf("%s redacts %d places, want at least the %d bare credentials:\n%s", where, n, len(last), printed)
		}
	}
}
