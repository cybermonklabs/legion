package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The boot's retirement of the refresh tokens stored in plain text is bounded: a sign-in pool that
// accepts the connection and never answers delays the start by the bound, not by each token's own
// timeout, and the start goes on, every token left in its row for the next boot and the counts
// logged without a token. The bound runs on a fake clock, which the test moves to it once the
// pool holds the first revocation, so the retirement can end on nothing but its bound.
func TestRetirePlainRefreshTokensAtBootStopsAtItsBound(t *testing.T) {
	ctx := context.Background()
	pool := signInPool(t, "dispatch-client", "client-secret")
	pool.DelayRevocation(time.Hour)
	boot := resolveWith(t, map[string]string{"DISPATCH_SIGNIN_ISSUER": pool.URL()})
	signIn := signInFlow(boot)
	database := storetest.Open(t)
	tokens := []string{"plain-token-1", "plain-token-2", "plain-token-3"}
	for i, token := range tokens {
		if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) values ($1, $2, now())`, []string{"a@d.example", "b@d.example", "c@d.example"}[i], token); err != nil {
			t.Fatal(err)
		}
	}
	people, _, err := openPeople(boot, t.TempDir(), database.Pool, signIn)
	if err != nil {
		t.Fatalf("openPeople: %v", err)
	}
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	fake := clocktest.New(time.Now())
	bound := fake.Now().Add(plainRefreshTokenRetireTimeout)
	retired := make(chan struct{})
	go func() {
		defer close(retired)
		retirePlainRefreshTokens(ctx, people, fake, plainRefreshTokenRetireTimeout)
	}()
	if armed := fake.AwaitArmed(t, "the retirement's bound", 1); !armed[0].Equal(bound) {
		t.Fatalf("the retirement armed %v, want its one bound at %v, %v from its start", armed, bound, plainRefreshTokenRetireTimeout)
	}
	// The store main builds revokes through the pool's code flow, which discovers the pool on the
	// first revocation: without it every token would fail at once with no request made, and the
	// bound would end a retirement that waited on nothing.
	testwait.Eventually(t, "the first revocation to reach the pool that never answers", func() bool {
		return len(pool.RevocationRequests()) > 0
	})
	select {
	case <-retired:
		t.Fatal("the retirement returned while the pool held its first revocation, before its bound")
	default:
	}
	fake.Advance(plainRefreshTokenRetireTimeout)
	testwait.Receive(t, "the retirement to return at its bound", retired)
	// The bound ends the retirement on the context the revocations run on, so the tokens after the
	// held one fail on that context without reaching the pool. A retirement that ignored it would
	// send each later token to the pool and wait out each one's own timeout.
	if sent := len(pool.RevocationRequests()); sent != 1 {
		t.Errorf("the pool received %d revocations, want 1: the bound must end the tokens after the held one before they are sent", sent)
	}
	out := logs.String()
	if !strings.Contains(out, "retired the refresh tokens stored in plain text") || !strings.Contains(out, "retired=0 failed=3") {
		t.Errorf("logged, want the counts retired=0 failed=3:\n%s", out)
	}
	for _, token := range tokens {
		if strings.Contains(out, token) {
			t.Errorf("the log carries %s:\n%s", token, out)
		}
	}
	var kept int
	if err := database.Pool.QueryRow(ctx, `select count(*) from people where refresh_token like 'plain-token-%'`).Scan(&kept); err != nil || kept != len(tokens) {
		t.Errorf("%d rows still hold their token (%v), want all %d left for the next boot", kept, err, len(tokens))
	}
}

// The boot's retirement against a sign-in pool that cannot be discovered fails every token
// without a revocation sent, leaves each in its row for the next boot, and the start goes on.
func TestRetirePlainRefreshTokensAgainstAnUndiscoveredPoolKeepsEveryToken(t *testing.T) {
	ctx := context.Background()
	pool := signInPool(t, "dispatch-client", "client-secret")
	pool.FailDiscovery(http.StatusServiceUnavailable)
	boot := resolveWith(t, map[string]string{"DISPATCH_SIGNIN_ISSUER": pool.URL()})
	database := storetest.Open(t)
	for _, email := range []string{"a@d.example", "b@d.example"} {
		if _, err := database.Pool.Exec(ctx, `insert into people (email, refresh_token, confirmed_at) values ($1, $2, now())`, email, "plain-token-"+email); err != nil {
			t.Fatal(err)
		}
	}
	people, _, err := openPeople(boot, t.TempDir(), database.Pool, signInFlow(boot))
	if err != nil {
		t.Fatalf("openPeople: %v", err)
	}
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	retirePlainRefreshTokens(ctx, people, clock.Real(), plainRefreshTokenRetireTimeout)
	if out := logs.String(); !strings.Contains(out, "retired=0 failed=2") {
		t.Errorf("logged, want the counts retired=0 failed=2:\n%s", out)
	}
	if got := pool.RevocationRequests(); len(got) != 0 {
		t.Errorf("the pool's revocation endpoint was sent %d tokens before its discovery answered, want none", len(got))
	}
	var kept int
	if err := database.Pool.QueryRow(ctx, `select count(*) from people where refresh_token like 'plain-token-%'`).Scan(&kept); err != nil || kept != 2 {
		t.Errorf("%d rows still hold their token (%v), want both left for the next boot", kept, err)
	}
}

// A Google outage does not sign anyone out. A signed-in person whose hourly confirmation has
// lapsed while the sign-in issuer's token endpoint answers 503 is refused 503 SIGNIN_UNAVAILABLE by
// a page route (/auth/whoami) and by the API (/api/v1/whoami), never 401 or 500; once the issuer
// answers, the same cookie is served 200 by both after one refresh.
func TestASignInIssuerOutageAnswersThePageAndTheAPI503AndKeepsTheCookie(t *testing.T) {
	pool := signInPool(t, "dispatch-client", "client-secret")
	boot := resolveWith(t, map[string]string{"DISPATCH_SIGNIN_ISSUER": pool.URL(), "DISPATCH_SIGNIN_GROUP": "dispatch-members"})
	people := peopleStore(t)
	handler := signInRouter(t, boot, people)
	signedIn := poolSignIn(t, handler, pool, "dispatch-members")
	if signedIn.Code != http.StatusFound || !sessionCookieSet(signedIn) {
		t.Fatalf("the sign-in: %d %s, want a signed-in redirect", signedIn.Code, signedIn.Body.String())
	}
	ask := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+path, nil)
		for _, cookie := range signedIn.Result().Cookies() {
			if cookie.Name == "dsession" {
				request.AddCookie(cookie)
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	ageConfirmation(t, people, "alice@example.com")
	pool.FailToken(http.StatusServiceUnavailable, "")
	for _, path := range []string{"/auth/whoami", "/api/v1/whoami"} {
		if response := ask(path); response.Code != http.StatusServiceUnavailable || errorCode(response) != "SIGNIN_UNAVAILABLE" {
			t.Errorf("GET %s past the hour while the token endpoint answers 503: %d %s, want 503 SIGNIN_UNAVAILABLE", path, response.Code, response.Body.String())
		}
	}
	pool.FailToken(0, "")
	for _, path := range []string{"/auth/whoami", "/api/v1/whoami"} {
		if response := ask(path); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "alice@example.com") {
			t.Errorf("GET %s with the same cookie once the issuer answers: %d %s, want 200 naming alice@example.com", path, response.Code, response.Body.String())
		}
	}
}
