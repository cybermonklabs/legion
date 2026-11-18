package oidc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// expiredClaims is iapClaims with its exp past the skew, then change applied.
func expiredClaims(change func(map[string]any)) func(map[string]any) {
	return func(c map[string]any) {
		c["iat"] = time.Now().Add(-20 * time.Minute).Unix()
		c["exp"] = time.Now().Add(-IAPClockSkew - time.Minute).Unix()
		if change != nil {
			change(c)
		}
	}
}

// An assertion is expired only when IAP signed it and nothing else is wrong with it. go-oidc
// reports an expired payload whatever its signature, and classify ranks a payload's exp ahead of
// the signature, so a forged assertion with a past exp used to come back ErrExpired, which a
// socket's heartbeat reads as a rotation. Each forged one is refused for its signature instead, an
// IAP-signed one with no exp or no iat as malformed, and the control, an assertion IAP signed that
// is only past its exp, is still ErrExpired.
func TestIAPVerifierCallsOnlyAnAssertionIAPSignedExpired(t *testing.T) {
	rig := newIAPRig(t)
	sameKid := oidctest.NewES256Key(t, rig.signing.KeyID())
	forgedKid := oidctest.NewES256Key(t, "forged-kid")
	rsaSameKid := oidctest.NewKey(t, rig.signing.KeyID())

	genuine := iapClaims()
	expiredClaims(nil)(genuine)
	parts := strings.Split(rig.server.Mint(t, rig.signing, genuine), ".")
	zeroed := parts[0] + "." + parts[1] + "." + strings.Repeat("A", len(parts[2]))

	if _, err := rig.verify(t, rig.signing, expiredClaims(nil)); !errors.Is(err, ErrExpired) {
		t.Fatalf("control: an expired assertion IAP signed: err = %v, want %v", err, ErrExpired)
	}
	for _, tc := range []struct {
		name string
		key  *oidctest.Key
		// change, applied to a fresh assertion's claims; raw, when set, is sent as it is instead.
		change func(map[string]any)
		raw    string
		want   error
	}{
		{name: "expired, another key under IAP's key id", key: sameKid, change: expiredClaims(nil), want: ErrSignature},
		{name: "expired, a key id IAP never published", key: forgedKid, change: expiredClaims(nil), want: ErrSignature},
		{name: "expired, IAP's payload with its signature zeroed", raw: zeroed, want: ErrSignature},
		{name: "expired, RS256 under IAP's key id", key: rsaSameKid, change: expiredClaims(nil), want: ErrSignature},
		{name: "past its exp but within the skew, another key under IAP's key id", key: sameKid, change: func(c map[string]any) {
			c["exp"] = time.Now().Add(-IAPClockSkew + 10*time.Second).Unix()
		}, want: ErrSignature},
		{name: "IAP-signed, no exp", key: rig.signing, change: func(c map[string]any) { delete(c, "exp") }, want: ErrMalformed},
		{name: "IAP-signed and expired, no iat", key: rig.signing, change: expiredClaims(func(c map[string]any) { delete(c, "iat") }), want: ErrMalformed},
	} {
		var err error
		if tc.raw != "" {
			_, err = rig.verifier.Verify(context.Background(), tc.raw)
		} else {
			_, err = rig.verify(t, tc.key, tc.change)
		}
		if errors.Is(err, ErrExpired) || !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v and not %v", tc.name, err, tc.want, ErrExpired)
		}
	}
}

// An expired assertion's signature is checked against IAP's key set like a fresh one's, so a key
// set that cannot be read reaches no verdict on it either: ErrIssuerUnavailable, never ErrExpired.
func TestIAPVerifierReportsAnUnreadableKeySetForAnExpiredAssertion(t *testing.T) {
	rig := newIAPRig(t)
	rig.server.FailKeys(http.StatusServiceUnavailable)
	if _, err := rig.verify(t, rig.signing, expiredClaims(nil)); errors.Is(err, ErrExpired) || !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("an expired assertion while the key set answers 503: err = %v, want %v", err, ErrIssuerUnavailable)
	}
	rig.server.FailKeys(0)
	if _, err := rig.verify(t, rig.signing, expiredClaims(nil)); !errors.Is(err, ErrExpired) {
		t.Fatalf("the same assertion once the key set answers: err = %v, want %v", err, ErrExpired)
	}
}
