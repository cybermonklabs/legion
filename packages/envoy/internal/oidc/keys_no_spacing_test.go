package oidc

// keys_no_spacing_test.go holds a Keys with no FailureSpacing, as an IssuerSet made with no spacing
// has, to a read of the key set at every Key: of a key set that cannot be read, of a set held past
// Lifetime + MinRefresh whose key set then fails, and for a key id the set does not hold while its
// key set fails.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// spacedCalls is how many Key calls a spacing test makes, the fifty tokens the broker's spacing is
// measured against.
const spacedCalls = 50

// A key set that cannot be read, read by a Keys whose FailureSpacing is zero, is read at every
// Key, each answering ErrKeySet: zero spaces nothing. The spacing every Keys starts with is
// TestAKeySetThatFailsAtOnceIsReadOncePerSpacing's.
func TestAKeySetWithNoSpacingIsReadAtEveryKey(t *testing.T) {
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = 0
	issuer.FailKeys(http.StatusServiceUnavailable)
	for call := 1; call <= spacedCalls; call++ {
		if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); !errors.Is(err, ErrKeySet) {
			t.Fatalf("Key, call %d, against a key set answering 503 = %v, want ErrKeySet", call, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != spacedCalls {
		t.Fatalf("%d Key calls read the failing key set %d times, want %d", spacedCalls, fetches, spacedCalls)
	}
}

// TestAStaleKeySetWithNoSpacingIsReadAtEveryKey: a Keys with no FailureSpacing, as an IssuerSet
// made with no spacing has, whose set is past Lifetime + MinRefresh and whose issuer's key set then
// fails, goes on answering the key it holds, and every Key call reads the key set first. With the
// spacing every Keys starts with, the first read's failure answers the later calls instead
// (TestAnOldSetWhoseReadFailsStillAnswersTheKeysItHolds).
func TestAStaleKeySetWithNoSpacingIsReadAtEveryKey(t *testing.T) {
	issuer := oidctest.New(t)
	keys, signing := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = 0
	// Every Key finds the set past Lifetime + MinRefresh, so it reads the key set first and the
	// cached set is only ever the fallback.
	keys.Lifetime, keys.MinRefresh = 0, 0
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	token := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:example-ns:example-worker", "broker"))
	issuer.FailKeys(http.StatusServiceUnavailable)
	for call := 1; call <= spacedCalls; call++ {
		key, err := keys.Key(context.Background(), "exchange-key", testAlgorithm)
		if err != nil || !signs(t, token, key) {
			t.Fatalf("Key, call %d, with the key set failing = %v, %v; want the cached key", call, key, err)
		}
	}
	if fetches, want := issuer.JWKSFetches(), 1+spacedCalls; fetches != want {
		t.Fatalf("the key set was read %d times, want %d", fetches, want)
	}
}

// TestAKeyRotatedInWhileTheKeySetCannotBeReadIsTheOutage: a set read once, whose issuer's key set
// then fails, is asked for a key id it does not hold three times in one MinRefresh window, the
// way a cluster that rotated its key while its key set is down sends tokens. A Keys with no
// FailureSpacing, as an IssuerSet made with no spacing has, holds no failed read over to a later
// call: a key id whose read failed is looked for again once FailureSpacing has passed, so each
// call reads the key set itself and answers that read's ErrKeySet, and no token is refused as
// signed by a key nobody published. One with the spacing every Keys starts with answers the later
// calls from the failed read instead (TestANewKeyIDWhoseReadFailedIsNoForgery).
func TestAKeyRotatedInWhileTheKeySetCannotBeReadIsTheOutage(t *testing.T) {
	const calls = 3
	issuer := oidctest.New(t)
	keys, _ := keysFor(t, issuer, "exchange-key")
	keys.FailureSpacing = 0
	if _, err := keys.Key(context.Background(), "exchange-key", testAlgorithm); err != nil {
		t.Fatalf("Key(the published key): %v", err)
	}
	issuer.FailKeys(http.StatusServiceUnavailable)
	for call := 1; call <= calls; call++ {
		if _, err := keys.Key(context.Background(), "rotated-in", testAlgorithm); !errors.Is(err, ErrKeySet) || errors.Is(err, ErrNoKey) {
			t.Fatalf("Key(a key id the set does not hold), call %d, with the key set failing = %v, want ErrKeySet and not ErrNoKey", call, err)
		}
	}
	if fetches := issuer.JWKSFetches(); fetches != 1+calls {
		t.Fatalf("the key set was read %d times, want %d: the first Key and one read for each call naming the unknown key id", fetches, 1+calls)
	}
}
