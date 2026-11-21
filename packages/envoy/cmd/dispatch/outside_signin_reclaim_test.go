package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// An account outside the Workspace can take an address of another domain the Workspace owns while
// no people row holds it: PRIV's list names alice@second.example before its member ever signed in,
// and an account with that verified email and no hd signs in as a restricted member and mints a
// personal token, which acts. Then the member (hd example.com, the same email, another Google
// account) signs in, and the address is the member's: the outsider's token is refused 401
// TOKEN_REVOKED at once, on a read and on a mint, rather than acting as the full member its owner's
// row now reads; the outsider's cookie is signed out; and the member's own cookie mints a token
// that acts as the full member. The router keeps sessions in the database (routerWithSessions), as
// a deployment does, so the sign-in's end of the address's sessions reaches the cookies.
func TestOutsideSignInReclaimRevokesTheOutsidersToken(t *testing.T) {
	const memberEmail = "alice@second.example"
	s := newOutsideSignIn(t, nil)
	handler := s.routerWithSessions(t)
	s.listOn(t, "PRIV", access.PrincipalPerson, memberEmail)

	outsider := codeFlowSignIn(t, handler, s.pool, map[string]any{"sub": "outsider", "email": memberEmail, "email_verified": true})
	if outsider.Code != http.StatusFound || !sessionCookieSet(outsider) {
		t.Fatalf("the account outside the Workspace at %s, which no row holds: the callback answered %d %s, want a signed-in redirect", memberEmail, outsider.Code, outsider.Body.String())
	}
	if kind := s.memberKind(t, memberEmail); kind != string(model.MemberRestricted) {
		t.Fatalf("people.member_kind of %s after the outside sign-in = %q, want restricted", memberEmail, kind)
	}
	outsiderToken := mintTokenOn(t, handler, outsider)
	requireTokenActs(t, "GET /api/v1/whoami with the outsider's token, before the member signs in",
		bearerRequest(handler, http.MethodGet, "/api/v1/whoami", "", outsiderToken))

	member := codeFlowSignIn(t, handler, s.pool, map[string]any{"sub": "member", "email": memberEmail, "email_verified": true, "hd": "example.com"})
	if member.Code != http.StatusFound || !sessionCookieSet(member) {
		t.Fatalf("the Workspace account %s: the callback answered %d %s, want a signed-in redirect", memberEmail, member.Code, member.Body.String())
	}
	if kind := s.memberKind(t, memberEmail); kind != string(model.MemberFull) {
		t.Fatalf("people.member_kind of %s after the member's sign-in = %q, want full", memberEmail, kind)
	}

	for _, request := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/projects", ""},
		{http.MethodPost, "/api/v1/me/agent-tokens", `{"name":"another agent"}`},
	} {
		recorder := bearerRequest(handler, request.method, request.path, request.body, outsiderToken)
		if recorder.Code != http.StatusUnauthorized || errorCode(recorder) != "TOKEN_REVOKED" {
			t.Errorf("%s %s with the outsider's token, after the member's sign-in, answered %d %s, want 401 TOKEN_REVOKED", request.method, request.path, recorder.Code, recorder.Body.String())
		}
	}
	if whoami := requestWith(handler, http.MethodGet, "/auth/whoami", outsider); whoami.Code != http.StatusUnauthorized || errorCode(whoami) != "NO_IDENTITY" {
		t.Errorf("the outsider's cookie, after the member's sign-in, answered GET /auth/whoami %d %s, want 401 NO_IDENTITY", whoami.Code, whoami.Body.String())
	}

	whoami := bearerRequest(handler, http.MethodGet, "/api/v1/whoami", "", mintTokenOn(t, handler, member))
	const want = `{"kind":"agent","owner":"alice@second.example","project":null,"scopes":["person","approve"],"service":null,"session":null}`
	if got := strings.TrimSpace(whoami.Body.String()); whoami.Code != http.StatusOK || got != want {
		t.Fatalf("GET /api/v1/whoami with the member's new token answered %d %s, want 200 %s", whoami.Code, got, want)
	}
}

// routerWithSessions is the router main serves for s's boot with the database's own session store
// (store.PgSessionStore) in place of currentSessions, so a cookie is current only while its
// generation is still its login's, as in a deployment, and a sign-in that ends an address's
// sessions ends its cookies here too.
func (s *outsideSignIn) routerWithSessions(t *testing.T) http.Handler {
	t.Helper()
	built := routes.AppContextOptions{
		Lifetime: t.Context(), SigningKey: testSigningKey, SignIn: signInFlow(s.boot), People: s.people,
		Store: s.database, Sessions: store.NewPgSessionStore(s.database.Pool),
	}
	rule, err := signInRule(s.boot, accessLister(built.Store))
	if err != nil {
		t.Fatal(err)
	}
	built.Identity = requestIdentityFor(s.boot, built.SigningKey, built.People, built.Sessions, built.SignIn, rule, nil)
	appCtx, err := routes.BuildAppContext(appContextOptions(s.boot, rule, built))
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}
	handler, background := routes.New(appCtx)
	t.Cleanup(background.Wait)
	return handler
}

// waitForBlockedBackends waits until at least blocked of the database's backends wait on a lock
// another holds, and reports whether they did: false once done closes first. It fails t after ten
// seconds of neither.
func waitForBlockedBackends(t *testing.T, s *outsideSignIn, blocked int, what string, done <-chan struct{}) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.database.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity where datname = current_database() and cardinality(pg_blocking_pids(pid)) > 0`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= blocked {
			return true
		}
		select {
		case <-done:
			return false
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s neither answered nor waited in ten seconds", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reclaimCaller is the outsider as each credential a mint can be made with: its cookie, its
// personal token, or a broker token its machine was issued (machine holds that machine's key), and
// the router the case serves it through.
type reclaimCaller struct {
	handler http.Handler
	cookie  *httptest.ResponseRecorder
	token   string
}

// A mint that races the member's reclaim of the address cannot leave the outsider a token: the
// outsider holds alice@second.example and a credential of it, and the member's sign-in
// (PgPeopleStore.SignInSubject under another sub) is held open with the people row locked, its
// sessions ended and its tokens revoked, until the test lets it commit. The outsider's mint, by
// the cookie or by the personal token, waits for that row, finds once the reclaim commits that its
// caller no longer holds the address, and is refused as any later request of that caller is (401
// NO_IDENTITY for the cookie, 401 TOKEN_REVOKED for the personal token), with no token row
// written. A broker token mints nothing, reclaim or none: none holds the approve scope
// (TestOutsideSignInABrokerTokenApprovesDeniesAndMintsNothing).
func TestOutsideSignInReclaimRefusesAMintThatWaitedOnIt(t *testing.T) {
	for _, test := range []struct {
		name, code string
		mint       func(c reclaimCaller, body string) *httptest.ResponseRecorder
		later      func(c reclaimCaller) *httptest.ResponseRecorder
	}{
		{"by the cookie", "NO_IDENTITY",
			func(c reclaimCaller, body string) *httptest.ResponseRecorder {
				return cookieRequest(c.handler, http.MethodPost, "/api/v1/me/agent-tokens", body, c.cookie)
			},
			func(c reclaimCaller) *httptest.ResponseRecorder {
				return requestWith(c.handler, http.MethodGet, "/auth/whoami", c.cookie)
			}},
		{"by the token", "TOKEN_REVOKED",
			func(c reclaimCaller, body string) *httptest.ResponseRecorder {
				return bearerRequest(c.handler, http.MethodPost, "/api/v1/me/agent-tokens", body, c.token)
			},
			func(c reclaimCaller) *httptest.ResponseRecorder {
				return bearerRequest(c.handler, http.MethodGet, "/api/v1/whoami", "", c.token)
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			s := newOutsideSignIn(t, nil)
			var caller reclaimCaller
			email, reclaimer := "alice@second.example", "member"
			caller.handler = s.routerWithSessions(t)
			s.listOn(t, "PRIV", access.PrincipalPerson, email)
			caller.cookie = codeFlowSignIn(t, caller.handler, s.pool, map[string]any{"sub": "outsider", "email": email, "email_verified": true})
			if caller.cookie.Code != http.StatusFound || !sessionCookieSet(caller.cookie) {
				t.Fatalf("the outside account at %s: the callback answered %d %s, want a signed-in redirect", email, caller.cookie.Code, caller.cookie.Body.String())
			}
			caller.token = mintTokenOn(t, caller.handler, caller.cookie)
			// Used once now, a personal token records its use, so the mint's own use of it a moment
			// later writes nothing (personalTokenActor records a use a minute apart) and waits on
			// no row the reclaim holds: the mint waits only where the mint itself takes a lock.
			requireTokenActs(t, "GET /api/v1/whoami with the outsider's credential "+test.name+", before the reclaim", test.later(caller))

			if _, err := s.database.Pool.Exec(ctx, `
				create function hold_reclaim() returns trigger language plpgsql as $$ begin perform pg_advisory_xact_lock(4199); return new; end $$;
				create trigger hold_reclaim before update on people for each row execute function hold_reclaim();
			`); err != nil {
				t.Fatal(err)
			}
			holder, err := s.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(4199)`); err != nil {
				t.Fatal(err)
			}
			reclaimed, reclaimDone := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(reclaimDone)
				reclaimed <- s.people.SignInSubject(ctx, email, reclaimer, "refresh-member", model.MemberFull)
			}()
			if !waitForBlockedBackends(t, s, 1, "the member's reclaim", reclaimDone) {
				t.Fatalf("the member's reclaim finished while the test held it: %v", <-reclaimed)
			}

			const name = "minted while the reclaim held the address"
			minted, mintDone := make(chan *httptest.ResponseRecorder, 1), make(chan struct{})
			go func() {
				defer close(mintDone)
				minted <- test.mint(caller, `{"name":"`+name+`"}`)
			}()
			if !waitForBlockedBackends(t, s, 2, "the outsider's mint", mintDone) {
				t.Logf("the outsider's mint %s answered while the reclaim held the address, without waiting for it", test.name)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-reclaimed; err != nil {
				t.Fatalf("the member's reclaim: %v", err)
			}
			refused := <-minted
			if refused.Code != http.StatusUnauthorized || errorCode(refused) != test.code {
				t.Errorf("the outsider's mint %s, which waited on the reclaim, answered %d %s, want 401 %s", test.name, refused.Code, refused.Body.String(), test.code)
			}
			if later := test.later(caller); later.Code != refused.Code || errorCode(later) != errorCode(refused) {
				t.Errorf("the outsider's next request %s answered %d %s, and the mint %d %s: want the mint refused as that request is", test.name, later.Code, later.Body.String(), refused.Code, refused.Body.String())
			}
			var written int
			if err := s.database.Pool.QueryRow(ctx, `select count(*) from agent_tokens where name = $1`, name).Scan(&written); err != nil {
				t.Fatal(err)
			}
			if written != 0 {
				t.Errorf("the refused mint %s wrote %d token rows, want none", test.name, written)
			}
		})
	}
}
