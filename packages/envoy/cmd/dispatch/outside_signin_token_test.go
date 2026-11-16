package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// The personal token of a person from outside the domain carries their sign-in to an agent, so it
// acts only while the rule that signed them in would sign them in now: while
// DISPATCH_SIGNIN_OUTSIDE=1 and some project names them directly. It is checked at every use,
// so it stops at once, where the person's cookie stops at its next hourly confirmation.

// cookieRequest answers method path from handler with body, carrying the session cookie signIn set
// as the dashboard's own script sends it, from the server's origin (requestWith, with a body).
func cookieRequest(handler http.Handler, method, path, body string, signIn *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set("Origin", "http://dispatch.test")
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range signIn.Result().Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" && cookie.MaxAge >= 0 {
			request.AddCookie(cookie)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// mintTokenOn mints a personal token through handler, POST /api/v1/me/agent-tokens on the cookie
// signIn set, as Settings mints one, and returns its plaintext.
func mintTokenOn(t *testing.T, handler http.Handler, signIn *httptest.ResponseRecorder) string {
	t.Helper()
	minted := cookieRequest(handler, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"an agent of mine"}`, signIn)
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || !strings.HasPrefix(token.Token, "dsp_") {
		t.Fatalf("POST /api/v1/me/agent-tokens on the cookie answered %d %s, want 201 with a token", minted.Code, minted.Body.String())
	}
	return token.Token
}

// bearerRequest answers method path from handler with body, carrying token as the bearer, as an
// agent sends it.
func bearerRequest(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// outsideBootWith is the boot newOutsideSignIn reads, from the same settings with overrides
// applied, pointed at s's fake issuer as newOutsideSignIn points its own.
func (s *outsideSignIn) outsideBootWith(t *testing.T, overrides map[string]string) bootConfig {
	t.Helper()
	values := map[string]string{
		"DISPATCH_SIGNIN_ISSUER":  "https://accounts.google.com",
		"DISPATCH_SIGNIN_GROUP":   "",
		"DISPATCH_SIGNIN_DOMAIN":  "example.com",
		"DISPATCH_SIGNIN_OUTSIDE": "1",
	}
	maps.Copy(values, overrides)
	boot := resolveWith(t, values)
	boot.SignInIssuer = s.pool.URL()
	return boot
}

// requireTokenActs requires recorder to be a 200, what an admitted token is answered.
func requireTokenActs(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, recorder.Code, recorder.Body.String())
	}
}

// requireTokenRefused requires recorder to be the 401 TOKEN_OWNER_NOT_ADMITTED whose words say
// saying.
func requireTokenRefused(t *testing.T, what, saying string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "TOKEN_OWNER_NOT_ADMITTED" || !strings.Contains(recorder.Body.String(), saying) {
		t.Fatalf("%s answered %d %s, want 401 TOKEN_OWNER_NOT_ADMITTED saying %q", what, recorder.Code, recorder.Body.String(), saying)
	}
}

// A listed account from outside the domain signs in and mints a token on its cookie, which reads
// PRIV. The same deployment, its settings read again with DISPATCH_SIGNIN_OUTSIDE unset, refuses
// the token at once, on a read and on a write, and still refuses it once the hourly check has
// ended the cookie session too.
func TestOutsideSignInTokenStopsWhenTheSettingIsOff(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	signIn := s.signInAs(t, s.handler, outsiderEmail)
	if signIn.Code != http.StatusFound || !sessionCookieSet(signIn) {
		t.Fatalf("the callback for %s answered %d, want a signed-in redirect", outsiderEmail, signIn.Code)
	}
	token := mintTokenOn(t, s.handler, signIn)
	read := bearerRequest(s.handler, http.MethodGet, "/api/v1/projects", "", token)
	requireTokenActs(t, "GET /api/v1/projects with the outsider's token", read)
	if !strings.Contains(read.Body.String(), `"key":"PRIV"`) {
		t.Fatalf("GET /api/v1/projects with the outsider's token answered %s, want PRIV", read.Body.String())
	}

	off := s.outsideBootWith(t, map[string]string{"DISPATCH_SIGNIN_OUTSIDE": ""})
	if off.SignInOutside {
		t.Fatal("DISPATCH_SIGNIN_OUTSIDE unset: SignInOutside true")
	}
	turnedOff := s.router(t, off)
	const saying = "DISPATCH_SIGNIN_OUTSIDE is unset"
	requireTokenRefused(t, "GET /api/v1/projects with the token, outside sign-in off", saying,
		bearerRequest(turnedOff, http.MethodGet, "/api/v1/projects", "", token))
	requireTokenRefused(t, "POST /api/v1/issues with the token, outside sign-in off", saying,
		bearerRequest(turnedOff, http.MethodPost, "/api/v1/issues", `{"project":"PRIV","title":"Written after outside sign-in was turned off","actor":{"kind":"session","id":"outsider-agent"}}`, token))

	ageConfirmation(t, s.people, outsiderEmail)
	if got := signedOutOf(t, "the whoami that ran the hourly check", requestWith(turnedOff, http.MethodGet, "/auth/whoami", signIn)); got.Reason != "OUTSIDE_SIGNIN_OFF" {
		t.Fatalf("the hourly check, outside sign-in off: %+v, want the sign-in ended OUTSIDE_SIGNIN_OFF", got)
	}
	requireTokenRefused(t, "GET /api/v1/whoami with the token, after the cookie session ended", saying,
		bearerRequest(turnedOff, http.MethodGet, "/api/v1/whoami", "", token))
}

// With outside sign-in on, a token whose owner no project names any more is refused at its next
// use, with no hour to wait, and acts again once a project names the owner again: the token itself
// was never revoked.
func TestOutsideSignInTokenStopsOnceNoListNamesTheOwner(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	signIn := s.signInAs(t, s.handler, outsiderEmail)
	if signIn.Code != http.StatusFound || !sessionCookieSet(signIn) {
		t.Fatalf("the callback for %s answered %d, want a signed-in redirect", outsiderEmail, signIn.Code)
	}
	token := mintTokenOn(t, s.handler, signIn)
	requireTokenActs(t, "GET /api/v1/whoami with the outsider's token", bearerRequest(s.handler, http.MethodGet, "/api/v1/whoami", "", token))

	s.unlist(t, "PRIV", outsiderEmail)
	requireTokenRefused(t, "GET /api/v1/whoami with the token once no list names its owner", "no project names them any more",
		bearerRequest(s.handler, http.MethodGet, "/api/v1/whoami", "", token))

	s.listOn(t, "PRIV", access.PrincipalPerson, outsiderEmail)
	requireTokenActs(t, "GET /api/v1/whoami with the token once PRIV's list names its owner again",
		bearerRequest(s.handler, http.MethodGet, "/api/v1/whoami", "", token))
}

// With DISPATCH_SIGNIN_OUTSIDE never set, a domain member's token answers GET /api/v1/whoami as it
// always did, byte for byte (R9).
func TestOutsideSignInTokenOfAFullMemberIsUnchangedWithTheSettingOff(t *testing.T) {
	s := newOutsideSignIn(t, map[string]string{"DISPATCH_SIGNIN_OUTSIDE": ""})
	signIn := googleSignIn(t, s.handler, s.pool, "example.com")
	if signIn.Code != http.StatusFound || !sessionCookieSet(signIn) {
		t.Fatalf("an account of example.com: the callback answered %d %s, want a signed-in redirect", signIn.Code, signIn.Body.String())
	}
	whoami := bearerRequest(s.handler, http.MethodGet, "/api/v1/whoami", "", mintTokenOn(t, s.handler, signIn))
	const want = `{"kind":"agent","owner":"alice@example.com","project":null,"scopes":["person","approve"],"service":null,"session":null}`
	if got := strings.TrimSpace(whoami.Body.String()); whoami.Code != http.StatusOK || got != want {
		t.Fatalf("GET /api/v1/whoami with alice's token answered %d %s, want 200 %s", whoami.Code, got, want)
	}
}
