package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// stalledIssuer is a sign-in issuer that answers no refresh until release closes, or the
// refresh's own bound passes, and then answers that it cannot be reached. entered counts the
// refreshes that have begun waiting on it.
type stalledIssuer struct {
	entered atomic.Int32
	release chan struct{}
}

func (i *stalledIssuer) Refresh(ctx context.Context, _ string) (oidc.Session, error) {
	i.entered.Add(1)
	select {
	case <-i.release:
	case <-ctx.Done():
	}
	return oidc.Session{}, fmt.Errorf("oidc: refresh: %w: the test's issuer answers none", oidc.ErrIssuerUnavailable)
}

// A sign-in issuer slow to answer holds none of the request pool's share outside admission: a
// document socket's admission and a heartbeat check give their place back while their caller's
// lapsed confirmation is renewed with the issuer (identity.Membership.Confirm). So while eight
// people whose confirmations have lapsed wait on refreshes the issuer has not answered - four
// document sockets admitting them (docs.Service.ServeHTTP) and four document sockets' heartbeat
// checks (signInStillResolves), as many as the share holds - no place of the share is held, and a
// DPoP presentation, whose decision takes a place (decideBrokerToken), takes one at once and is
// answered as the same presentation is with the share empty. Once the issuer answers that it
// cannot renew them, each socket is refused 503 SIGNIN_UNAVAILABLE and each check answers
// ErrSignInUnavailable, as before, and every place is free again.
func TestASignInIssuerSlowToAnswerHoldsNoPlaceOfTheShareOutsideAdmission(t *testing.T) {
	ctx := context.Background()
	limiter := admission.New(admission.DefaultLimits())
	issuer := &stalledIssuer{release: make(chan struct{})}
	answer := sync.OnceFunc(func() { close(issuer.release) })
	database := storetest.Open(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	sessions := store.NewPgSessionStore(database.Pool)
	person := identity.CookieIdentity{
		SigningKey: testSigningKey,
		Sessions:   sessions,
		Membership: &identity.Membership{People: people, Sessions: sessions, SignIn: issuer, Rule: identity.GroupRule{Group: "dispatch-members"}},
	}
	rig := newBrokerTokenRigOn(t, testServerOptions{store: database, admission: limiter, identity: person})
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), Identity: person, Outside: limiter.Outside(), Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	// Before the rig's own cleanups, so no work is still waiting on the issuer when the server and
	// its database close.
	var working sync.WaitGroup
	t.Cleanup(func() {
		answer()
		working.Wait()
	})

	caller := newBrokerCaller(t)
	const before, during = "a-token-presented-with-the-share-empty", "a-token-presented-while-the-issuer-stalls"
	rig.broker.Says(before, liveNamedToken(caller.jkt))
	rig.broker.Says(during, liveNamedToken(caller.jkt))
	emptyShare := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", before, nil))
	if emptyShare.Code != http.StatusOK {
		t.Fatalf("a presentation with the share empty: status=%d body=%s, want 200", emptyShare.Code, emptyShare.Body.String())
	}

	lapsed := make([]string, admission.OutsideLimit)
	cookies := make([]*http.Cookie, len(lapsed))
	for i := range lapsed {
		lapsed[i] = fmt.Sprintf("lapsed-%d@d.example", i)
		if err := people.SignIn(ctx, lapsed[i], fmt.Sprintf("refresh-token-%d", i)); err != nil {
			t.Fatalf("record %s's sign-in: %v", lapsed[i], err)
		}
		generation, err := sessions.EnsureSession(ctx, lapsed[i])
		if err != nil {
			t.Fatalf("establish %s's session: %v", lapsed[i], err)
		}
		if cookies[i], err = http.ParseSetCookie(auth.IssueSessionCookie(lapsed[i], generation, testSigningKey, false)); err != nil {
			t.Fatalf("parse %s's session cookie: %v", lapsed[i], err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `update people set confirmed_at = now() - interval '2 hours' where email = any($1)`, lapsed); err != nil {
		t.Fatalf("date the confirmations two hours back: %v", err)
	}

	checks := make(chan error, len(lapsed))
	sockets := make(chan *httptest.ResponseRecorder, len(lapsed))
	for i, cookie := range cookies {
		if i%2 == 0 {
			check := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
			check.AddCookie(cookie)
			working.Go(func() { checks <- rig.served.signInStillResolves(check) })
			continue
		}
		socket := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws/doc/00000000-0000-4000-8000-%012d", i), nil)
		socket.AddCookie(cookie)
		working.Go(func() {
			answered := httptest.NewRecorder()
			documents.ServeHTTP(answered, socket)
			sockets <- answered
		})
	}
	waitUntil(t, "every lapsed sign-in's refresh waiting on the issuer", func() bool {
		return int(issuer.entered.Load()) == len(lapsed)
	})

	stalled := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", during, nil))
	if stalled.Code != emptyShare.Code || stalled.Body.String() != emptyShare.Body.String() {
		t.Errorf("a presentation while %d refreshes wait on the issuer: status=%d body=%s, want it answered as with the share empty: status=%d body=%s",
			len(lapsed), stalled.Code, stalled.Body.String(), emptyShare.Code, emptyShare.Body.String())
	}
	if stats := limiter.Outside().Stats(); stats.InUse != 0 || stats.MaxQueued != 0 || stats.Refused != [len(stats.Refused)]admission.OutsideRefusals{} {
		t.Errorf("with %d refreshes waiting on the issuer, %d places of the share outside admission are held, %d waited at most and %v were refused, want none of any",
			len(lapsed), stats.InUse, stats.MaxQueued, stats.Refused)
	}

	answer()
	for range len(lapsed) / 2 {
		select {
		case err := <-checks:
			if !errors.Is(err, identity.ErrSignInUnavailable) {
				t.Errorf("a heartbeat check once the issuer answered it cannot renew: %v, want ErrSignInUnavailable", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a heartbeat check that waited on the issuer was never answered once it answered")
		}
		select {
		case answered := <-sockets:
			if answered.Code != http.StatusServiceUnavailable || !strings.Contains(answered.Body.String(), `"code":"SIGNIN_UNAVAILABLE"`) {
				t.Errorf("a document socket once the issuer answered it cannot renew: status=%d body=%s, want 503 SIGNIN_UNAVAILABLE", answered.Code, answered.Body.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a document socket that waited on the issuer was never answered once it answered")
		}
	}
	if stats := limiter.Outside().Stats(); stats.InUse != 0 || stats.Queued != 0 {
		t.Errorf("once every refresh was answered, %d places are held and %d wait, want none", stats.InUse, stats.Queued)
	}
}

// countedLogins is an Identity that counts the resolutions begun with it, so a test knows when
// every caller it set going has reached its resolution.
type countedLogins struct {
	identity.Identity
	begun atomic.Int32
}

func (c *countedLogins) Login(r *http.Request) (string, error) {
	c.begun.Add(1)
	return c.Identity.Login(r)
}

// An IAP key set slow to answer holds none of the request pool's share outside admission either: a
// document socket's admission and a heartbeat check give their place back while their caller's
// assertion is verified, since the verification may read IAP's key set - here for the first
// assertion - and reads no pool (identity.IAPIdentity.Login). So while four document sockets
// admitting a person behind IAP and four document sockets' heartbeat checks wait on a key set that
// has not answered, as many as the share holds, no place of the share is held, and a DPoP
// presentation takes one at once and is answered as the same presentation is with the share
// empty. Once the key set answers that it cannot be read, each socket is refused 503
// SIGNIN_UNAVAILABLE and each check answers ErrSignInUnavailable, as for a sign-in issuer that
// cannot be reached, and every place is free again.
func TestAnIAPKeySetSlowToAnswerHoldsNoPlaceOfTheShareOutsideAdmission(t *testing.T) {
	limiter := admission.New(admission.DefaultLimits())
	database := storetest.Open(t)
	signed := newIAPSocketKeys(t, database)
	var fetches atomic.Int32
	release := make(chan struct{})
	answer := sync.OnceFunc(func() { close(release) })
	keySet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "the test's key set answers none", http.StatusServiceUnavailable)
	}))
	t.Cleanup(keySet.Close)
	stalled := oidc.NewKeysAt(keySet.URL)
	// Past every wait of the test, so only answer ends the read.
	stalled.FetchTimeout = time.Minute
	person := &countedLogins{Identity: identity.IAPIdentity{
		Verifier: oidc.NewIAPVerifier(iapSocketAudience, stalled),
		Rule:     identity.DomainRule{Domain: "example.com"},
		People:   store.NewPgPeopleStore(database.Pool, testSigningKey, nil),
	}}
	rig := newBrokerTokenRigOn(t, testServerOptions{store: database, admission: limiter, identity: person})
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), Identity: person, Outside: limiter.Outside(), Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	// Before the rig's own cleanups and the key set's close, so no work is still waiting on the key
	// set when the server, its database and the key set close.
	var working sync.WaitGroup
	t.Cleanup(func() {
		answer()
		working.Wait()
	})

	caller := newBrokerCaller(t)
	const before, during = "a-token-presented-with-the-share-empty", "a-token-presented-while-the-key-set-stalls"
	rig.broker.Says(before, liveNamedToken(caller.jkt))
	rig.broker.Says(during, liveNamedToken(caller.jkt))
	emptyShare := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", before, nil))
	if emptyShare.Code != http.StatusOK {
		t.Fatalf("a presentation with the share empty: status=%d body=%s, want 200", emptyShare.Code, emptyShare.Body.String())
	}

	checks := make(chan error, admission.OutsideLimit)
	sockets := make(chan *httptest.ResponseRecorder, admission.OutsideLimit)
	for i := range admission.OutsideLimit {
		assertion := signed.assertion(t, time.Now().Add(9*time.Minute))
		if i%2 == 0 {
			check := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
			check.Header = assertion
			working.Go(func() { checks <- rig.served.signInStillResolves(check) })
			continue
		}
		socket := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/ws/doc/00000000-0000-4000-8000-%012d", i), nil)
		socket.Header = assertion
		working.Go(func() {
			answered := httptest.NewRecorder()
			documents.ServeHTTP(answered, socket)
			sockets <- answered
		})
	}
	waitUntil(t, "every caller's resolution begun, and the key set's read", func() bool {
		return int(person.begun.Load()) == admission.OutsideLimit && fetches.Load() > 0
	})
	waitUntil(t, "every place of the share given back while the key set stalls", func() bool {
		return limiter.Outside().Stats().InUse == 0
	})

	stalledShare := rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", during, nil))
	if stalledShare.Code != emptyShare.Code || stalledShare.Body.String() != emptyShare.Body.String() {
		t.Errorf("a presentation while %d resolutions wait on the key set: status=%d body=%s, want it answered as with the share empty: status=%d body=%s",
			admission.OutsideLimit, stalledShare.Code, stalledShare.Body.String(), emptyShare.Code, emptyShare.Body.String())
	}
	if stats := limiter.Outside().Stats(); stats.InUse != 0 || stats.MaxQueued != 0 || stats.Refused != [len(stats.Refused)]admission.OutsideRefusals{} {
		t.Errorf("with %d resolutions waiting on the key set, %d places of the share outside admission are held, %d waited at most and %v were refused, want none of any",
			admission.OutsideLimit, stats.InUse, stats.MaxQueued, stats.Refused)
	}

	answer()
	for range admission.OutsideLimit / 2 {
		select {
		case err := <-checks:
			if !errors.Is(err, identity.ErrSignInUnavailable) {
				t.Errorf("a heartbeat check once the key set answered it cannot be read: %v, want ErrSignInUnavailable", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a heartbeat check that waited on the key set was never answered once it answered")
		}
		select {
		case answered := <-sockets:
			if answered.Code != http.StatusServiceUnavailable || !strings.Contains(answered.Body.String(), `"code":"SIGNIN_UNAVAILABLE"`) {
				t.Errorf("a document socket once the key set answered it cannot be read: status=%d body=%s, want 503 SIGNIN_UNAVAILABLE", answered.Code, answered.Body.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a document socket that waited on the key set was never answered once it answered")
		}
	}
	if stats := limiter.Outside().Stats(); stats.InUse != 0 || stats.Queued != 0 {
		t.Errorf("once the key set answered, %d places are held and %d wait, want none", stats.InUse, stats.Queued)
	}
}

// heldLogins is an Identity that holds each resolution at its start until the test sends on
// proceed, and answers on answered what the resolution it wraps answered and how the share outside
// admission stood as it answered, so a test can set work waiting for a place before a resolution
// runs and read whether that work was handed one while it ran. stop, closed as the test ends,
// lets a held resolution go unanswered.
type heldLogins struct {
	identity.Identity
	outside  *admission.Outside
	entered  chan struct{}
	proceed  chan struct{}
	stop     chan struct{}
	answered chan heldLogin
}

// heldLogin is one resolution's answer, and the share outside admission as it answered.
type heldLogin struct {
	email string
	err   error
	share admission.OutsideStats
}

func (h *heldLogins) Login(r *http.Request) (string, error) {
	h.entered <- struct{}{}
	select {
	case <-h.proceed:
	case <-h.stop:
		return "", errors.New("the test ended before it let the resolution go")
	}
	email, err := h.Identity.Login(r)
	h.answered <- heldLogin{email: email, err: err, share: h.outside.Stats()}
	return email, err
}

// An IAP key set already read and fresh keeps the place through the verification: a heartbeat
// check's resolution and a document socket's admission hold their place of the request pool's
// share outside admission from the moment they take it to the person's record, since the
// verification is answered from the set in memory and waits on nothing, so it runs no
// before-wait hook (oidc.WithBeforeWait, identity.IAPIdentity.Login). Only a verification that
// will read the set gives its place back
// (TestAnIAPKeySetSlowToAnswerHoldsNoPlaceOfTheShareOutsideAdmission). So with every other place
// held and one more work waiting for a place, the resolution answers its person, the waiting work
// is still waiting when it does - the place was never given back for it to take - nothing was
// refused, and the key set was not read again. Given back and taken again around every
// verification, the place went to the waiting work, and the resolution waited out the share's
// second for it and was answered a busy pool.
func TestAnIAPKeySetAlreadyReadKeepsThePlaceOfTheShareThroughTheVerification(t *testing.T) {
	limiter := admission.New(admission.DefaultLimits())
	database := storetest.Open(t)
	signed := newIAPSocketKeys(t, database)
	person := &heldLogins{
		Identity: signed.identity, outside: limiter.Outside(),
		entered: make(chan struct{}, 1), proceed: make(chan struct{}), stop: make(chan struct{}),
		answered: make(chan heldLogin, 1),
	}
	rig := newBrokerTokenRigOn(t, testServerOptions{store: database, admission: limiter, identity: person})
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), Identity: person, Outside: limiter.Outside(), Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	var working sync.WaitGroup
	t.Cleanup(func() {
		close(person.stop)
		working.Wait()
	})

	// The first assertion reads the key set; every one after it, inside the set's Lifetime, is
	// answered from memory.
	warm := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
	warm.Header = signed.assertion(t, time.Now().Add(9*time.Minute))
	if _, err := signed.identity.Login(warm); err != nil {
		t.Fatalf("the first assertion, which reads the key set: %v", err)
	}
	read := signed.keys.JWKSFetches()
	if read != 1 {
		t.Fatalf("the key set was read %d times for the first assertion, want 1", read)
	}

	for _, caller := range []struct {
		name string
		run  func(header http.Header)
	}{
		{"a heartbeat check", func(header http.Header) {
			check := httptest.NewRequest(http.MethodGet, "/ws/doc/room", nil)
			check.Header = header
			_ = rig.served.signInStillResolves(check)
		}},
		{"a document socket's admission", func(header http.Header) {
			socket := httptest.NewRequest(http.MethodGet, "/ws/doc/00000000-0000-4000-8000-000000000001", nil)
			socket.Header = header
			documents.ServeHTTP(httptest.NewRecorder(), socket)
		}},
	} {
		t.Run(caller.name, func(t *testing.T) {
			outside := limiter.Outside()
			// Every place of the share but one is held, which the resolution's caller takes.
			held := 0
			releaseHeld := func() {
				for ; held > 0; held-- {
					outside.Release()
				}
			}
			defer releaseHeld()
			for range admission.OutsideLimit - 1 {
				if err := outside.Take(t.Context(), admission.DPoPDecision); err != nil {
					t.Fatalf("hold a place of the share: %v", err)
				}
				held++
			}
			refusedBefore := outside.Stats().Refused
			done := make(chan struct{})
			assertion := signed.assertion(t, time.Now().Add(9*time.Minute))
			working.Go(func() {
				defer close(done)
				caller.run(assertion)
			})
			select {
			case <-person.entered:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never reached its resolution", caller.name)
			}
			if stats := outside.Stats(); stats.InUse != admission.OutsideLimit {
				t.Fatalf("%s is resolving its caller with %d places of the share held, want all %d: it holds the last", caller.name, stats.InUse, admission.OutsideLimit)
			}
			// One more work waits for a place: the next place given back goes to it.
			waiting := make(chan error, 1)
			working.Go(func() { waiting <- outside.Take(context.Background(), admission.DPoPDecision) })
			waitUntil(t, "a work waiting for a place of the full share", func() bool {
				return outside.Stats().Queued == 1
			})

			person.proceed <- struct{}{}
			var answer heldLogin
			select {
			case answer = <-person.answered:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s's resolution never answered", caller.name)
			}
			if answer.err != nil || answer.email != "a.person@example.com" {
				t.Errorf("%s's resolution with the key set read and fresh answered %q, %v; want a.person@example.com", caller.name, answer.email, answer.err)
			}
			if answer.share.Queued != 1 || answer.share.InUse != admission.OutsideLimit {
				t.Errorf("as %s's resolution answered, %d places were held and %d works waited; want all %d held and the 1 waiting still waiting: the resolution gave its place back and the waiting work took it",
					caller.name, answer.share.InUse, answer.share.Queued, admission.OutsideLimit)
			}
			if refused := answer.share.Refused; refused != refusedBefore {
				t.Errorf("%s's resolution with the key set read and fresh: refusals %v, before it %v, want none more: it waited for its place again and was refused",
					caller.name, refused, refusedBefore)
			}
			if fetches := signed.keys.JWKSFetches(); fetches != read {
				t.Errorf("%s's resolution read the key set (%d reads, want %d): it is read and fresh", caller.name, fetches, read)
			}

			releaseHeld()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("%s never ended once the share's places came free", caller.name)
			}
			select {
			case err := <-waiting:
				if err == nil {
					outside.Release()
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the work waiting for a place was never given one")
			}
			if stats := outside.Stats(); stats.InUse != 0 || stats.Queued != 0 {
				t.Errorf("once %s ended, %d places are held and %d wait, want none", caller.name, stats.InUse, stats.Queued)
			}
		})
	}
}
