package identity

// A gateway or a proxy in the sign-in issuer's token endpoint's place can answer a lapsed
// membership's refresh with a page of its own that shows the request back: Dispatch's client
// secret (on its Basic header, then in the form x/oauth2 retries with) and the person's refresh
// token. x/oauth2 keeps that page whole in the *oauth2.RetrieveError the refresh returns, and
// Confirm logged that error and returned it. This row runs a real refresh against such a page and
// reads the log line and the error.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// refreshEcho is a stand-in for whatever answers in the token endpoint's place: it answers 502
// with an HTML page that shows each credential the request carried bare on a line of its own (the
// Authorization header's value after its scheme, the client secret and the refresh token in the
// form), then the request whole. It keeps every credential it showed, and the last request's
// apart, since x/oauth2 probes the client's auth style with a second request after a first fails.
type refreshEcho struct {
	mu        sync.Mutex
	all, last []string
}

func (e *refreshEcho) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var bare []string
	for _, value := range r.Header.Values("Authorization") {
		if _, credential, ok := strings.Cut(value, " "); ok {
			bare = append(bare, credential)
		}
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media == "application/x-www-form-urlencoded" {
		form, _ := url.ParseQuery(string(body))
		bare = append(append(bare, form["client_secret"]...), form["refresh_token"]...)
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

// TestProxyEchoALapsedMembershipLogsNoCredentialOfItsRefresh: a lapsed membership whose refresh a
// gateway answers with an echo page is refused with ErrSignInUnavailable and keeps the sign-in, as
// any refresh the issuer could not answer does, and neither the log line nor the error carries the
// client secret, its Basic pair or the refresh token; the page is named through the excerpt.
func TestProxyEchoALapsedMembershipLogsNoCredentialOfItsRefresh(t *testing.T) {
	const secret, refresh = "dispatch-client-secret-RDX-6a5b", "person-refresh-RDX-3c2d1e0f"
	stand := &refreshEcho{}
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

	sessions := &testSessionStore{generations: map[string]int64{memberEmail: 0}}
	people := newTestPeopleStore(sessions)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	people.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	rule := GroupRule{Group: memberGroup}
	if err := rule.WriteSignIn(context.Background(), people, memberEmail, "", refresh); err != nil {
		t.Fatalf("record sign-in: %v", err)
	}
	clock.Lock()
	now = now.Add(2 * time.Hour)
	clock.Unlock()
	membership := &Membership{People: people, Sessions: sessions, SignIn: oidc.NewCodeFlow(issuer.URL, "dispatch-client", secret, ""), Rule: rule}

	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	err := membership.Confirm(context.Background(), memberEmail)
	if !errors.Is(err, ErrSignInUnavailable) || !errors.Is(err, oidc.ErrIssuerUnavailable) || errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Confirm = %v, want ErrSignInUnavailable carrying oidc.ErrIssuerUnavailable", err)
	}
	if kept, _ := people.get(memberEmail); kept.RefreshToken != refresh || sessions.generation(memberEmail) != 0 {
		t.Fatalf("a gateway's page ended the sign-in: %#v, generation %d", kept, sessions.generation(memberEmail))
	}
	stand.mu.Lock()
	all, last := append([]string(nil), stand.all...), append([]string(nil), stand.last...)
	stand.mu.Unlock()
	if len(last) == 0 {
		t.Fatal("the stand-in showed no credential, so this row could not fail")
	}
	line := logged.Lines("dispatch: the sign-in issuer could not confirm a lapsed membership")
	if line == "" {
		t.Fatalf("the failed refresh was not logged; the log holds:\n%s", logged.String())
	}
	for where, printed := range map[string]string{"the log line": line, "the error": err.Error()} {
		t.Logf("%s: %s", where, printed)
		for _, credential := range append(all, secret, refresh) {
			if strings.Contains(printed, credential) {
				t.Errorf("%s carries the credential %q the refresh sent:\n%s", where, credential, printed)
			}
		}
		// The boundary names the page (redact.OAuth2): its words with the request's credentials
		// redacted, or, where the rule cannot read them, not at all.
		if n := strings.Count(printed, "[redacted]"); n < len(last) && !strings.Contains(printed, "(not printed: ") {
			t.Errorf("%s redacts %d places, want at least the %d bare credentials:\n%s", where, n, len(last), printed)
		}
	}
}
