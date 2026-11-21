package oidc

import (
	"context"
	"crypto"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// testAlgorithm is the algorithm oidctest signs and publishes with, the one a caller asks Key for.
const testAlgorithm = string(jose.RS256)

// keysFor publishes one key as kid and returns the key source for the issuer serving it, with
// nothing fetched yet: NewKeys reads discovery alone, so a test may count the first key-set read.
func keysFor(t *testing.T, issuer *oidctest.Issuer, kid string) (*Keys, *oidctest.Key) {
	t.Helper()
	signing := issuer.PublishKey(t, kid)
	keys, err := NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("NewKeys(%s): %v", issuer.URL(), err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 0 {
		t.Fatalf("NewKeys read the key set %d times, want 0: the first Key reads it", fetches)
	}
	return keys, signing
}

// signs reports whether raw, a token the test minted, verifies against key: the one way to show
// that Key answered the material the issuer signs with rather than some other published key.
func signs(t *testing.T, raw string, key crypto.PublicKey) bool {
	t.Helper()
	signed, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("parse the minted token: %v", err)
	}
	_, err = signed.Verify(key)
	return err == nil
}

// onTestClock puts keys on a clock the test moves by hand, so the test passes RetrySpacing,
// Lifetime or MinRefresh by advancing it, never by sleeping.
func onTestClock(keys *Keys) *testClock {
	clock := newTestClock()
	keys.now = clock.Now
	return clock
}

// awaitLastRead waits for keys' last key-set read to end, whatever its outcome.
func awaitLastRead(t *testing.T, keys *Keys) {
	t.Helper()
	keys.mu.Lock()
	read := keys.last
	keys.mu.Unlock()
	if read == nil {
		t.Fatal("no key-set read was started")
	}
	select {
	case <-read.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the key-set read did not end within 30 s")
	}
}

func TestKeyAnswersThePublishedKey(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "exchange-key")
	token := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))

	for _, kid := range []string{"exchange-key", ""} {
		key, err := keys.Key(context.Background(), kid, testAlgorithm)
		if err != nil {
			t.Fatalf("Key(%q): %v", kid, err)
		}
		if !signs(t, token, key) {
			t.Errorf("Key(%q) answered a key the issuer's token does not verify against", kid)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Errorf("key set read %d times for two answers, want 1: the set is cached", fetches)
	}
}

func TestKeyReadsTheSetOnceForAnUnknownKeyIDInTheWindow(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		_, err := keys.Key(context.Background(), "never-published", testAlgorithm)
		if !errors.Is(err, ErrNoKey) {
			t.Fatalf("Key(an unpublished key id), attempt %d = %v, want ErrNoKey", attempt, err)
		}
		if !strings.Contains(err.Error(), "never-published") {
			t.Errorf("Key error %q names no key id", err)
		}
		if fetches := issuer.JWKSFetches(); fetches != 2 {
			t.Errorf("key set read %d times after attempt %d, want 2: the first attempt refetches, the second is inside MinRefresh", fetches, attempt)
		}
	}
}

func TestKeyFindsAKeyPublishedAfterTheWindowHasPassed(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	clock := onTestClock(keys)
	rotated := oidctest.NewKey(t, "rotated-in")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	if _, err := keys.Key(context.Background(), "rotated-in", testAlgorithm); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Key(a key id nobody has published yet) = %v, want ErrNoKey", err)
	}

	issuer.Publish(rotated)
	if _, err := keys.Key(context.Background(), "rotated-in", testAlgorithm); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Key(the new key) inside the window = %v, want ErrNoKey: the window holds the read back", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times inside the window, want 2", fetches)
	}

	clock.advance(keys.MinRefresh)
	key, err := keys.Key(context.Background(), "rotated-in", testAlgorithm)
	if err != nil {
		t.Fatalf("Key(the new key) once the window has passed: %v", err)
	}
	token := issuer.Mint(t, rotated, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))
	if !signs(t, token, key) {
		t.Error("Key answered a key the newly published key's token does not verify against")
	}
}

// With no set held yet, an issuer whose key set fails fast is read once per RetrySpacing, not once
// per Key: every Key in that time is answered the failed read's ErrKeySet, and the next read
// starts once RetrySpacing has passed. A cold verifier during an outage then multiplies no reads.
func TestAColdKeysReadsAFailingIssuerOncePerRetrySpacing(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	clock := onTestClock(keys)
	issuer.FailKeys(http.StatusInternalServerError)
	ask := func(calls int) {
		t.Helper()
		for call := 1; call <= calls; call++ {
			if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
				t.Fatalf("call %d, with no set held and the key set answering 500 = %v, want ErrKeySet alone", call, err)
			}
			awaitLastRead(t, keys)
		}
	}
	ask(20)
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Errorf("key set read %d times for 20 calls with no set held, want 1: the next read is held back for RetrySpacing (%s)", fetches, keys.RetrySpacing)
	}
	clock.advance(keys.RetrySpacing)
	ask(20)
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times once RetrySpacing has passed, want 2", fetches)
	}
}

// A read outlives the caller that started it: the caller's context ends during its wait, the
// issuer answers after that, and the next caller is served from the set that read filled, with
// one key-set request in all. On a real server the request's context ends when its handler
// returns, so a read tied to it would never fill the cache from an issuer slower than FetchWait.
func TestKeyReadOutlivesTheCallerThatStartedIt(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FetchWait = 100 * time.Millisecond
	issuer.DelayKeys(400 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := keys.Key(ctx, "exchange-key", testAlgorithm)
	cancel()
	if !errors.Is(err, ErrKeySet) {
		t.Fatalf("Key on a context that ends during its wait = %v, want ErrKeySet", err)
	}
	awaitLastRead(t, keys)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key once the first caller's read has landed = %v, want the key from that read", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Errorf("key set read %d times, want 1: the first caller's read outlives it", fetches)
	}
}

func TestKeyKeepsServingTheCachedSetWhenAReadFails(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "exchange-key")
	// Every Key reads the issuer again, so the cached set is only ever the fallback.
	keys.Lifetime = 0
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	issuer.FailKeys(http.StatusInternalServerError)

	key, err := keys.Key(context.Background(), "exchange-key", testAlgorithm)
	if err != nil {
		t.Fatalf("Key(a cached key) with the read failing = %v, want the cached key", err)
	}
	token := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))
	if !signs(t, token, key) {
		t.Error("Key answered a key the issuer's token does not verify against")
	}

	if _, err := keys.Key(context.Background(), "never-published", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Errorf("Key(a key the cached set does not hold) with the read failing = %v, want ErrKeySet", err)
	}
}

// A Keys as NewKeys builds it, the one the secrets broker and Dispatch verify through, waits a
// key-set read out, up to FetchTimeout: a cold read slower than the listener's 3 s wait but inside
// FetchTimeout answers the key, never ErrKeySet, which the broker would answer as a refusal. Only
// a caller that sets FetchWait (the listener) gives up sooner.
func TestKeysFromNewKeysWaitASlowReadOut(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	issuer.DelayKeys(3500 * time.Millisecond)
	start := time.Now()
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key on a cold cache whose read takes 3.5 s, inside FetchTimeout (%s) = %v after %s, want the key", keys.FetchTimeout, err, time.Since(start).Round(time.Millisecond))
	}
}

// A set past its Lifetime that holds the key answers at once, from the cache, and the issuer is
// read again behind the answer: an issuer that hangs costs a token the cache can judge nothing. A
// Key that waited on that read instead would get its failure once FetchTimeout ends it, not the
// key.
func TestKeyAnswersAStaleSetAtOnceAndReadsItAgainBehind(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	clock := onTestClock(keys)
	keys.FetchTimeout = 500 * time.Millisecond
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	clock.advance(keys.Lifetime)
	issuer.DelayKeys(time.Minute)

	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(a key the stale set holds) while the issuer hangs = %v, want the key from the cache", err)
	}
	awaitLastRead(t, keys)
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: the stale set is read again behind its answer", fetches)
	}
}

// An issuer that accepts the connection and never answers holds no Key past FetchWait: the caller
// gets ErrKeySet, the issuer unavailable, inside its own window, while the read goes on under
// FetchTimeout. A key rotated in meanwhile gets the same answer while the read is in flight and,
// once it has failed, until RetrySpacing has passed, and never ErrNoKey, which would call a token
// the issuer may well have signed a forgery.
func TestKeyWaitsAtMostFetchWaitOnAnIssuerThatHangs(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	onTestClock(keys)
	keys.FetchWait, keys.FetchTimeout = 200*time.Millisecond, 2*time.Second
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	issuer.DelayKeys(time.Minute)
	for call := 1; call <= 3; call++ {
		start := time.Now()
		_, err := keys.Key(context.Background(), "rotated-in", testAlgorithm)
		elapsed := time.Since(start)
		if !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
			t.Errorf("call %d, a key id the set does not hold while the issuer hangs = %v, want ErrKeySet alone", call, err)
		}
		if elapsed >= keys.FetchTimeout {
			t.Errorf("call %d waited %s, want FetchWait (%s), well inside the read's FetchTimeout (%s)", call, elapsed, keys.FetchWait, keys.FetchTimeout)
		}
		if call == 2 {
			// The read ends at FetchTimeout, and its failure answers the third call: the test's
			// clock has not moved, so RetrySpacing has not passed since it failed.
			awaitLastRead(t, keys)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: one read in flight, then none within RetrySpacing of its failure", fetches)
	}
}

// A stale set that holds the key answers every Key while the issuer fails fast, and is read
// again behind those answers no more than once per RetrySpacing, not once per Key: an outage
// multiplies no reads.
func TestKeyReadsAStaleSetAgainOncePerRetrySpacingWhileReadsFail(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	clock := onTestClock(keys)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	clock.advance(keys.Lifetime)
	issuer.FailKeys(http.StatusInternalServerError)
	answer := func(calls int) {
		t.Helper()
		for call := 1; call <= calls; call++ {
			if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
				t.Fatalf("call %d, a key the stale set holds while reads fail = %v, want the key", call, err)
			}
			awaitLastRead(t, keys)
		}
	}
	answer(20)
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times for 20 answers from a stale set, want 2: the first read and one failed refresh, the next held back for RetrySpacing (%s)", fetches, keys.RetrySpacing)
	}
	clock.advance(keys.RetrySpacing)
	answer(20)
	if fetches := issuer.JWKSFetches(); fetches != 3 {
		t.Errorf("key set read %d times once RetrySpacing has passed, want 3: one more failed refresh, and the next held back again", fetches)
	}
}

// A key rotated in while the key set answers 500 reaches no verdict on any call, not the first
// alone: each gets the failed read's ErrKeySet. Once the issuer is back the next read comes
// RetrySpacing after the failed one ended, not the minute of MinRefresh a read that succeeded
// holds the next one back for, so the rotated key is found within Dispatch's retry window.
func TestKeyAnswersARotationDuringAnOutageUnavailableUntilAReadSucceeds(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	clock := onTestClock(keys)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	issuer.FailKeys(http.StatusInternalServerError)
	rotated := oidctest.NewKey(t, "rotated-in")
	issuer.Publish(rotated)
	for call := 1; call <= 3; call++ {
		if _, err := keys.Key(context.Background(), "rotated-in", testAlgorithm); !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
			t.Fatalf("call %d with the key set answering 500 = %v, want ErrKeySet alone", call, err)
		}
		awaitLastRead(t, keys)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: a failed read holds the next one back", fetches)
	}
	issuer.FailKeys(0)
	clock.advance(keys.RetrySpacing)
	key, err := keys.Key(context.Background(), "rotated-in", testAlgorithm)
	if err != nil {
		t.Fatalf("Key(the rotated key) once the key set reads again, past RetrySpacing and inside MinRefresh (%s): %v", keys.MinRefresh, err)
	}
	if !signs(t, issuer.Mint(t, rotated, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker")), key) {
		t.Error("Key answered a key the rotated key's token does not verify against")
	}
	if _, err := keys.Key(context.Background(), "never-published", testAlgorithm); !errors.Is(err, ErrNoKey) {
		t.Errorf("Key(a key id nobody publishes) after a good read = %v, want ErrNoKey", err)
	}
}

// Callers racing on a key id the set does not hold while the key set cannot be read are each
// answered that the issuer is unavailable, never ErrNoKey, which the verifier calls a forgery and
// the listener answers 401: the miss is decided under one lock, so no caller acts on a look taken
// before another caller started the read that is now in flight. The race is narrow,
// so the test runs it in many rounds, each on a fresh cache that has read the set once.
func TestKeyAnswersRacingCallersDuringARotationOutageUnavailable(t *testing.T) {
	const rounds, callers = 25, 64
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "exchange-key")
	for round := range rounds {
		keys, err := NewKeys(context.Background(), issuer.URL())
		if err != nil {
			t.Fatalf("round %d: NewKeys: %v", round, err)
		}
		issuer.FailKeys(0)
		issuer.DelayKeys(0)
		if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
			t.Fatalf("round %d: Key(the published key): %v", round, err)
		}
		before := issuer.JWKSFetches()
		issuer.FailKeys(http.StatusInternalServerError)
		// The read is slow enough that every caller arrives while it is in flight.
		issuer.DelayKeys(50 * time.Millisecond)
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, callers)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = keys.Key(context.Background(), "rotated-in", testAlgorithm)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
				t.Errorf("round %d, caller %d of %d racing on a rotated key id while the key set answers 500 = %v, want ErrKeySet alone", round, i, callers, err)
			}
		}
		if fetches := issuer.JWKSFetches() - before; fetches != 1 {
			t.Errorf("round %d: key set read %d times for %d racing callers, want 1", round, fetches, callers)
		}
	}
}

func TestKeyReadsTheSetOnceForConcurrentCallers(t *testing.T) {
	issuer := oidctest.New(t)
	// The read is slow enough that every caller arrives while the first one is still waiting,
	// which is the case this test is about.
	issuer.DelayKeys(100 * time.Millisecond)
	keys, _ := keysFor(t, issuer, "exchange-key")

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = keys.Key(context.Background(), "exchange-key", testAlgorithm)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: Key: %v", i, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Errorf("key set read %d times for %d concurrent callers, want 1", fetches, callers)
	}
}

// A Keys holding no key set yet reads on every Key, with no spacing after a read that failed, so
// what bounds the issuer's load during a cold outage is that the reads never overlap: every caller
// that arrives while a read is in flight waits on that read rather than starting its own. Against
// an issuer that fails slowly and one that hangs, many concurrent cold callers cost one key-set
// request, and each is answered that the set could not be read.
func TestKeyJoinsTheColdReadInFlightWhileTheIssuerFails(t *testing.T) {
	for _, outage := range []struct {
		name  string
		start func(*oidctest.Issuer)
	}{
		{"a key set that answers 500 after 200 ms", func(i *oidctest.Issuer) {
			i.FailKeys(http.StatusInternalServerError)
			i.DelayKeys(200 * time.Millisecond)
		}},
		{"a key set that never answers", func(i *oidctest.Issuer) { i.DelayKeys(time.Minute) }},
	} {
		t.Run(outage.name, func(t *testing.T) {
			issuer := oidctest.New(t)
			keys, _ := keysFor(t, issuer, "exchange-key")
			keys.FetchWait, keys.FetchTimeout = 300*time.Millisecond, time.Second
			outage.start(issuer)
			const callers = 32
			var wg sync.WaitGroup
			start := make(chan struct{})
			errs := make([]error, callers)
			for i := range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, errs[i] = keys.Key(context.Background(), "exchange-key", testAlgorithm)
				}()
			}
			close(start)
			wg.Wait()
			for i, err := range errs {
				if !errors.Is(err, ErrKeySet) {
					t.Errorf("cold caller %d of %d = %v, want ErrKeySet", i, callers, err)
				}
			}
			if fetches := issuer.JWKSFetches(); fetches != 1 {
				t.Errorf("key set read %d times for %d concurrent cold callers, want 1: each waits on the read in flight", fetches, callers)
			}
		})
	}
}

func TestNewKeysRefusesAnIssuerThatDoesNotDiscover(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.FailDiscovery(http.StatusInternalServerError)
	keys, err := NewKeys(context.Background(), issuer.URL())
	if err == nil {
		t.Fatalf("NewKeys(an issuer whose discovery fails) = %v, want an error", keys)
	}
	if !strings.Contains(err.Error(), issuer.URL()) {
		t.Errorf("NewKeys error %q names no issuer", err)
	}
}

func TestSelectKeyChoosesTheOneUsableKey(t *testing.T) {
	signing := jose.JSONWebKey{Key: "signing", KeyID: "exchange-key", Algorithm: testAlgorithm, Use: "sig"}
	encryption := jose.JSONWebKey{Key: "encryption", KeyID: "exchange-key", Algorithm: testAlgorithm, Use: "enc"}
	elliptic := jose.JSONWebKey{Key: "elliptic", KeyID: "elliptic-key", Algorithm: string(jose.ES256), Use: "sig"}
	unmarked := jose.JSONWebKey{Key: "unmarked", KeyID: "unmarked-key"}
	second := jose.JSONWebKey{Key: "second", KeyID: "second-key", Algorithm: testAlgorithm, Use: "sig"}

	for _, c := range []struct {
		name string
		set  []jose.JSONWebKey
		kid  string
		alg  string
		want any
		err  error
	}{
		{name: "the key the token names", set: []jose.JSONWebKey{signing, second}, kid: "exchange-key", alg: testAlgorithm, want: "signing"},
		{name: "an encryption key under the same id is passed over", set: []jose.JSONWebKey{encryption, signing}, kid: "exchange-key", alg: testAlgorithm, want: "signing"},
		{name: "a key marked for no use is usable", set: []jose.JSONWebKey{unmarked}, kid: "unmarked-key", alg: testAlgorithm, want: "unmarked"},
		{name: "no key id, one usable key", set: []jose.JSONWebKey{signing, elliptic}, alg: testAlgorithm, want: "signing"},
		{name: "a key for another algorithm", set: []jose.JSONWebKey{elliptic}, kid: "elliptic-key", alg: testAlgorithm, err: ErrNoKey},
		{name: "a key id the set does not hold", set: []jose.JSONWebKey{signing}, kid: "never-published", alg: testAlgorithm, err: ErrNoKey},
		{name: "an empty set", kid: "exchange-key", alg: testAlgorithm, err: ErrNoKey},
		{name: "no key id, no usable key", set: []jose.JSONWebKey{elliptic}, alg: testAlgorithm, err: ErrNoKey},
		{name: "no key id, two usable keys", set: []jose.JSONWebKey{signing, second}, alg: testAlgorithm, err: ErrAmbiguousKey},
		{name: "one key id, two keys under it", set: []jose.JSONWebKey{signing, {Key: "twin", KeyID: "exchange-key", Use: "sig"}}, kid: "exchange-key", alg: testAlgorithm, err: ErrAmbiguousKey},
		{name: "no algorithm asked for narrows nothing", set: []jose.JSONWebKey{elliptic}, kid: "elliptic-key", want: "elliptic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectKey(c.set, c.kid, c.alg)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("selectKey(%q, %q) = %v, %v; want %v", c.kid, c.alg, got, err, c.err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("selectKey(%q, %q) = %v, %v; want %v", c.kid, c.alg, got, err, c.want)
			}
		})
	}
}
