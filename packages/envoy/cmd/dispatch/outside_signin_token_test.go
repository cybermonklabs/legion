package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The personal token of a person from outside the domain carries their sign-in to an agent, so it
// acts only while the rule the deployment runs admits them now. It is checked at every use, so it
// stops at once, where the person's cookie stops at its next hourly confirmation.

// cookieRequest answers method path from handler with body, carrying the session cookie signIn set
// as the dashboard's own script sends it, from the server's origin (requestWith, with a body).
func cookieRequest(handler http.Handler, method, path, body string, signIn *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://dispatch.test"+path, strings.NewReader(body))
	request.Header.Set("Origin", "http://dispatch.test")
	request.Header.Set("Content-Type", "application/json")
	for _, cookie := range signIn.Result().Cookies() {
		if isSessionCookie(cookie) {
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

// requireTokenRefused requires recorder to be the 401 TOKEN_OWNER_NOT_ADMITTED whose words say
// saying.
func requireTokenRefused(t *testing.T, what, saying string, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "TOKEN_OWNER_NOT_ADMITTED" || !strings.Contains(recorder.Body.String(), saying) {
		t.Fatalf("%s answered %d %s, want 401 TOKEN_OWNER_NOT_ADMITTED saying %q", what, recorder.Code, recorder.Body.String(), saying)
	}
}

// On a deployment that admits no restricted member, the personal tokens of one act for nobody:
// a token the member minted while full, and one whose stored owner is the member's address wrapped
// in a tab and a no-break space - which model.CanonicalLogin trims, as every reader of a login does,
// so its lookup finds the member's people row (canonical_login) rather than reading it as the
// token of nobody the people table holds, which the rule never asks about. Each is refused 401
// TOKEN_OWNER_NOT_ADMITTED, saying the deployment admits nobody from outside, on a read and on a
// write. The control: once the member's row is a full member's again, both act. No writer stores a
// wrapped owner today; that row is written here directly, its plaintext random.
func TestOutsideSignInARestrictedMembersTokenActsForNobodyWhereNoneIsAdmitted(t *testing.T) {
	ctx := context.Background()
	s := newOutsideSignIn(t, nil)
	signIn := s.workspaceSignIn(t, s.handler, outsiderEmail, "google-outsider")
	minted := mintTokenOn(t, s.handler, signIn)
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	wrapped := "dsp_" + base64.RawURLEncoding.EncodeToString(random)
	hash := sha256.Sum256([]byte(wrapped))
	if _, err := s.database.Pool.Exec(ctx, `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes) values ($1, 'a wrapped owner''s agent', $2, $3, $4)
	`, "\t"+outsiderEmail+"\u00a0", hash[:], wrapped[4:12], model.Scopes); err != nil {
		t.Fatal(err)
	}
	membership, _, err := s.people.Membership(ctx, outsiderEmail)
	if err != nil {
		t.Fatal(err)
	}
	kind := func(kind model.MemberKind) {
		t.Helper()
		if err := s.people.ConfirmSubject(ctx, outsiderEmail, "google-outsider", membership.RefreshToken, membership.RefreshToken, kind); err != nil {
			t.Fatalf("record %s as a %s member: %v", outsiderEmail, kind, err)
		}
	}

	kind(model.MemberRestricted)
	const saying = "admits nobody from outside it"
	for name, token := range map[string]string{"the minted token": minted, "the wrapped owner's token": wrapped} {
		requireTokenRefused(t, "GET /api/v1/projects with "+name, saying,
			bearerRequest(s.handler, http.MethodGet, "/api/v1/projects", "", token))
		requireTokenRefused(t, "POST /api/v1/issues with "+name, saying,
			bearerRequest(s.handler, http.MethodPost, "/api/v1/issues", `{"project":"PRIV","title":"Written by a restricted member's agent","actor":{"kind":"session","id":"outsider-agent"}}`, token))
	}

	kind(model.MemberFull)
	for name, token := range map[string]string{"the minted token": minted, "the wrapped owner's token": wrapped} {
		requireTokenActs(t, "GET /api/v1/whoami with "+name+", the member full again",
			bearerRequest(s.handler, http.MethodGet, "/api/v1/whoami", "", token))
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
