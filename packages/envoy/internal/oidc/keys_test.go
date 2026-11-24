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
	"github.com/sjawhar/envoy/internal/testwait"
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

// A set stale by MinRefresh or more is read again before it answers. When that read fails, a key
// the cached set holds is still answered from it, and a key it does not hold is the read's
// failure, ErrKeySet: an issuer that is down stops no verification it does not have to.
func TestAnOldSetWhoseReadFailsStillAnswersTheKeysItHolds(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "exchange-key")
	// Every set is past Lifetime + MinRefresh, so every Key reads the issuer first and the cached
	// set is only ever the fallback.
	keys.Lifetime, keys.MinRefresh = 0, 0
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
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Errorf("key set read %d times, want 2: the first read and the one that failed", fetches)
	}

	if _, err := keys.Key(context.Background(), "never-published", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Errorf("Key(a key the cached set does not hold) with the read failing = %v, want ErrKeySet", err)
	}
}

// A caller whose keys deadline passes while a set stale by MinRefresh or more is read again is
// answered from the cached set where it holds the key, at that deadline: the read carries on and
// is kept.
func TestAKeysDeadlineCutsTheReadOfAnOldSetToTheCachedKey(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime, keys.MinRefresh = time.Millisecond, time.Millisecond
	time.Sleep(10 * time.Millisecond)
	issuer.DelayKeys(time.Second)
	ctx := WithKeysDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	start := time.Now()
	key, err := keys.Key(ctx, "exchange-key", testAlgorithm)
	if err != nil || key == nil {
		t.Fatalf("Key on a set past Lifetime + MinRefresh, its read slower than the keys deadline: %v, want the cached key", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("Key answered after %s, want it at its 50 ms keys deadline, after waiting on the read", elapsed.Round(time.Millisecond))
	}
}

// For a caller that waits, while the issuer answers within the caller's wait, a key it has
// withdrawn verifies for at most Lifetime + MinRefresh after the last good read: a token signed by
// it, presented to a cache that sat idle for longer, is read against the issuer first and refused,
// never answered from the old set.
func TestAWithdrawnKeyIsRefusedOnceTheSetIsPastMinRefresh(t *testing.T) {
	issuer := oidctest.New(t)
	keys, withdrawn := keysFor(t, issuer, "key-a")
	keys.Lifetime, keys.MinRefresh = 100*time.Millisecond, 200*time.Millisecond
	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	token := issuer.Mint(t, withdrawn, issuer.Claims(testSubject, testAudience))
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify while key-a is published: %v", err)
	}
	issuer.Withdraw("key-a")
	issuer.PublishKey(t, "key-b")
	time.Sleep(time.Second)
	claims, err := verifier.Verify(context.Background(), token)
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify of a token signed by the withdrawn key-a after 1 s idle (Lifetime 100 ms, MinRefresh 200 ms) = %+v, %v; want ErrSignature", claims, err)
	}
}

// A caller whose own context has ended before the read of a set past Lifetime + MinRefresh lands,
// a client that hung up or half-closed, gets no answer from the cached set: a token signed by a key
// the issuer has withdrawn is refused, never accepted from a set of any age. The caller has gone, so
// the fallback that keeps serving a live caller serves nobody here.
func TestAHungUpCallerGetsNoAnswerFromAnOldSet(t *testing.T) {
	issuer := oidctest.New(t)
	keys, withdrawn := keysFor(t, issuer, "key-a")
	keys.Lifetime, keys.MinRefresh = 100*time.Millisecond, 200*time.Millisecond
	verifier := NewWithKeys(issuer.URL(), testAudience, keys)
	token := issuer.Mint(t, withdrawn, issuer.Claims(testSubject, testAudience))
	if _, err := verifier.Verify(context.Background(), token); err != nil {
		t.Fatalf("Verify while key-a is published: %v", err)
	}
	issuer.Withdraw("key-a")
	issuer.PublishKey(t, "key-b")
	issuer.DelayKeys(200 * time.Millisecond)
	time.Sleep(time.Second)
	hungUp, cancel := context.WithCancel(context.Background())
	cancel()
	claims, err := verifier.Verify(hungUp, token)
	if err == nil || !errors.Is(err, ErrCancelled) {
		t.Fatalf("Verify for a hung-up caller of a token signed by the withdrawn key-a, the set past Lifetime + MinRefresh = %+v, %v; want ErrCancelled, never the cached key", claims, err)
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

// A stale set (older than Lifetime, by less than MinRefresh) that holds the key asked for answers
// at once, with no wait for its refresh from a key set 500 ms slow: the refresh runs in the
// background, and the set it reads is kept.
func TestAStaleSetThatHoldsTheKeyAnswersAtOnceAndRefreshesInTheBackground(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	_, first := keys.state()
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	issuer.DelayKeys(500 * time.Millisecond)
	ctx := WithKeysDeadline(context.Background(), time.Now().Add(time.Second))
	start := time.Now()
	key, err := keys.Key(ctx, "exchange-key", testAlgorithm)
	if err != nil || key == nil {
		t.Fatalf("Key with a stale set that holds the key: %v, want the cached key", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Key answered after %s, want at once from the cached set, not after the refresh or the keys deadline", elapsed.Round(time.Millisecond))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, fetched := keys.state(); fetched.After(first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the background refresh never landed within 5 s (key set read %d times)", issuer.JWKSFetches())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: the first read and one background refresh", fetches)
	}
}

// A failed read is followed by ErrKeySet for a key id the cache lacks only while no good read has
// come since: once a read has succeeded, a key id that set lacks is ErrNoKey, the 401 of a key the
// issuer does not publish, even within MinRefresh of the earlier failure.
func TestAGoodReadAfterAFailedOneSettlesAMissingKeyID(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "key-a")
	keys.FailureSpacing = 50 * time.Millisecond
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key): %v", err)
	}
	issuer.FailKeys(http.StatusInternalServerError)
	if _, err := keys.Key(context.Background(), "never-published-1", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("a key id whose miss read fails: err = %v, want ErrKeySet", err)
	}
	issuer.FailKeys(0)
	time.Sleep(keys.FailureSpacing + 50*time.Millisecond)
	if _, err := keys.Key(context.Background(), "never-published-2", testAlgorithm); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a key id the good read once the spacing passed does not hold: err = %v, want ErrNoKey", err)
	}
	_, err := keys.Key(context.Background(), "never-published-3", testAlgorithm)
	if !errors.Is(err, ErrNoKey) || errors.Is(err, ErrKeySet) {
		t.Fatalf("a key id the latest, good read does not hold, within MinRefresh of the earlier failure: err = %v, want ErrNoKey and not ErrKeySet", err)
	}
}

// A caller that hangs up ends only its own wait: the read it started runs on, reaches the issuer
// and is kept, so the next caller is answered from that read with no read of its own. The caller
// that hung up is ErrKeySet in either order of its leaving and its read's end, each held here
// (hungUpCaller): its read held before the issuer when the caller has its answer, and its read
// ended before the caller looks at it, the order a loaded runner gave in CI. The second order runs
// 64 times: where wait leaves the choice to a select, each run is the read's set or ErrKeySet at
// random, so 64 runs that are all ErrKeySet pass by chance once in 2^64.
func TestACallerThatHangsUpEndsOnlyItsOwnWait(t *testing.T) {
	issuer := oidctest.New(t)
	source, _ := keysFor(t, issuer, "exchange-key")
	for _, order := range []struct {
		name          string
		readEndsFirst bool
		runs          int
	}{
		{"the caller leaves while its read is held before the issuer", false, 1},
		{"the read ends before the caller looks at it", true, 64},
	} {
		t.Run(order.name, func(t *testing.T) {
			for run := range order.runs {
				hungUpCaller(t, issuer, source.jwksURI, order.readEndsFirst, run, order.runs)
			}
		})
	}
}

// hungUpCaller is one caller that hung up before it asked a new Keys at jwksURI for a key no read
// has fetched: it must be ErrKeySet, and the read it started must be kept and answer the next
// caller, with no read of that caller's own. The read is held in the transport, before it reaches
// the issuer, until the test lets it go, so the test sets the order. With readEndsFirst the caller
// is held where its wait begins (its keys deadline's look-up, heldWait) until its read, let go,
// has ended; without it the caller has its answer while its read is still held, and the read is
// let go after.
func hungUpCaller(t *testing.T, issuer *oidctest.Issuer, jwksURI string, readEndsFirst bool, run, runs int) {
	t.Helper()
	keys := NewKeysAt(jwksURI)
	// The read is held for as long as the test chooses, so its own bound is the test binary's
	// deadline, not a span that a machine which stalls the test could outlast.
	keys.FetchTimeout = testwait.Remaining(t, 30*time.Second)
	release, letGo := context.WithCancel(context.Background())
	defer letGo()
	keys.HTTPClient = &http.Client{Transport: heldRead{release: release.Done()}}
	inFlight := func() *keysFetch {
		keys.mu.Lock()
		defer keys.mu.Unlock()
		return keys.inflight
	}
	reads := issuer.JWKSFetches()
	notAtTheIssuer := func(when string) *keysFetch {
		read := inFlight()
		if read == nil {
			t.Fatalf("run %d of %d: no read in flight %s, though the transport holds it", run+1, runs, when)
		}
		if fetches := issuer.JWKSFetches() - reads; fetches != 0 {
			t.Fatalf("run %d of %d: the issuer had %d key-set reads %s, though the transport holds the read", run+1, runs, fetches, when)
		}
		return read
	}
	hungUp, cancel := context.WithCancel(context.Background())
	cancel()
	var caller context.Context = hungUp
	held := false
	if readEndsFirst {
		caller = &heldWait{Context: hungUp, hold: func() {
			read := notAtTheIssuer("where the hung-up caller's wait begins")
			letGo()
			testwait.Receive(t, "the end of the read the hung-up caller started", read.done)
			held = true
		}}
	}
	if _, err := keys.Key(caller, "exchange-key", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("Key for a caller that hung up before any set was read: err = %v, want ErrKeySet (run %d of %d)", err, run+1, runs)
	}
	if readEndsFirst && !held {
		t.Fatalf("run %d of %d: the hung-up caller's wait never looked its keys deadline up, so its read did not end first: the order was not held", run+1, runs)
	}
	if !readEndsFirst {
		read := notAtTheIssuer("once the hung-up caller had its answer")
		letGo()
		testwait.Receive(t, "the end of the read the hung-up caller started", read.done)
	}
	if _, fetched := keys.state(); fetched.IsZero() {
		t.Fatalf("run %d of %d: the hung-up caller's read was not kept: no set has been read", run+1, runs)
	}
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("run %d of %d: Key after the hung-up caller's read landed: %v, want the key from that read", run+1, runs, err)
	}
	if fetches := issuer.JWKSFetches() - reads; fetches != 1 {
		t.Fatalf("run %d of %d: key set read %d times, want 1: the hung-up caller's read, kept for the next", run+1, runs, fetches)
	}
}

// A wait that has ended before it begins is ErrKeySet even where the read it waits on has ended
// too, for both ways a wait ends: a caller that hung up, and a live caller whose keys deadline has
// passed. fetch and settleMiss both wait through wait. Both of wait's cases are ready from the
// start here, so each runs 64 times: where the select alone decides, each run is the read's set
// at random, and 64 runs that are all ErrKeySet pass by chance once in 2^64.
func TestAWaitThatEndedBeforeItBeganIsErrKeySetThoughItsReadHasEnded(t *testing.T) {
	hungUp, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ended := range []struct {
		name   string
		caller context.Context
		cause  error
	}{
		{"a caller that hung up", hungUp, context.Canceled},
		{"a live caller whose keys deadline has passed", WithKeysDeadline(context.Background(), time.Now().Add(-time.Second)), context.DeadlineExceeded},
	} {
		t.Run(ended.name, func(t *testing.T) {
			keys := NewKeysAt("https://keys.example/keys")
			for run := range 64 {
				read := &keysFetch{done: make(chan struct{}), keys: []jose.JSONWebKey{{KeyID: "exchange-key"}}}
				close(read.done)
				set, err := keys.wait(ended.caller, read)
				if set != nil || !errors.Is(err, ErrKeySet) || !errors.Is(err, ended.cause) {
					t.Fatalf("wait for %s, its read ended: %d keys, err = %v, want none and ErrKeySet wrapping %v (run %d of 64)", ended.name, len(set), err, ended.cause, run+1)
				}
			}
		})
	}
}

// heldRead holds each key-set read in the transport, before it reaches the issuer, until release
// is closed or the read's own context ends, and then sends it on.
type heldRead struct {
	release <-chan struct{}
}

func (h heldRead) RoundTrip(r *http.Request) (*http.Response, error) {
	select {
	case <-h.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// heldWait is a caller's context whose first look-up of the keys deadline runs hold. wait looks
// the keys deadline up before it looks at the read or at the caller's end, so hold runs where the
// caller's wait begins, after the read the caller joined or started is under way.
type heldWait struct {
	context.Context
	once sync.Once
	hold func()
}

func (c *heldWait) Value(key any) any {
	if _, deadline := key.(keysDeadlineKey); deadline {
		c.once.Do(c.hold)
	}
	return c.Context.Value(key)
}

// Callers that hang up while a key set accepts the connection and never answers start no extra
// reads: every read runs to its own FetchTimeout, its failure starts FailureSpacing, and a flood
// of such callers costs the issuer at most one read per FailureSpacing.
func TestCallersThatHangUpCostNoMoreThanOneReadPerSpacing(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = time.Second
	keys.FetchTimeout = 500 * time.Millisecond
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	issuer.DelayKeys(4 * time.Second)

	const flood = 3 * time.Second
	stop := time.Now().Add(flood)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				_, _ = keys.Key(ctx, "never-published", testAlgorithm)
				cancel()
			}
		}()
	}
	wg.Wait()
	if reads, most := issuer.JWKSFetches()-1, 1+int(flood/keys.FailureSpacing); reads > most {
		t.Fatalf("callers hanging up at 20 ms for %s made %d key-set reads, want at most %d: one per FailureSpacing", flood, reads, most)
	}
}

// While a background refresh is under way, the callers a stale set answers start no other: 200
// callers at once against a key set 3 s slow cost one refresh.
func TestAStaleSetIsRefreshedOnceForManyCallers(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	issuer.DelayKeys(3 * time.Second)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
				t.Errorf("a caller on the stale set: %v, want the cached key", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	time.Sleep(200 * time.Millisecond)
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times for 200 callers on a stale set, want 2: the first read and one refresh", fetches)
	}
}

// A caller that has hung up, on a set stale by less than MinRefresh that holds its key, is
// answered from that set and starts no background refresh: nobody waits on it, so the next live
// caller starts the refresh instead.
func TestAHungUpCallerOnAStaleSetStartsNoRefresh(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	hungUp, cancel := context.WithCancel(context.Background())
	cancel()
	if key, err := keys.Key(hungUp, "exchange-key", testAlgorithm); err != nil || key == nil {
		t.Fatalf("Key for a hung-up caller on a stale set that holds the key: %v, want the cached key", err)
	}
	time.Sleep(200 * time.Millisecond)
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times, want 1: a hung-up caller starts no background refresh", fetches)
	}
}

// A refresh asked for once the set is fresh again, because another caller's refresh landed in the
// meantime, starts no read.
func TestARefreshOfAFreshSetReadsNothing(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.refreshInBackground(context.Background())
	time.Sleep(200 * time.Millisecond)
	if fetches := issuer.JWKSFetches(); fetches != 1 {
		t.Fatalf("key set read %d times, want 1: the set was fresh, so no refresh", fetches)
	}
}

// A miss read that failed is followed early by another only while no good read has come since:
// once a read has succeeded, a key id it lacks waits out MinRefresh from the miss, as any settled
// miss does, and makes no read of its own.
func TestAMissWindowDoesNotReopenAfterAGoodRead(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "key-a")
	keys.FailureSpacing = 50 * time.Millisecond
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key): %v", err)
	}
	issuer.FailKeys(http.StatusInternalServerError)
	if _, err := keys.Key(context.Background(), "never-published-1", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("a key id whose miss read fails: err = %v, want ErrKeySet", err)
	}
	issuer.FailKeys(0)
	time.Sleep(keys.FailureSpacing + 50*time.Millisecond)
	// A good read that no miss prompted: the stale set's background refresh.
	keys.Lifetime = time.Millisecond
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key) on the stale set: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for issuer.JWKSFetches() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the background refresh never reached the issuer within 5 s (reads %d)", issuer.JWKSFetches())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	keys.Lifetime = time.Hour
	if _, err := keys.Key(context.Background(), "never-published-2", testAlgorithm); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a key id the good refresh does not hold, within MinRefresh of the failed miss: err = %v, want ErrNoKey", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 3 {
		t.Fatalf("key set read %d times, want 3: no new miss read after the good refresh", fetches)
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

// Once a set has been read and has gone stale, a refresh that fails is not tried again for
// FailureSpacing: every caller in that time is answered from the cached set and starts no read, so
// a key set that is down costs the issuer one read per FailureSpacing however many tokens arrive.
// Once the spacing has passed, the next caller's refresh reads again, and a set that answers is
// kept.
func TestAStaleSetWhoseRefreshFailedIsReadOncePerSpacing(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = 2 * time.Second
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	keys.Lifetime = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	issuer.FailKeys(http.StatusInternalServerError)
	waitForReads := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for issuer.JWKSFetches() < want {
			if time.Now().After(deadline) {
				t.Fatalf("key set read %d times within 3 s, want %d", issuer.JWKSFetches(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("the first caller with the set stale: %v, want the cached key", err)
	}
	waitForReads(2)
	deadline := time.Now().Add(3 * time.Second)
	for {
		keys.mu.Lock()
		failed := !keys.failedAt.IsZero()
		keys.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failing background refresh was never recorded within 3 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	const callers = 200
	for i := range callers {
		if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
			t.Fatalf("caller %d while the stale set's refresh fails: %v, want the cached key", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times for %d callers within FailureSpacing of a failed refresh, want 2: the first read and one refresh", fetches, callers)
	}

	issuer.FailKeys(0)
	time.Sleep(keys.FailureSpacing + 100*time.Millisecond)
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key once the spacing has passed and the key set answers: %v", err)
	}
	waitForReads(3)
}

// During a key rotation whose read failed, a token naming the new key id is ErrKeySet only until
// FailureSpacing has passed since that failure: the next such token reads again, without waiting
// out MinRefresh, and is answered from the set it reads. A key set that keeps failing costs one
// read per FailureSpacing, no more.
func TestARotationMissReadsAgainOnceTheSpacingHasPassed(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "key-a")
	keys.FailureSpacing = 300 * time.Millisecond
	if _, err := keys.Key(context.Background(), "key-a", testAlgorithm); err != nil {
		t.Fatalf("Key(the cached key): %v", err)
	}
	issuer.PublishKey(t, "key-b")
	issuer.FailKeys(http.StatusInternalServerError)
	if _, err := keys.Key(context.Background(), "key-b", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("the first token naming key-b, whose read fails: err = %v, want ErrKeySet", err)
	}
	for i := range 50 {
		if _, err := keys.Key(context.Background(), "key-b", testAlgorithm); !errors.Is(err, ErrKeySet) {
			t.Fatalf("token %d naming key-b within the spacing: err = %v, want ErrKeySet", i, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times within FailureSpacing of the failed rotation read, want 2", fetches)
	}

	issuer.FailKeys(0)
	time.Sleep(keys.FailureSpacing + 100*time.Millisecond)
	if _, err := keys.Key(context.Background(), "key-b", testAlgorithm); err != nil {
		t.Fatalf("a token naming key-b once the spacing has passed and the key set answers: %v, want key-b well within MinRefresh (%s)", err, keys.MinRefresh)
	}
	if fetches := issuer.JWKSFetches(); fetches != 3 {
		t.Fatalf("key set read %d times, want 3: one more read once the spacing passed", fetches)
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
