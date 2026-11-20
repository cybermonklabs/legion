package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// outsideRuleListingEveryone signs rigDomain's people in as full members, and admits every address
// as a restricted member: its lister names every address, since these tests are about which origin
// answers, not about who is listed.
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

// A sign-in started on the outside origin's Host comes back to that origin's callback, and one
// started on the canonical Host to the canonical callback; each asks Google for hd, the domain whose
// people alone sign in with Google. With no outside origin, the canonical Host's redirect is byte
// for byte today's.
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
		if got := location.Query().Get("hd"); got != rigDomain {
			t.Errorf("GET %s: the issuer is asked for hd %q, want %q", target, got, rigDomain)
		}
	}

	off := newOriginRig(t, identity.DomainRule{Domain: rigDomain}, "")
	location, _ := off.start(t, "http://canon.test/auth/start")
	query := location.Query()
	want := off.issuer.URL() + "/authorize?access_type=offline&client_id=" + rigClientID + "&hd=" + rigDomain +
		"&nonce=" + url.QueryEscape(query.Get("nonce")) + "&prompt=consent&redirect_uri=" + url.QueryEscape(canonicalOrigin+"/auth/callback") +
		"&response_type=code&scope=openid+email+profile&state=" + url.QueryEscape(query.Get("state"))
	if got := location.String(); got != want {
		t.Errorf("GET /auth/start with no outside origin: Location\n%s\nwant today's\n%s", got, want)
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

// signInAt runs a whole Google sign-in through the rig's router at host, as the account claims
// names: the start and the callback are both requests whose Host is host, so each comes back to
// that origin's callback.
func (r *signInRig) signInAt(t *testing.T, host string, claims map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	r.issuer.SignInAs(claims)
	authURL, stateCookie := r.start(t, "http://"+host+"/auth/start")
	code, state := r.issuer.Authorize(t, authURL.String())
	callback := httptest.NewRequest(http.MethodGet, "http://"+host+"/auth/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), nil)
	callback.AddCookie(stateCookie)
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, callback)
	return response
}

// A Google sign-in of an address outside the domain is refused at both origins, whatever a list
// names: under the rule that admits restricted members by their listing, with a list naming every
// address, a verified Google account outside rigDomain gets the refusal page saying it is not in
// the domain, no session cookie and no people row, at the canonical origin and at the outside
// origin. The control: an account of rigDomain signs in at each.
func TestAGoogleSignInOfAnOutsideAddressIsRefusedAtBothOrigins(t *testing.T) {
	rig := newOriginRig(t, outsideRuleListingEveryone(), outsideOrigin)
	for _, host := range []string{"canon.test", "outside.test"} {
		refused := rig.signInAt(t, host, map[string]any{"sub": "outside-subject", "email": "Stranger@Other.example", "email_verified": true})
		if refused.Code != http.StatusForbidden || sessionCookie(refused) != nil ||
			!strings.Contains(refused.Body.String(), "stranger@other.example is not in the domain that may use Dispatch") {
			t.Errorf("a Google sign-in of stranger@other.example at %s answered %d %s, want the 403 page saying it is not in the domain, with no session", host, refused.Code, refused.Body.String())
		}
		if person, found := rig.people.get("stranger@other.example"); found {
			t.Errorf("the refused sign-in at %s recorded stranger@other.example as %+v, want no row", host, person)
		}
		if member := rig.signInAt(t, host, googleClaims("google-subject")); member.Code != http.StatusFound || sessionCookie(member) == nil {
			t.Errorf("the control, an account of %s, at %s answered %d %s, want a signed-in redirect", rigDomain, host, member.Code, member.Body.String())
		}
	}
}
