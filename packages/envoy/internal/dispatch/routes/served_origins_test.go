package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// newServedRig is a router at tableCanonical that also serves tableServed and served tableFormer
// before, signing the people of rigDomain in with Google through a fake issuer whose client lists
// both served callbacks, the canonical address's and tableServed's, as the Google client of such a
// server must.
func newServedRig(t *testing.T) *signInRig {
	t.Helper()
	rig := newSignInRigFor(t, tableCanonical, identity.DomainRule{Domain: rigDomain}, rigDomain)
	rig.issuer.EnableCodeFlow(rig.issuer.PublishKey(t, "served-signing-key"), rigClientID, "dispatch-secret", tableCanonical+"/auth/callback", tableServed+"/auth/callback")
	rig.options.ServedOrigins = []string{tableServed}
	rig.options.FormerOrigins = []string{tableFormer}
	ctx, err := BuildAppContext(withDocuments(t, rig.options))
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	standInEventLog(ctx, 0)
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	rig.ctx, rig.handler = ctx, handler
	return rig
}

// TestServedAddressTakesBrowserWritesAndSignIn (c-l-served 1, 2 and 3): a served address is an
// origin the server answers as itself, by the request's Host alone: its name in any case or with a
// trailing dot, and its port as a number, the scheme's default (443) equal to none. A cookie write
// there passes the origin check with that address's Origin, and a write at the canonical Host with
// the canonical Origin still does; a write at the served Host with the canonical Origin is refused,
// and so is one at the canonical Host whose X-Forwarded-Host names the served host, since a
// forwarded header chooses nothing, and one at another port of the served host (8443), which is a
// host of its own. A Google sign-in started at the served Host sends the issuer that address's
// callback and ends there with the canonical address's session cookie, host-only; one started at
// the canonical Host, whatever X-Forwarded-Host says, comes back to the canonical callback.
func TestServedAddressTakesBrowserWritesAndSignIn(t *testing.T) {
	rig := newServedRig(t)
	for _, write := range []struct {
		host, origin string
		header       http.Header
		admitted     bool
	}{
		{"b.example.test", tableServed, nil, true},
		{"a.example.test", tableCanonical, nil, true},
		{"B.EXAMPLE.TEST", tableServed, nil, true},
		{"b.example.test.", tableServed, nil, true},
		{"b.example.test:443", tableServed, nil, true},
		{"b.example.test", "", http.Header{"Sec-Fetch-Site": {"same-origin"}}, true},
		{"b.example.test", tableCanonical, nil, false},
		{"a.example.test", tableServed, http.Header{"X-Forwarded-Host": {"b.example.test"}}, false},
		{"b.example.test:8443", tableServed, nil, false},
	} {
		requireWrite(t, "a cookie write with Origin "+write.origin+" to Host "+write.host+" "+headerNames(write.header),
			signedInWrite(t, rig, write.host, write.origin, write.header), write.admitted)
	}

	redirectURI, cookie := signInAt(t, rig, "b.example.test")
	if redirectURI != tableServed+"/auth/callback" {
		t.Errorf("GET /auth/start at Host b.example.test: redirect_uri %q, want the served address's callback %q", redirectURI, tableServed+"/auth/callback")
	}
	if cookie.Name != "dsession" || cookie.Domain != "" || cookie.Path != "/" {
		t.Errorf("the sign-in at the served address set %s with Domain %q and Path %q, want the canonical address's dsession, host-only at /", cookie.Name, cookie.Domain, cookie.Path)
	}
	forwarded := httptest.NewRequest(http.MethodGet, "https://a.example.test/auth/start", nil)
	forwarded.Header.Set("X-Forwarded-Host", "b.example.test")
	response := httptest.NewRecorder()
	rig.handler.ServeHTTP(response, forwarded)
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusFound || err != nil || location.Query().Get("redirect_uri") != tableCanonical+"/auth/callback" {
		t.Errorf("GET /auth/start at Host a.example.test, X-Forwarded-Host b.example.test: %d to %q, want a redirect_uri of the canonical callback", response.Code, response.Header().Get("Location"))
	}
}

// TestAFormerHostIsNotServed (c-l-served 7): at a former address's host, at its own port, at
// another and spelt otherwise, neither Google sign-in route is served (the 404 a path no route
// serves answers, with no cookie), and every browser write that carries a session cookie or an
// Identity-Aware Proxy assertion is refused 403 invalid request origin, whatever its Origin: the
// former address's own, the canonical address's, or none with Sec-Fetch-Site: same-origin. So is a
// document socket opened with the session cookie, whatever Authorization header it carries beside
// it, since the socket reads none. A read there, and a write an agent's Authorization header makes,
// with or without a cookie beside it, are answered as at the canonical Host: the API reads such a
// write's Authorization header and not the cookie.
//
// Control: remove the former fence (hostOrigins.atFormer answering false) and the sign-in starts
// there, back to the canonical callback, and the writes with the canonical Origin or none pass.
func TestAFormerHostIsNotServed(t *testing.T) {
	rig := newServedRig(t)
	for _, host := range []string{"old.example.test", "old.example.test:8443", "OLD.example.test."} {
		for _, path := range []string{"/auth/start", "/auth/callback?code=c&state=s"} {
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://"+host+path, nil))
			if response.Code != http.StatusNotFound || len(response.Result().Cookies()) != 0 {
				t.Errorf("GET %s at Host %s: %d %v %s, want the 404 of a path no route serves, with no cookie", path, host, response.Code, response.Header(), strings.TrimSpace(response.Body.String()))
			}
		}
		for _, write := range []struct {
			origin string
			header http.Header
		}{
			{tableFormer, nil},
			{tableCanonical, nil},
			{"", http.Header{"Sec-Fetch-Site": {"same-origin"}}},
		} {
			requireWrite(t, "a cookie write with Origin "+write.origin+" to Host "+host+" "+headerNames(write.header),
				signedInWrite(t, rig, host, write.origin, write.header), false)
		}
		assertion := httptest.NewRequest(http.MethodPost, "https://"+host+"/auth/logout", nil)
		assertion.Header.Set(identity.IAPAssertionHeader, "an-assertion")
		assertion.Header.Set("Origin", tableCanonical)
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, assertion)
		requireWrite(t, "a write carrying an Identity-Aware Proxy assertion to Host "+host, response, false)

		cookie := sessionCookie(rig.signIn(t, googleClaims("google-subject")))
		if cookie == nil {
			t.Fatal("the sign-in issued no session cookie")
		}
		read := func(at string) (int, string) {
			request := httptest.NewRequest(http.MethodGet, "https://"+at+"/auth/whoami", nil)
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, request)
			return response.Code, response.Body.String()
		}
		canonicalStatus, canonicalBody := read("a.example.test")
		if status, body := read(host); status != http.StatusOK || status != canonicalStatus || body != canonicalBody {
			t.Errorf("GET /auth/whoami with a session cookie at Host %s: %d %s, want the canonical Host's 200 %s", host, status, strings.TrimSpace(body), strings.TrimSpace(canonicalBody))
		}
		// A document socket writes on the cookie alone, so at a former host it is refused as every
		// cookie write is, an Authorization header beside the cookie included, since the socket reads
		// none; at the served host it passes the fence to the socket's own upgrade.
		socket := func(at, origin, authorization string) (int, string) {
			request := httptest.NewRequest(http.MethodGet, "https://"+at+"/ws/doc/an-artifact", nil)
			request.Header.Set("Origin", origin)
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, request)
			return response.Code, response.Body.String()
		}
		for _, authorization := range []string{"", "Basic eDp5", "Bearer not-a-token"} {
			if status, body := socket(host, tableFormer, authorization); status != http.StatusForbidden || !strings.Contains(body, "invalid request origin") {
				t.Errorf("a document socket with a session cookie and Authorization %q at Host %s: %d %s, want 403 invalid request origin", authorization, host, status, strings.TrimSpace(body))
			}
			if status, body := socket("b.example.test", tableServed, authorization); strings.Contains(body, "invalid request origin") {
				t.Errorf("a document socket with a session cookie and Authorization %q at the served Host b.example.test: %d %s, want it past the former fence", authorization, status, strings.TrimSpace(body))
			}
		}
		agent := func(at string, cookie *http.Cookie) (int, string) {
			request := httptest.NewRequest(http.MethodPost, "https://"+at+"/auth/logout", nil)
			request.Header.Set("Authorization", "Bearer not-a-token")
			if cookie != nil {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, request)
			return response.Code, response.Body.String()
		}
		for _, beside := range []*http.Cookie{nil, {Name: "dsession", Value: "not-a-session"}} {
			status, body := agent(host, beside)
			if wantStatus, wantBody := agent("a.example.test", beside); status != wantStatus || body != wantBody || strings.Contains(body, "invalid request origin") {
				t.Errorf("a write with an Authorization header and cookie %v at Host %s: %d %s, want the canonical Host's %d %s", beside, host, status, strings.TrimSpace(body), wantStatus, strings.TrimSpace(wantBody))
			}
		}
	}
}

// TestServedHostAtAnotherPortIsAnsweredAsTheCanonicalOne: a served address is its host at its own
// port, the scheme's default here, however that port is written. At another port of its host the
// server answers as the canonical address, as at any Host it names no origin at: a sign-in started
// there comes back to the canonical callback, and a cookie write there passes with the canonical
// Origin and is refused with the served address's, at either port.
func TestServedHostAtAnotherPortIsAnsweredAsTheCanonicalOne(t *testing.T) {
	rig := newServedRig(t)
	for _, start := range []struct{ host, callback string }{
		{"b.example.test:8443", tableCanonical + "/auth/callback"},
		{"b.example.test:443", tableServed + "/auth/callback"},
		{"b.example.test:0443", tableServed + "/auth/callback"},
	} {
		location, _ := rig.start(t, "https://"+start.host+"/auth/start")
		if got := location.Query().Get("redirect_uri"); got != start.callback {
			t.Errorf("GET /auth/start at Host %s: redirect_uri %q, want %q", start.host, got, start.callback)
		}
	}
	for _, write := range []struct {
		host, origin string
		admitted     bool
	}{
		{"b.example.test:8443", tableCanonical, true},
		{"b.example.test:8443", tableServed, false},
		{"b.example.test:8443", "https://b.example.test:8443", false},
		{"b.example.test:443", tableServed, true},
		{"b.example.test:443", tableCanonical, false},
	} {
		requireWrite(t, "a cookie write with Origin "+write.origin+" to Host "+write.host, signedInWrite(t, rig, write.host, write.origin, nil), write.admitted)
	}
}

// whoamiRouter is newTestRouterWith over a database of its own, whose GET /auth/whoami and GET
// /api/v1/whoami answer the person the X-Dispatch-User header names, with configure applied to its
// options, which set the ServerURL and the lists that name the server's origins.
func whoamiRouter(t *testing.T, configure func(*AppContextOptions)) http.Handler {
	t.Helper()
	handler, _ := newTestRouterWith(t, func(options *AppContextOptions) {
		options.Store = storetest.Open(t)
		configure(options)
	})
	return handler
}

// rawWhoamiAt answers GET /auth/whoami's raw body for login, sent to host.
func rawWhoamiAt(t *testing.T, handler http.Handler, host, login string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/auth/whoami", nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, strings.TrimSuffix(response.Body.String(), "\n")
}

// TestWhoamiNamesTheServedAndFormerAddresses (R2): with either list set, GET /auth/whoami names the
// canonical origin, each served and each former address, in that order, at every Host: the
// canonical one, a served one, a former one and one the server names no origin at. With neither
// list, its answer is byte for byte the one it was before either existed, with no origins key.
func TestWhoamiNamesTheServedAndFormerAddresses(t *testing.T) {
	const answer = `{"event_head":"0","kind":"user","login":"alice@example.com"`
	lists := whoamiRouter(t, func(options *AppContextOptions) {
		options.ServerURL, options.ServedOrigins, options.FormerOrigins = tableCanonical, []string{tableServed}, []string{tableFormer}
	})
	want := answer + `,"origins":["` + tableCanonical + `","` + tableServed + `","` + tableFormer + `"]}`
	for _, host := range []string{"a.example.test", "b.example.test", "old.example.test", "other.example.test"} {
		if status, body := rawWhoamiAt(t, lists, host, "alice@example.com"); status != http.StatusOK || body != want {
			t.Errorf("whoami at Host %s = %d %s, want %s", host, status, body, want)
		}
	}
	former := whoamiRouter(t, func(options *AppContextOptions) {
		options.ServerURL, options.FormerOrigins = tableCanonical, []string{tableFormer}
	})
	if status, body := rawWhoamiAt(t, former, "a.example.test", "alice@example.com"); status != http.StatusOK || body != answer+`,"origins":["`+tableCanonical+`","`+tableFormer+`"]}` {
		t.Errorf("whoami with a former address alone = %d %s, want the canonical origin and the former address", status, body)
	}
	one := whoamiRouter(t, func(options *AppContextOptions) { options.ServerURL = tableCanonical })
	for _, host := range []string{"a.example.test", "b.example.test"} {
		if status, body := rawWhoamiAt(t, one, host, "alice@example.com"); status != http.StatusOK || body != answer+"}" {
			t.Errorf("whoami at Host %s with neither list = %d %s, want today's three keys and no origins", host, status, body)
		}
	}
}

// TestTheAPIWhoamiMatchesAuthWhoamiThroughTheRouter (R10, D14): BuildAppContext hands the router's
// one origin table's list (hostOrigins.origins) to api.DepsInput.WhoamiOrigins, so GET
// /api/v1/whoami names the exact origins GET /auth/whoami names at every Host on both its user and
// its agent forms:
//   - with both lists set, canonical, served, former at the canonical, a served and a former Host;
//   - with neither list, no "origins" key on either form.
func TestTheAPIWhoamiMatchesAuthWhoamiThroughTheRouter(t *testing.T) {
	const agentKey = "dispatch-router-whoami-origins-fixture"
	tokens, err := auth.ParseSharedAgentTokens(agentKey)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens: %v", err)
	}
	decodeOrigins := func(what string, response *httptest.ResponseRecorder) ([]string, bool) {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", what, response.Code, response.Body.String())
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
			t.Fatalf("decode %s body %s: %v", what, response.Body.String(), err)
		}
		raw, present := fields["origins"]
		if !present {
			return nil, false
		}
		var origins []string
		if err := json.Unmarshal(raw, &origins); err != nil {
			t.Fatalf("decode %s origins %s: %v", what, string(raw), err)
		}
		return origins, true
	}
	ask := func(handler http.Handler, path, host, kind string) ([]string, bool) {
		request := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
		if kind == "user" {
			request.Header.Set("X-Dispatch-User", "alice@example.com")
		} else {
			request.Header.Set("Authorization", "Bearer "+agentKey)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return decodeOrigins(kind+" GET "+path+" at Host "+host, response)
	}

	every := []string{tableCanonical, tableServed, tableFormer}
	both := whoamiRouter(t, func(options *AppContextOptions) {
		options.ServerURL, options.ServedOrigins, options.FormerOrigins = tableCanonical, []string{tableServed}, []string{tableFormer}
		options.AgentTokens = tokens
	})
	for _, host := range []string{"a.example.test", "b.example.test", "old.example.test"} {
		authOrigins, authPresent := ask(both, "/auth/whoami", host, "user")
		if !authPresent || !slices.Equal(authOrigins, every) {
			t.Errorf("GET /auth/whoami at Host %s = %v (present=%v), want %v", host, authOrigins, authPresent, every)
		}
		for _, kind := range []string{"user", "agent"} {
			got, present := ask(both, "/api/v1/whoami", host, kind)
			if !present || !slices.Equal(got, authOrigins) {
				t.Errorf("%s GET /api/v1/whoami at Host %s = %v (present=%v), want GET /auth/whoami's %v", kind, host, got, present, authOrigins)
			}
		}
	}

	none := whoamiRouter(t, func(options *AppContextOptions) {
		options.ServerURL, options.AgentTokens = tableCanonical, tokens
	})
	if got, present := ask(none, "/auth/whoami", "a.example.test", "user"); present {
		t.Errorf("GET /auth/whoami with neither list carried origins=%v, want omitted", got)
	}
	for _, kind := range []string{"user", "agent"} {
		if got, present := ask(none, "/api/v1/whoami", "a.example.test", kind); present {
			t.Errorf("%s GET /api/v1/whoami with neither list carried origins=%v, want omitted", kind, got)
		}
	}
}

// TestBuildAppContextRefusesAListItCannotAnswer: the router refuses to be built with a served or a
// former address beside no ServerURL, which leaves no canonical origin for the table to answer
// every other Host with, and with an entry that is no origin weborigin.Parse takes.
func TestBuildAppContextRefusesAListItCannotAnswer(t *testing.T) {
	for _, row := range []struct {
		name    string
		options AppContextOptions
		refusal string
	}{
		{"a served address with no ServerURL", AppContextOptions{ServedOrigins: []string{tableServed}}, "need a ServerURL"},
		{"a former address with no ServerURL", AppContextOptions{FormerOrigins: []string{tableFormer}}, "need a ServerURL"},
		{"a served address with a path", AppContextOptions{ServerURL: tableCanonical, ServedOrigins: []string{tableServed + "/dispatch"}}, "ServedOrigins entry 1 of 1"},
		{"a former address in upper case", AppContextOptions{ServerURL: tableCanonical, FormerOrigins: []string{"https://OLD.example.test"}}, "FormerOrigins entry 1 of 1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			people := newMemoryPeople()
			options := row.options
			options.SigningKey, options.People, options.Lifetime = testSigningKey, people, t.Context()
			options.Identity = identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
			if _, err := BuildAppContext(withDocuments(t, options)); err == nil || !strings.Contains(err.Error(), row.refusal) {
				t.Errorf("BuildAppContext = %v, want a refusal naming %q", err, row.refusal)
			}
		})
	}
}
