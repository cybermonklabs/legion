package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cookieRequest answers method path from handler with body, carrying the session cookie signIn set
// as the dashboard's own script sends it, from the server's origin (requestWith, with a body).
func cookieRequest(handler http.Handler, method, path, body string, signIn *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set("Origin", "http://dispatch.test")
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range signIn.Result().Cookies() {
		if cookie.Name == "dsession" && cookie.Value != "" {
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

// requireTokenActs requires recorder to be a 200, what an admitted token is answered.
func requireTokenActs(t *testing.T, what string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s, want 200", what, recorder.Code, recorder.Body.String())
	}
}

// A domain member's token answers GET /api/v1/whoami as it always did, byte for byte (R9).
func TestOutsideSignInTokenOfAFullMemberIsUnchanged(t *testing.T) {
	s := newOutsideSignIn(t, nil)
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
