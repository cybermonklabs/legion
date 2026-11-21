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
	const window = 200 * time.Millisecond
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.MinRefresh = window
	// Generated before the window opens: generating an RSA key takes long enough to pass a short
	// window on its own, and this test is about the window, not about how slow that is.
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

	time.Sleep(window)
	key, err := keys.Key(context.Background(), "rotated-in", testAlgorithm)
	if err != nil {
		t.Fatalf("Key(the new key) once the window has passed: %v", err)
	}
	token := issuer.Mint(t, rotated, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))
	if !signs(t, token, key) {
		t.Error("Key answered a key the newly published key's token does not verify against")
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
	// A caller that has already hung up: the read fails before it reaches the issuer, which is
	// the cheap, race-free stand-in for an issuer that cannot be reached.
	hungUp, cancel := context.WithCancel(context.Background())
	cancel()

	key, err := keys.Key(hungUp, "exchange-key", testAlgorithm)
	if err != nil {
		t.Fatalf("Key(a cached key) with the read failing = %v, want the cached key", err)
	}
	token := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))
	if !signs(t, token, key) {
		t.Error("Key answered a key the issuer's token does not verify against")
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Errorf("key set read %d times, want 1: the failed read never reached the issuer", fetches)
	}

	if _, err := keys.Key(hungUp, "never-published", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Errorf("Key(a key the cached set does not hold) with the read failing = %v, want ErrKeySet", err)
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

// A key set that fails at once, read while no set has ever been read, is read once per
// FailureSpacing however many tokens arrive: every other caller in that time is answered the
// failure with no read. Once the spacing has passed, the next caller reads again, and a set that
// answers is kept. A key source starts with DefaultKeysFailureSpacing.
func TestAKeySetThatFailsAtOnceIsReadOncePerSpacing(t *testing.T) {
	if got := NewKeysAt("http://127.0.0.1:1/keys").FailureSpacing; got != DefaultKeysFailureSpacing {
		t.Fatalf("NewKeysAt's FailureSpacing = %s, want DefaultKeysFailureSpacing (%s)", got, DefaultKeysFailureSpacing)
	}
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = 300 * time.Millisecond
	issuer.FailKeys(http.StatusInternalServerError)

	const callers = 200
	for i := range callers {
		if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); !errors.Is(err, ErrKeySet) {
			t.Fatalf("caller %d while the key set answers 500: err = %v, want ErrKeySet", i, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times for %d callers within FailureSpacing, want 1", fetches, callers)
	}

	issuer.FailKeys(0)
	time.Sleep(keys.FailureSpacing + 100*time.Millisecond)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key once the spacing has passed and the key set answers: %v", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: one more read once the spacing passed", fetches)
	}
}

// During a key rotation the cached set lacks the new key id until a read finds it. A second token
// naming that id while the read that would settle it has not (it failed, within MinRefresh) is
// ErrKeySet, the issuer being unavailable, not ErrNoKey, a signature no published key makes: the
// key may well be published.
func TestANewKeyIDWhoseReadFailedIsNoForgery(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "key-a")
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key): %v", err)
	}
	issuer.PublishKey(t, "key-b")
	issuer.FailKeys(http.StatusInternalServerError)

	if _, err := keys.Key(context.Background(), "key-b", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("the first token naming key-b, whose read fails: err = %v, want ErrKeySet", err)
	}
	_, err := keys.Key(context.Background(), "key-b", testAlgorithm)
	if !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
		t.Fatalf("the second token naming key-b within MinRefresh of the failed read: err = %v, want ErrKeySet and not ErrNoKey", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: the cached read and the one key-b prompted", fetches)
	}
}

// A second token naming a new key id, arriving while the read its first token prompted is still
// under way, joins that read and is answered from the set it reads: it never gets ErrNoKey, a
// signature no published key makes, for a key the issuer does publish.
func TestASecondTokenForANewKeyJoinsTheReadUnderWay(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "key-a")
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key): %v", err)
	}
	issuer.PublishKey(t, "key-b")
	issuer.DelayKeys(500 * time.Millisecond)
	first := make(chan error, 1)
	go func() {
		_, err := keys.Key(context.Background(), "key-b", testAlgorithm)
		first <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for issuer.JWKSFetches() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the first token naming key-b never prompted a read within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := keys.Key(context.Background(), "key-b", testAlgorithm); err != nil {
		t.Fatalf("a second token naming key-b while the read it needs is under way: %v, want key-b from that read", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the first token naming key-b: %v", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: the second token joined the first one's read", fetches)
	}
}

// Callers that each carry a keys deadline (WithKeysDeadline) shorter than a slow key set share one
// read: each leaves at its deadline with ErrKeySet, its own context still live, the read goes on in
// the background, and the set it reads is kept, so a plain Key after it is answered from the cache
// with no read of its own.
func TestCallersWithAKeysDeadlineShareOneReadThatIsKept(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	issuer.DelayKeys(500 * time.Millisecond)
	const callers = 50
	var wg sync.WaitGroup
	errs := make([]error, callers)
	live := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx := WithKeysDeadline(context.Background(), time.Now().Add(100*time.Millisecond))
			_, errs[i] = keys.Key(ctx, "exchange-key", testAlgorithm)
			live[i] = ctx.Err()
		}()
	}
	began := time.Now()
	close(start)
	wg.Wait()
	if waited := time.Since(began); waited > 400*time.Millisecond {
		t.Fatalf("the callers took %s to leave, want each at its 100 ms keys deadline, before the 500 ms read ends", waited.Round(time.Millisecond))
	}
	for i, err := range errs {
		if !errors.Is(err, ErrKeySet) || live[i] != nil {
			t.Fatalf("caller %d past its keys deadline: err = %v and its context %v, want ErrKeySet and a live context", i, err, live[i])
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times for %d callers, want 1", fetches, callers)
	}
	time.Sleep(600 * time.Millisecond)
	issuer.DelayKeys(0)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("a plain Key once the background read has landed: %v, want the kept set", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times, want 1: the background read's set was kept", fetches)
	}
}

// Once a set is cached, a refresh that falls due (the set older than Lifetime) and that a keys
// deadline cuts short is no failure for a key the cached set holds: Key answers from that set, as
// it does when a refresh fails, rather than making the verification wait for the read or refusing
// it.
func TestAKeysDeadlineThatCutsAStaleRefreshServesTheCachedSet(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	issuer.DelayKeys(time.Second)
	ctx := WithKeysDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	start := time.Now()
	key, err := keys.Key(ctx, "exchange-key", testAlgorithm)
	if err != nil || key == nil {
		t.Fatalf("Key with a stale set and a keys deadline shorter than the refresh: %v, want the cached key", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Key answered after %s, want it at its 50 ms keys deadline from the cached set", elapsed.Round(time.Millisecond))
	}
}

// A verification whose keys deadline (WithKeysDeadline) passes before a slow key set answers is
// ErrIssuerUnavailable, the issuer being unavailable, at that deadline: never ErrCancelled, the
// end of the caller's own context, nor a verification failure, and never a wait to the read's own
// FetchTimeout.
func TestAKeysDeadlineEndsTheWaitAsUnavailable(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "signing-key")
	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	sound := issuer.Mint(t, signing, issuer.Claims(testSubject, testAudience))
	issuer.DelayKeys(3 * time.Second)

	ctx := WithKeysDeadline(context.Background(), time.Now().Add(200*time.Millisecond))
	start := time.Now()
	_, err := verifier.Verify(ctx, sound)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrIssuerUnavailable) || errors.Is(err, ErrCancelled) || errors.Is(err, ErrSignature) {
		t.Fatalf("Verify past its keys deadline = %v, want ErrIssuerUnavailable and neither ErrCancelled nor ErrSignature", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("the keys deadline ended the caller's context: %v", ctx.Err())
	}
	if elapsed > time.Second {
		t.Fatalf("Verify answered after %s, want it at its 200 ms keys deadline, not the read's end", elapsed.Round(time.Millisecond))
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
