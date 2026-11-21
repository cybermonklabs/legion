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

// canonicalSessionCookie is the name a sign-in at the canonical origin issues the session cookie
// under, Secure or not: `dsession`, as every earlier build issues it, so the build before this one
// reads it in a roll and after a rollback (auth.SetSessionCookie).
const canonicalSessionCookie = "dsession"

// The router of twoPeopleRouter answers at these two origins.
const (
	namesCanonicalOrigin = "https://dispatch.example"
	namesOutsideOrigin   = "https://outside.dispatch.example"
)

// twoPeopleRouter is twoPeopleRouterAt namesOutsideOrigin, with the cookie Secure.
func twoPeopleRouter(t *testing.T) (handler http.Handler, alice, mallory string) {
	t.Helper()
	return twoPeopleRouterAt(t, namesOutsideOrigin, false)
}

// twoPeopleRouterAt is a router on a cookie identity at namesCanonicalOrigin, also answering at
// outside, under DISPATCH_INSECURE_COOKIE when insecure is true, holding a current session for alice
// and one for mallory, with the value of each one's session cookie.
func twoPeopleRouterAt(t *testing.T, outside string, insecure bool) (handler http.Handler, alice, mallory string) {
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
		SigningKey:     testSigningKey,
		People:         people,
		Sessions:       sessions,
		Identity:       identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions},
		AgentTokens:    sharedAgentTokens(t, "agent-token"),
		ServerURL:      namesCanonicalOrigin,
		OutsideOrigin:  outside,
		InsecureCookie: insecure,
		Lifetime:       t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ = New(ctx)
	return handler, values["alice@d.example"], values["mallory@d.example"]
}

// whoamiCarrying answers GET /auth/whoami at origin from handler, carrying cookies in their order.
func whoamiCarrying(handler http.Handler, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, origin+"/auth/whoami", nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// At the canonical origin a browser signed in by an earlier build holds `dsession`, and one a later
// build signed in `__Host-dsession`; GET /auth/whoami names the person of either. One holding both,
// for two people, is the person of `__Host-dsession`: a `dsession` another host under the domain
// planted beside a victim's session signs them in as nobody else (login CSRF).
//
// At the outside origin, which no released build has served, the session cookie has one name,
// `__Host-dsession` (the outside origin is https): a `dsession` alone there is no session, and
// beside a `__Host-dsession` it changes nothing. That holds at every port of the outside origin's
// host, since a browser sends one host's cookies to every port of it.
//
// Controls: read `dsession` alone (auth.sessionCookieValue), and every row carrying
// `__Host-dsession` fails; read the canonical origin's two names at the outside origin too, and its
// rows of a planted `dsession` alone fail; mark the outside origin by host and port, and the row at
// another port fails.
func TestWhoamiReadsTheSessionCookieOfItsOrigin(t *testing.T) {
	handler, alice, mallory := twoPeopleRouter(t)
	host := func(value string) *http.Cookie { return &http.Cookie{Name: "__Host-dsession", Value: value} }
	plain := func(value string) *http.Cookie { return &http.Cookie{Name: "dsession", Value: value} }
	for _, tc := range []struct {
		name    string
		origin  string
		cookies []*http.Cookie
		login   string // "" for none: 401
	}{
		{name: "canonical: only dsession, issued by an earlier build", origin: namesCanonicalOrigin, cookies: []*http.Cookie{plain(alice)}, login: "alice@d.example"},
		{name: "canonical: only __Host-dsession", origin: namesCanonicalOrigin, cookies: []*http.Cookie{host(alice)}, login: "alice@d.example"},
		{name: "canonical: alice's __Host-dsession, and mallory's dsession first", origin: namesCanonicalOrigin, cookies: []*http.Cookie{plain(mallory), host(alice)}, login: "alice@d.example"},
		{name: "canonical: alice's __Host-dsession first, and mallory's dsession", origin: namesCanonicalOrigin, cookies: []*http.Cookie{host(alice), plain(mallory)}, login: "alice@d.example"},
		{name: "outside: only __Host-dsession", origin: namesOutsideOrigin, cookies: []*http.Cookie{host(alice)}, login: "alice@d.example"},
		{name: "outside: only a planted dsession", origin: namesOutsideOrigin, cookies: []*http.Cookie{plain(mallory)}, login: ""},
		{name: "outside: alice's __Host-dsession, and mallory's dsession first", origin: namesOutsideOrigin, cookies: []*http.Cookie{plain(mallory), host(alice)}, login: "alice@d.example"},
		{name: "outside host, another port: only __Host-dsession", origin: namesOutsideOrigin + ":8443", cookies: []*http.Cookie{host(alice)}, login: "alice@d.example"},
		{name: "outside host, another port: only a planted dsession", origin: namesOutsideOrigin + ":8443", cookies: []*http.Cookie{plain(mallory)}, login: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := whoamiCarrying(handler, tc.origin, tc.cookies...)
			if tc.login == "" {
				if response.Code != http.StatusUnauthorized {
					t.Errorf("GET %s/auth/whoami carrying %v answered %d %s, want 401", tc.origin, tc.cookies, response.Code, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"`+tc.login+`"`) {
				t.Errorf("GET %s/auth/whoami carrying %v answered %d %s, want 200 naming %s", tc.origin, tc.cookies, response.Code, response.Body.String(), tc.login)
			}
		})
	}
}

// A write that carries a session cookie of its origin must come from that origin
// (enforceCookieOrigin, through auth.HasSessionCookie): a page of another site that makes the browser
// send either name at the canonical origin, or `__Host-dsession` at the outside origin's host, is
// refused 403. A `dsession` at the outside origin's host, at any port, is no session cookie there:
// the write passes the check and finds no person, 401, and signs nobody out.
//
// Controls: auth.HasSessionCookie reading `dsession` alone, and the canonical `__Host-dsession` row
// reaches the sign-out, which answers 200 and signs alice out; read the canonical origin's two names
// at the outside origin, and its `dsession` rows answer 403; mark the outside origin by host and
// port, and the row at another port answers 403.
func TestAWriteCarryingASessionCookieMustComeFromItsOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin, name string
		status       int
	}{
		{origin: namesCanonicalOrigin, name: "__Host-dsession", status: http.StatusForbidden},
		{origin: namesCanonicalOrigin, name: "dsession", status: http.StatusForbidden},
		{origin: namesOutsideOrigin, name: "__Host-dsession", status: http.StatusForbidden},
		{origin: namesOutsideOrigin, name: "dsession", status: http.StatusUnauthorized},
		{origin: namesOutsideOrigin + ":8443", name: "dsession", status: http.StatusUnauthorized},
	} {
		t.Run(tc.origin+" "+tc.name, func(t *testing.T) {
			handler, alice, _ := twoPeopleRouter(t)
			request := httptest.NewRequest(http.MethodPost, tc.origin+"/auth/logout", nil)
			request.AddCookie(&http.Cookie{Name: tc.name, Value: alice})
			request.Header.Set("Origin", "https://other.dispatch.example")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			refused := response.Code == http.StatusForbidden && strings.Contains(response.Body.String(), "invalid request origin")
			if response.Code != tc.status || tc.status == http.StatusForbidden && !refused {
				t.Errorf("a foreign-origin sign-out at %s carrying %s answered %d %s, want %d", tc.origin, tc.name, response.Code, response.Body.String(), tc.status)
			}
			if still := whoamiCarrying(handler, namesCanonicalOrigin, &http.Cookie{Name: "dsession", Value: alice}); still.Code != http.StatusOK {
				t.Errorf("after the foreign-origin sign-out, alice's session answered whoami %d %s, want 200: the write signed her out", still.Code, still.Body.String())
			}
		})
	}
}

// At the outside origin the session cookie follows that origin's scheme, not DISPATCH_INSECURE_COOKIE:
// at an https outside origin it is `__Host-dsession` even under the setting (a browser takes a
// Secure cookie there), and at an http one it is `dsession` even with the cookie Secure elsewhere (a
// browser refuses a `__Host-` cookie over plain http). Each is read there alone.
//
// Control: take the outside origin's name from the setting rather than its scheme, and every row
// fails.
func TestTheOutsideOriginsSessionCookieFollowsItsScheme(t *testing.T) {
	host := func(value string) *http.Cookie { return &http.Cookie{Name: "__Host-dsession", Value: value} }
	plain := func(value string) *http.Cookie { return &http.Cookie{Name: "dsession", Value: value} }
	const httpOutside = "http://outside.dispatch.example"
	for _, tc := range []struct {
		name     string
		outside  string
		insecure bool
		// read is the name the outside origin reads, and ignored the one it does not.
		read, ignored func(string) *http.Cookie
	}{
		{name: "https outside origin under DISPATCH_INSECURE_COOKIE", outside: namesOutsideOrigin, insecure: true, read: host, ignored: plain},
		{name: "http outside origin with the cookie Secure", outside: httpOutside, insecure: false, read: plain, ignored: host},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, alice, mallory := twoPeopleRouterAt(t, tc.outside, tc.insecure)
			if response := whoamiCarrying(handler, tc.outside, tc.read(alice)); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"login":"alice@d.example"`) {
				t.Errorf("GET %s/auth/whoami carrying %v answered %d %s, want 200 naming alice", tc.outside, tc.read(alice), response.Code, response.Body.String())
			}
			if response := whoamiCarrying(handler, tc.outside, tc.ignored(mallory)); response.Code != http.StatusUnauthorized {
				t.Errorf("GET %s/auth/whoami carrying %v answered %d %s, want 401", tc.outside, tc.ignored(mallory), response.Code, response.Body.String())
			}
		})
	}
}
