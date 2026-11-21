package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
)

// twoPeopleRouter is a router on a cookie identity at https://dispatch.example holding a current
// session for alice and one for mallory, with the value of each one's session cookie.
func twoPeopleRouter(t *testing.T) (handler http.Handler, alice, mallory string) {
	t.Helper()
	people := newMemoryPeople()
	sessions := &memorySessionStore{generations: map[string]int64{}}
	values := map[string]string{}
	for _, email := range []string{"alice@d.example", "mallory@d.example"} {
		if err := people.SignIn(context.Background(), email, "refresh-token"); err != nil {
			t.Fatalf("record %s's sign-in: %v", email, err)
		}
		sessions.generations[email] = 0
		cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(email, 0, testSigningKey, true))
		if err != nil {
			t.Fatalf("parse %s's session cookie: %v", email, err)
		}
		values[email] = cookie.Value
	}
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:  testSigningKey,
		People:      people,
		Sessions:    sessions,
		Identity:    identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		AgentTokens: sharedAgentTokens(t, "agent-token"),
		ServerURL:   "https://dispatch.example",
		Lifetime:    t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ = New(ctx)
	return handler, values["alice@d.example"], values["mallory@d.example"]
}

// A browser signed in before the rename holds `dsession`, and one signed in after it
// `__Host-dsession`; GET /auth/whoami names the person of either. One holding both, for two
// people, is the person of `__Host-dsession`: a `dsession` another host under the domain planted
// beside a victim's session signs them in as nobody else (login CSRF).
//
// Control: read `dsession` alone (auth.sessionCookieNames), and every row carrying
// `__Host-dsession` fails.
func TestWhoamiReadsTheHostCookieBeforeTheOldName(t *testing.T) {
	handler, alice, mallory := twoPeopleRouter(t)
	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
		login   string
	}{
		{name: "only dsession, issued before the rename", cookies: []*http.Cookie{{Name: "dsession", Value: alice}}, login: "alice@d.example"},
		{name: "only __Host-dsession", cookies: []*http.Cookie{{Name: "__Host-dsession", Value: alice}}, login: "alice@d.example"},
		{name: "alice's __Host-dsession, and mallory's dsession first", cookies: []*http.Cookie{{Name: "dsession", Value: mallory}, {Name: "__Host-dsession", Value: alice}}, login: "alice@d.example"},
		{name: "alice's __Host-dsession first, and mallory's dsession", cookies: []*http.Cookie{{Name: "__Host-dsession", Value: alice}, {Name: "dsession", Value: mallory}}, login: "alice@d.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://dispatch.example/auth/whoami", nil)
			for _, cookie := range tc.cookies {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"`+tc.login+`"`) {
				t.Errorf("GET /auth/whoami carrying %v answered %d %s, want 200 naming %s", tc.cookies, response.Code, response.Body.String(), tc.login)
			}
		})
	}
}

// A write that carries a session cookie, under either name, must come from the server's own
// origin (enforceCookieOrigin, through auth.HasSessionCookie): a page of another site that makes
// the browser send `__Host-dsession` alone is refused 403 as one sending `dsession` is.
//
// Control: auth.HasSessionCookie reading `dsession` alone, and the `__Host-dsession` row reaches
// the sign-out, which answers 200 and signs alice out.
func TestAWriteCarryingEitherSessionCookieMustComeFromTheServersOrigin(t *testing.T) {
	for _, name := range []string{"__Host-dsession", "dsession"} {
		t.Run(name, func(t *testing.T) {
			handler, alice, _ := twoPeopleRouter(t)
			request := httptest.NewRequest(http.MethodPost, "https://dispatch.example/auth/logout", nil)
			request.AddCookie(&http.Cookie{Name: name, Value: alice})
			request.Header.Set("Origin", "https://other.dispatch.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "invalid request origin") {
				t.Errorf("a foreign-origin sign-out carrying %s answered %d %s, want 403 invalid request origin", name, response.Code, response.Body.String())
			}
		})
	}
}
