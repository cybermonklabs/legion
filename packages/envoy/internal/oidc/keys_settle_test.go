package oidc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestATokenThatFindsTheMissWindowShutJoinsTheReadThatOpenedIt: token A names a key the cached set
// does not hold, opens the miss window and starts the read it allows; token B for the same key id
// arrives the moment after, finds the window shut, and joins A's read, so it is answered the new
// key and never ErrNoKey. The window opens and the read starts under one lock (missRead), which
// leaves B no moment in which the window is shut with no read behind it.
func TestATokenThatFindsTheMissWindowShutJoinsTheReadThatOpenedIt(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "old-key")
	if _, err := keys.Key(context.Background(), "old-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	issuer.PublishKey(t, "new-key")
	var fired atomic.Bool
	var second error
	keys.missOpened = func() {
		if fired.CompareAndSwap(false, true) {
			_, second = keys.Key(context.Background(), "new-key", testAlgorithm)
		}
	}
	if _, err := keys.Key(context.Background(), "new-key", testAlgorithm); err != nil {
		t.Fatalf("token A, which opened the miss window = %v, want the new key", err)
	}
	if !fired.Load() {
		t.Fatal("token A opened no miss window, so token B never ran")
	}
	if second != nil {
		t.Fatalf("token B, which found the miss window shut the moment after A opened it = %v, want the new key: it joins A's read", second)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: the first read and A's, which B joined", fetches)
	}
}

// TestATokenWhoseSetAReadReplacedIsAnsweredFromThatRead: token B looks at the cached set, which
// does not hold the new key; before it goes on, token A for the same key id opens the miss window
// and its read finds the key. B then finds the window shut and no read under way, and is answered
// the key from the set A's read found, not ErrNoKey from the set it looked at (settleMiss).
func TestATokenWhoseSetAReadReplacedIsAnsweredFromThatRead(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "old-key")
	if _, err := keys.Key(context.Background(), "old-key", testAlgorithm); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	issuer.PublishKey(t, "new-key")
	var fired atomic.Bool
	var first error
	keys.afterState = func() {
		if fired.CompareAndSwap(false, true) {
			_, first = keys.Key(context.Background(), "new-key", testAlgorithm)
		}
	}
	_, err := keys.Key(context.Background(), "new-key", testAlgorithm)
	if first != nil {
		t.Fatalf("token A, whose read found the new key = %v, want the new key", first)
	}
	if err != nil {
		t.Fatalf("token B, which looked at the set before A's read replaced it = %v, want the new key: the set A's read found answers it", err)
	}
	if fetches := issuer.JWKSFetches(); fetches != 2 {
		t.Fatalf("key set read %d times, want 2: the first read and A's", fetches)
	}
}

// TestKeysStartWithTheDocumentedTimings: a key source from NewKeysAt and one from NewKeys start
// with a Lifetime of 15 minutes, a MinRefresh of 1 minute, a FailureSpacing of 5 seconds and a
// FetchTimeout of 10 seconds, the numbers the broker's operator documentation names.
func TestKeysStartWithTheDocumentedTimings(t *testing.T) {
	discovered, _ := keysFor(t, oidctest.New(t), "signing-key")
	for name, keys := range map[string]*Keys{"NewKeysAt": NewKeysAt("https://issuer.example/keys"), "NewKeys": discovered} {
		for _, timing := range []struct {
			field     string
			got, want time.Duration
		}{
			{"Lifetime", keys.Lifetime, 15 * time.Minute},
			{"MinRefresh", keys.MinRefresh, time.Minute},
			{"FailureSpacing", keys.FailureSpacing, 5 * time.Second},
			{"FetchTimeout", keys.FetchTimeout, 10 * time.Second},
		} {
			if timing.got != timing.want {
				t.Errorf("%s: %s = %v, want %v", name, timing.field, timing.got, timing.want)
			}
		}
	}
}

// TestOnChangeIsToldEachChangeOnce: OnChange is told of a read only where it finds the key set other
// than the read before it: nothing for a first read that succeeds; one call, with an ErrKeySet
// naming the key set, for three failed reads in a row, while the cached set answers the key each
// time; one call with nil for the two good reads after them; and one more for a failed read after
// that. A first read that fails is a change too.
func TestOnChangeIsToldEachChangeOnce(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "signing-key")
	// Every Key reads the set: no lifetime, no spacing of a miss and none of a failure.
	keys.Lifetime, keys.MinRefresh, keys.FailureSpacing = 0, 0, 0
	var mu sync.Mutex
	var changes []error
	keys.OnChange = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		changes = append(changes, err)
	}
	read := func() {
		t.Helper()
		if _, err := keys.Key(context.Background(), "signing-key", testAlgorithm); err != nil {
			t.Fatalf("Key of the key the set holds = %v, want the key", err)
		}
	}
	read()
	issuer.FailKeys(http.StatusServiceUnavailable)
	read()
	read()
	read()
	issuer.FailKeys(0)
	read()
	read()
	issuer.FailKeys(http.StatusServiceUnavailable)
	read()
	mu.Lock()
	got := append([]error(nil), changes...)
	mu.Unlock()
	if len(got) != 3 || got[1] != nil {
		t.Fatalf("OnChange was told %v, want three changes: a failure, nil, a failure", got)
	}
	for _, failure := range []error{got[0], got[2]} {
		if !errors.Is(failure, ErrKeySet) || !strings.Contains(failure.Error(), keys.URL()) {
			t.Fatalf("OnChange was told the failure %v, want an ErrKeySet naming %s", failure, keys.URL())
		}
	}

	failing := oidctest.New(t)
	unread, _ := keysFor(t, failing, "signing-key")
	failing.FailKeys(http.StatusServiceUnavailable)
	var told atomic.Int32
	unread.OnChange = func(err error) {
		if err != nil {
			told.Add(1)
		}
	}
	if _, err := unread.Key(context.Background(), "signing-key", testAlgorithm); !errors.Is(err, ErrKeySet) {
		t.Fatalf("Key on a key set whose first read fails = %v, want ErrKeySet", err)
	}
	if got := told.Load(); got != 1 {
		t.Fatalf("a first read that failed told OnChange %d failures, want one", got)
	}
}
