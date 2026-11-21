package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

const (
	// canonicalOrigin is the rig's DISPATCH_SERVER_URL and outsideOrigin its
	// DISPATCH_OUTSIDE_ORIGIN, the second origin it answers at.
	canonicalOrigin = "https://canon.test"
	outsideOrigin   = "https://outside.test"
)

// outsideRuleListingEveryone signs rigDomain's people in as full members, and every verified
// Google account outside it as a restricted member: its lister names every address, since these
// tests are about which origin answers, not about who is listed.
func outsideRuleListingEveryone() identity.OutsideRule {
	return identity.OutsideRule{
		DomainRule: identity.DomainRule{Domain: rigDomain},
		People:     identity.ListerFunc(func(context.Context, string) (bool, error) { return true, nil }),
	}
}

// newOriginRig is a router at canonicalOrigin that also answers at outside (empty for none),
// signing people in under rule through a fake issuer that mints Google's claims for rigDomain.
func newOriginRig(t *testing.T, rule identity.SignInRule, outside string) *signInRig {
	t.Helper()
	rig := newSignInRigFor(t, canonicalOrigin, rule, rigDomain)
	rig.options.OutsideOrigin = outside
	ctx, err := BuildAppContext(rig.options)
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	rig.ctx = ctx
	rig.handler, _ = New(ctx)
	return rig
}

// With outside sign-in on, a sign-in started on the outside origin's Host comes back to that
// origin's callback, and one started on the canonical Host to the canonical callback; neither asks
// Google for hd. With it off, the canonical Host's redirect is byte for byte today's, hd included.
func TestOutsideSignInStartsAtTheOriginOfItsOwnHost(t *testing.T) {
	rig := newOriginRig(t, outsideRuleListingEveryone(), outsideOrigin)
	for target, want := range map[string]string{
		"http://outside.test/auth/start": outsideOrigin + "/auth/callback",
		"http://OUTSIDE.test/auth/start": outsideOrigin + "/auth/callback",
		"http://canon.test/auth/start":   canonicalOrigin + "/auth/callback",
	} {
		location, _ := rig.start(t, target)
		if got := location.Query().Get("redirect_uri"); got != want {
			t.Errorf("GET %s: redirect_uri %q, want %q", target, got, want)
		}
		if location.Query().Has("hd") {
			t.Errorf("GET %s with outside sign-in on: the issuer is asked for hd %q, want no hd on any origin", target, location.Query().Get("hd"))
		}
	}

	off := newOriginRig(t, identity.DomainRule{Domain: rigDomain}, "")
	location, _ := off.start(t, "http://canon.test/auth/start")
	query := location.Query()
	want := off.issuer.URL() + "/authorize?access_type=offline&client_id=" + rigClientID + "&hd=" + rigDomain +
		"&nonce=" + url.QueryEscape(query.Get("nonce")) + "&prompt=consent&redirect_uri=" + url.QueryEscape(canonicalOrigin+"/auth/callback") +
		"&response_type=code&scope=openid+email+profile&state=" + url.QueryEscape(query.Get("state"))
	if got := location.String(); got != want {
		t.Errorf("GET /auth/start with outside sign-in off: Location\n%s\nwant today's\n%s", got, want)
	}
}

// A browser write carrying a session cookie passes the origin check only when its Origin is the
// origin of its own Host: the outside origin on the outside Host (in any case), the canonical
// origin on the canonical Host, and nothing across them or from elsewhere. The write is a sign-out,
// so each case signs in afresh.
func TestOutsideSignInCookieOriginCheckTakesEachOriginOnItsOwnHost(t *testing.T) {
	rig := newOriginRig(t, outsideRuleListingEveryone(), outsideOrigin)
	for _, test := range []struct {
		host, origin string
		passes       bool
	}{
		{"outside.test", outsideOrigin, true},
		{"OUTSIDE.test", outsideOrigin, true},
		{"canon.test", canonicalOrigin, true},
		{"canon.test", outsideOrigin, false},
		{"outside.test", canonicalOrigin, false},
		{"outside.test", "https://evil.test", false},
	} {
		cookie := sessionCookie(rig.signIn(t, googleClaims("google-subject")))
		if cookie == nil {
			t.Fatal("the sign-in issued no session cookie")
		}
		request := httptest.NewRequest(http.MethodPost, "http://"+test.host+"/auth/logout", nil)
		request.Header.Set("Origin", test.origin)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		refused := response.Code == http.StatusForbidden && strings.Contains(response.Body.String(), "invalid request origin")
		if test.passes && response.Code != http.StatusOK {
			t.Errorf("POST /auth/logout, Host %s, Origin %s: %d %s, want the write served", test.host, test.origin, response.Code, response.Body.String())
		}
		if !test.passes && !refused {
			t.Errorf("POST /auth/logout, Host %s, Origin %s: %d %s, want 403 invalid request origin", test.host, test.origin, response.Code, response.Body.String())
		}
	}
}

// whoamiAt answers GET /auth/whoami's raw body for alice, a full member, from a router at
// canonicalOrigin that also answers at outside (empty for none).
func whoamiAt(t *testing.T, outside string) (int, string) {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `insert into people (email) values ('alice@example.com')`); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	people := newMemoryPeople()
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey: testSigningKey, People: people, Store: database, Lifetime: t.Context(),
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		ServerURL:     canonicalOrigin,
		OutsideOrigin: outside,
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, _ := New(ctx)
	return rawWhoami(t, handler, "alice@example.com")
}

// /auth/whoami names both origins, the canonical one first, where the server answers at two; where
// it answers at one, a full member's answer is byte for byte today's, with no origins key (R9).
func TestOutsideSignInWhoamiAnswersBothOrigins(t *testing.T) {
	if status, body := whoamiAt(t, outsideOrigin); status != http.StatusOK ||
		body != `{"event_head":"0","kind":"user","login":"alice@example.com","origins":["`+canonicalOrigin+`","`+outsideOrigin+`"]}` {
		t.Errorf("whoami with an outside origin = %d %s, want both origins, the canonical one first", status, body)
	}
	if status, body := whoamiAt(t, ""); status != http.StatusOK || body != `{"event_head":"0","kind":"user","login":"alice@example.com"}` {
		t.Errorf("whoami with no outside origin = %d %s, want today's three keys and no origins", status, body)
	}
}

// The outside origin's pages - the enrollment page an owner's link opens, the passkey sign-in at
// its root - say no page may frame them, in both headers a browser reads that from, and that they
// load no plugin object and take no <base>. The policy is written out here rather than read from
// the constant, so a change to it fails this row. The canonical origin's same pages carry neither
// header, as before: the control that the reading tells the two origins apart.
func TestTheOutsideOriginsPagesRefuseEveryFrame(t *testing.T) {
	rig := newOriginRig(t, outsideRuleListingEveryone(), outsideOrigin)
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatalf("write dashboard index: %v", err)
	}
	rig.ctx.WebDistDir = webDist
	for _, path := range []string{"/enroll", "/", "/projects/PRIV"} {
		for origin, want := range map[string]struct{ policy, frameOptions string }{
			outsideOrigin:   {policy: "frame-ancestors 'none'; object-src 'none'; base-uri 'none'", frameOptions: "DENY"},
			canonicalOrigin: {},
		} {
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, origin+path, nil))
			policy, frameOptions := response.Header().Get("Content-Security-Policy"), response.Header().Get("X-Frame-Options")
			if response.Code != http.StatusOK || policy != want.policy || frameOptions != want.frameOptions {
				t.Errorf("GET %s%s = %d, Content-Security-Policy %q, X-Frame-Options %q; want 200, %q, %q",
					origin, path, response.Code, policy, frameOptions, want.policy, want.frameOptions)
			}
		}
	}
}

// A signed-out GET /auth/whoami at the outside origin names the passkey sign-in wherever that
// origin serves no Google sign-in (googleSignInHere's condition), so its page offers no Google
// link that answers 404 there: under the domain rule it names "passkey". Under the outside rule
// (DISPATCH_SIGNIN_OUTSIDE=1), which serves Google there, it names none, and the page offers that
// Google sign-in. The canonical origin's 401 names none under either rule.
func TestTheOutsideOriginsWhoamiNamesTheSignInItServes(t *testing.T) {
	const plain = `{"code":"NO_IDENTITY","error":"unauthorized"}`
	for _, test := range []struct {
		name    string
		rule    identity.SignInRule
		outside string
	}{
		{"the outside rule", outsideRuleListingEveryone(), plain},
		{"the domain rule", identity.DomainRule{Domain: rigDomain}, `{"code":"NO_IDENTITY","error":"unauthorized","sign_in":"passkey"}`},
	} {
		rig := newOriginRig(t, test.rule, outsideOrigin)
		for origin, want := range map[string]string{outsideOrigin: test.outside, canonicalOrigin: plain} {
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, origin+"/auth/whoami", nil))
			if body := strings.TrimSpace(response.Body.String()); response.Code != http.StatusUnauthorized || body != want {
				t.Errorf("GET %s/auth/whoami signed out under %s = %d %s, want 401 %s", origin, test.name, response.Code, body, want)
			}
		}
	}
}

// A refused outside sign-in offers a link that asks Google for its account chooser, so a browser
// holding one Google session is not sent straight back with the account just refused; the
// sign-in a person starts any other way asks for consent alone, and a refusal under the domain
// rule alone keeps today's plain link.
func TestOutsideSignInRefusalOffersGooglesAccountChooser(t *testing.T) {
	unlisted := identity.OutsideRule{
		DomainRule: identity.DomainRule{Domain: rigDomain},
		People:     identity.ListerFunc(func(context.Context, string) (bool, error) { return false, nil }),
	}
	outsider := map[string]any{"sub": "outside-subject", "email": "stranger@other.example", "email_verified": true}
	refusalLink := func(rig *signInRig) string {
		t.Helper()
		page := rig.signIn(t, outsider)
		if page.Code != http.StatusForbidden {
			t.Fatalf("the sign-in of an account no list names answered %d %s, want the refusal page", page.Code, page.Body.String())
		}
		_, after, found := strings.Cut(page.Body.String(), `<a href="`)
		link, _, closed := strings.Cut(after, `"`)
		if !found || !closed {
			t.Fatalf("the refusal page holds no link: %s", page.Body.String())
		}
		return link
	}
	prompt := func(rig *signInRig, target string) string {
		t.Helper()
		location, _ := rig.start(t, target)
		return location.Query().Get("prompt")
	}

	rig := newOriginRig(t, unlisted, outsideOrigin)
	link := refusalLink(rig)
	if link != "/auth/start?prompt=select_account" {
		t.Fatalf("the refused outside sign-in's link is %q, want /auth/start?prompt=select_account", link)
	}
	if got := prompt(rig, "http://outside.test"+link); got != "select_account consent" {
		t.Errorf("GET %s: the issuer is asked prompt=%q, want the chooser beside the consent a refresh token needs", link, got)
	}
	if got := prompt(rig, "http://outside.test/auth/start"); got != "consent" {
		t.Errorf("GET /auth/start: the issuer is asked prompt=%q, want consent alone", got)
	}
	if domainOnly := refusalLink(newOriginRig(t, identity.DomainRule{Domain: rigDomain}, "")); domainOnly != "/auth/start" {
		t.Errorf("a refusal under the domain rule alone links to %q, want today's /auth/start", domainOnly)
	}
}
