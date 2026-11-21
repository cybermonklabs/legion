package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A dev sign-in server (DISPATCH_DEV_SIGNIN=1) refuses every DISPATCH_SIGNIN_* setting, so it has
// no sign-in rule, and a restricted member exists on it only as a local harness writes one (the
// project-access driver's mark_restricted). It holds such a member's personal token to the outside
// rule's list arm (devRestrictedAdmission, identity.ListedAdmission): the token of a restricted
// member PRIV's access list names acts, and the token of one no project names is refused
// 401 TOKEN_OWNER_NOT_ADMITTED saying so, as outside sign-in answers them.
//
// Control in signin.go: devRestrictedAdmission answering nil, as a dev sign-in server held such a
// token before (the listed member's token is refused, saying outside sign-in is off).
func TestADevSignInServerHoldsARestrictedMembersTokenToTheLists(t *testing.T) {
	const listed, unlisted = "listed@other.example", "unlisted@other.example"
	ctx := context.Background()
	database := storetest.Open(t)
	boot, err := resolveBootConfig(devSignInEnvironment(nil))
	if err != nil || !boot.DevSignIn {
		t.Fatalf("the dev sign-in boot: DevSignIn %t, %v", boot.DevSignIn, err)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{
		ServerURL: devSignInOrigin, Store: database,
		People:   store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
		Sessions: store.NewPgSessionStore(database.Pool),
	})
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('PRIV', 'Private', $1)`, privOwner); err != nil {
		t.Fatalf("seed PRIV: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := access.AddPrincipal(ctx, tx, events.NewBroker(), "PRIV", access.PrincipalPerson, listed, model.Actor{Kind: "user", ID: privOwner}); err != nil {
		t.Fatalf("name %s on PRIV: %v", listed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{}
	for _, person := range []string{listed, unlisted} {
		tokens[person] = devSignInToken(t, handler, person)
		if _, err := database.Pool.Exec(ctx, `update people set member_kind = 'restricted' where email = $1`, person); err != nil {
			t.Fatalf("mark %s restricted: %v", person, err)
		}
	}

	whoami := func(token string) *httptest.ResponseRecorder {
		return devSignInRequest(handler, http.MethodGet, "/api/v1/whoami", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	}
	if answer := whoami(tokens[listed]); answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `"member_kind":"restricted"`) {
		t.Errorf("GET /api/v1/whoami with the token of %s, a restricted member PRIV names, answered %d %s, want 200 naming a restricted member", listed, answer.Code, answer.Body.String())
	}
	const noList = "no project names them any more"
	if answer := whoami(tokens[unlisted]); answer.Code != http.StatusUnauthorized || errorCode(answer) != "TOKEN_OWNER_NOT_ADMITTED" || !strings.Contains(answer.Body.String(), noList) {
		t.Errorf("GET /api/v1/whoami with the token of %s, a restricted member no project names, answered %d %s, want 401 TOKEN_OWNER_NOT_ADMITTED saying %q", unlisted, answer.Code, answer.Body.String(), noList)
	}
}

// devSignInRequest answers method path from handler as a browser or an agent on this machine sends
// it to the dev sign-in server's origin, with body, and each of with applied to the request.
func devSignInRequest(handler http.Handler, method, path, body string, with ...func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, devSignInOrigin+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40000"
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, apply := range with {
		apply(request)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// devSignInToken signs login in at handler's dev sign-in route and mints a personal token on the
// cookie it sets, as Settings mints one, returning the token's plaintext.
func devSignInToken(t *testing.T, handler http.Handler, login string) string {
	t.Helper()
	signIn := devSignInRequest(handler, http.MethodGet, "/auth/_dev/signin?login="+url.QueryEscape(login), "")
	if signIn.Code != http.StatusFound || !sessionCookieSet(signIn) {
		t.Fatalf("dev sign-in as %s answered %d %s, want a redirect setting the session cookie", login, signIn.Code, signIn.Body.String())
	}
	minted := devSignInRequest(handler, http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"an agent of mine"}`, func(r *http.Request) {
		r.Header.Set("Origin", devSignInOrigin)
		for _, cookie := range signIn.Result().Cookies() {
			if cookie.Name == hostSessionCookie && cookie.Value != "" {
				r.AddCookie(cookie)
			}
		}
	})
	var token struct {
		Token string `json:"token"`
	}
	if minted.Code != http.StatusCreated || json.Unmarshal(minted.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatalf("%s's mint on the dev sign-in cookie answered %d %s, want 201 with a token", login, minted.Code, minted.Body.String())
	}
	return token.Token
}
