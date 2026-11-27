package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The rows of this file refuse a request the units, or the place, it gave back for its caller's
// wait on an issuer (admission.Away.TakeBack): the row fills the class, or the share outside
// admission, while the issuer holds the request, and lets the issuer answer. Each entry point then
// answers 429 OVERLOADED with Retry-After: 1, runs nothing of its handler, and sends no statement
// after the refusal, whatever the layers between the resolution and the handler do with the error.

// requireRefusedTakeBack holds the answer of a request refused its take-back: 429 OVERLOADED with
// Retry-After: 1, its handler never run, and no connection of the request pool taken after the
// refusal (acquired is the pool's acquire count when the row let the issuer answer).
func requireRefusedTakeBack(t *testing.T, answer *httptest.ResponseRecorder, ran bool, pool *store.Pool, acquired int64) {
	t.Helper()
	if answer.Code != http.StatusTooManyRequests || answer.Header().Get("Retry-After") != admission.RetryAfter ||
		!strings.Contains(answer.Body.String(), `"code":"`+admission.CodeOverloaded+`"`) {
		t.Errorf("a request refused the units it gave back for the issuer's wait: status=%d Retry-After=%q body=%s, want 429 %s with Retry-After: %s",
			answer.Code, answer.Header().Get("Retry-After"), strings.TrimSpace(answer.Body.String()), admission.CodeOverloaded, admission.RetryAfter)
	}
	if ran {
		t.Error("the route's handler ran for a request refused its take-back, want it never run")
	}
	if took := pool.Stat().AcquireCount() - acquired; took != 0 {
		t.Errorf("the refused request took %d request-pool connections after the issuer answered, want none: no statement after the refusal", took)
	}
}

// serveInBackground serves request on handler, answering on the channel it returns.
func serveInBackground(handler http.Handler, request *http.Request) <-chan *httptest.ResponseRecorder {
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		answered <- recorder
	}()
	return answered
}

// takeEvery takes every unit of class the limiter has left, failing the row where one is not free.
func takeEvery(t *testing.T, limiter *admission.Limiter, class admission.Class) []*admission.Ticket {
	t.Helper()
	var held []*admission.Ticket
	for limiter.Stats()[class].InFlight < limiter.Stats()[class].Limit {
		ticket := limiter.Arrive(class, 1).AdmitNow()
		if ticket == nil {
			t.Fatalf("a %s unit was not free with %d of %d held", class, limiter.Stats()[class].InFlight, limiter.Stats()[class].Limit)
		}
		held = append(held, ticket)
	}
	return held
}

// lapsedCookie is the session cookie of email, a person who signed in through the issuer and whose
// confirmation lapsed two hours ago, so the next resolution of the cookie renews it with the issuer.
func lapsedCookie(t *testing.T, database *store.Store, people *store.PgPeopleStore, sessions *store.PgSessionStore, email string) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	if err := people.SignIn(ctx, email, "refresh-token-"+email); err != nil {
		t.Fatalf("record %s's sign-in: %v", email, err)
	}
	generation, err := sessions.EnsureSession(ctx, email)
	if err != nil {
		t.Fatalf("establish %s's session: %v", email, err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie(email, generation, testSigningKey, false))
	if err != nil {
		t.Fatalf("parse %s's session cookie: %v", email, err)
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = $1`, email); err != nil {
		t.Fatalf("date %s's confirmation two hours back: %v", email, err)
	}
	return cookie
}

// A service-account token behind a layer that reads a failed resolution as no caller and goes on:
// a write that names its presser (Dispatch-Login) is resolved by refuseChangedLogin, which hands a
// request whose caller does not resolve to the next layer and so on to the handler. The token's
// issuer is undiscovered and its discovery held, so the resolution gives its write unit back for
// the wait; the row takes every write unit meanwhile and only then lets the discovery answer
// (heldDiscoveryIssuer), and the take-back waits its class's queue wait and is refused. The
// request is answered 429 OVERLOADED with Retry-After: 1 and the route's handler, which sends no
// statement and would answer 204, never runs.
func TestARefusedTakeBackOfAServiceTokenBehindALayerThatSwallowsItIsAnsweredOverloaded(t *testing.T) {
	t.Parallel()
	address, token, discoveries, release := heldDiscoveryIssuer(t)
	limiter := networkWaitLimiter()
	database := storetest.Open(t)
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store: database, admission: limiter, oidc: oidc.NewLazyVerifier(address, serviceTokenAudience),
	})
	var ran atomic.Bool
	route := apiRoute{routeDecl: routeDecl{Method: http.MethodPost, Pattern: "/api/v1/test-refused-take-back", Auth: authAny}, Handler: func(w http.ResponseWriter, _ *http.Request) {
		ran.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}}
	request := httptest.NewRequest(http.MethodPost, route.Pattern, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(contracts.DispatchLoginHeader, "alice")
	answered := serveInBackground(served.routeHandler(route, limiter, time.Minute), request)
	waitUntil(t, "the token's resolution waiting on its issuer's discovery, holding no write unit", func() bool {
		return discoveries.Load() == 1 && limiter.Stats()[admission.Write].InFlight == 0
	})
	held := takeEvery(t, limiter, admission.Write)
	defer func() {
		for _, ticket := range held {
			ticket.Release()
		}
	}()
	acquired := database.Pool.Stat().AcquireCount()
	release()
	answer := testwait.Receive(t, "the request once the discovery answered", answered)
	requireRefusedTakeBack(t, answer, ran.Load(), database.Pool, acquired)
}

// A cookie whose confirmation has lapsed, resolved by a handler's own caller check that reads a
// failed resolution as no person and goes on (callerHoldsScope): the renewal waits on the sign-in
// issuer, so the resolution gives its read unit back; the row takes every read unit meanwhile; the
// issuer answers and the take-back waits its class's queue wait and is refused. The request is
// answered 429 OVERLOADED with Retry-After: 1, and nothing of the handler after its caller check -
// which sends no statement and would answer 204 - runs.
func TestARefusedTakeBackOfACookiesRefreshBehindACallerCheckThatSwallowsItIsAnsweredOverloaded(t *testing.T) {
	t.Parallel()
	limiter := networkWaitLimiter()
	issuer := &stalledIssuer{release: make(chan struct{})}
	answerIssuer := sync.OnceFunc(func() { close(issuer.release) })
	database := storetest.Open(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{
		SigningKey: testSigningKey,
		Sessions:   sessions,
		Membership: &identity.Membership{People: people, Sessions: sessions, SignIn: issuer, Rule: identity.GroupRule{Group: "dispatch-members"}},
	}
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{store: database, admission: limiter, identity: person})
	t.Cleanup(answerIssuer)
	var ran atomic.Bool
	route := apiRoute{routeDecl: routeDecl{Method: http.MethodGet, Pattern: "/api/v1/test-refused-take-back", Auth: authAny}, Handler: func(w http.ResponseWriter, r *http.Request) {
		human := served.callerHoldsScope(r, model.ScopePerson)
		ran.Store(true)
		w.Header().Set("X-Test-Person", fmt.Sprint(human))
		w.WriteHeader(http.StatusNoContent)
	}}
	request := httptest.NewRequest(http.MethodGet, route.Pattern, nil)
	request.AddCookie(lapsedCookie(t, database, people, sessions, "lapsed@d.example"))
	answered := serveInBackground(served.routeHandler(route, limiter, time.Minute), request)
	waitUntil(t, "the cookie's renewal waiting on the sign-in issuer, holding no read unit", func() bool {
		return issuer.entered.Load() == 1 && limiter.Stats()[admission.Read].InFlight == 0
	})
	held := takeEvery(t, limiter, admission.Read)
	defer func() {
		for _, ticket := range held {
			ticket.Release()
		}
	}()
	acquired := database.Pool.Stat().AcquireCount()
	answerIssuer()
	answer := testwait.Receive(t, "the request once the issuer answered", answered)
	requireRefusedTakeBack(t, answer, ran.Load(), database.Pool, acquired)
}

// A document socket whose caller's confirmation has lapsed holds a place of the request pool's
// share outside admission, which it gives back while the sign-in issuer renews the confirmation;
// the row takes every place meanwhile; the issuer answers and the place's take-back waits the
// share's second and is refused. The socket is answered 429 OVERLOADED with Retry-After: 1 before
// its upgrade, where it was answered 503 as an unserved read, and reads nothing more: not its
// document's issue, not the live server processes.
func TestARefusedTakeBackOfADocumentSocketsPlaceIsAnsweredOverloadedBeforeTheUpgrade(t *testing.T) {
	t.Parallel()
	limiter := admission.New(admission.DefaultLimits())
	issuer := &stalledIssuer{release: make(chan struct{})}
	answerIssuer := sync.OnceFunc(func() { close(issuer.release) })
	database := storetest.Open(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{
		SigningKey: testSigningKey,
		Sessions:   sessions,
		Membership: &identity.Membership{People: people, Sessions: sessions, SignIn: issuer, Rule: identity.GroupRule{Group: "dispatch-members"}},
	}
	outside := limiter.Outside()
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), Identity: person, Outside: outside, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	t.Cleanup(answerIssuer)
	socket := httptest.NewRequest(http.MethodGet, "/ws/doc/00000000-0000-4000-8000-000000000001", nil)
	socket.AddCookie(lapsedCookie(t, database, people, sessions, "lapsed@d.example"))
	answered := serveInBackground(documents, socket)
	waitUntil(t, "the socket's renewal waiting on the sign-in issuer, holding no place", func() bool {
		return issuer.entered.Load() == 1 && outside.Stats().InUse == 0
	})
	for range admission.OutsideLimit {
		if err := outside.Take(context.Background(), admission.HeartbeatCheck); err != nil {
			t.Fatalf("take a place of the share while the socket waits on the issuer: %v", err)
		}
	}
	defer func() {
		for range admission.OutsideLimit {
			outside.Release()
		}
	}()
	acquired := database.Pool.Stat().AcquireCount()
	answerIssuer()
	answer := testwait.Receive(t, "the socket once the issuer answered", answered)
	requireRefusedTakeBack(t, answer, answer.Code == http.StatusSwitchingProtocols, database.Pool, acquired)
}
