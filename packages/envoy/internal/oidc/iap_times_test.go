package oidc

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// Verify reads an assertion's exp and iat itself, after go-oidc has checked its signature, issuer
// and audience. An assertion IAP signed with no exp is malformed, and one issued further ahead than
// the skew is not yet valid, whether or not it is also past its exp; neither is ever called
// expired. The same assertions signed by a key IAP never had are signature failures, since the
// signature is judged before any time is. Each result is the one the two-verifier code gave.
func TestIAPVerifierJudgesExpAndIatOnlyOnceTheSignatureHolds(t *testing.T) {
	rig := newIAPRig(t)
	forger := oidctest.NewES256Key(t, rig.signing.KeyID())
	ahead := func(c map[string]any) { c["iat"] = time.Now().Add(IAPClockSkew + time.Minute).Unix() }
	noExp := func(c map[string]any) { delete(c, "exp") }
	both := func(changes ...func(map[string]any)) func(map[string]any) {
		return func(c map[string]any) {
			for _, change := range changes {
				change(c)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		signed error
	}{
		{"no exp", noExp, ErrMalformed},
		{"no exp, iat ahead past the skew", both(noExp, ahead), ErrMalformed},
		{"iat ahead past the skew", ahead, ErrNotYetValid},
		{"expired, iat ahead past the skew", expiredClaims(ahead), ErrNotYetValid},
	} {
		for _, key := range []struct {
			name string
			key  *oidctest.Key
			want error
		}{{"IAP's key", rig.signing, tc.signed}, {"another key under IAP's key id", forger, ErrSignature}} {
			claims, err := rig.verify(t, key.key, tc.change)
			var expired *IAPExpiredError
			if !errors.Is(err, key.want) || errors.Is(err, ErrExpired) || errors.As(err, &expired) {
				t.Errorf("%s, signed by %s: err = %v, want %v and not %v", tc.name, key.name, err, key.want, ErrExpired)
			}
			if claims.Subject != "" || claims.Email != "" {
				t.Errorf("%s, signed by %s: Verify returned claims %+v beside its refusal", tc.name, key.name, claims)
			}
		}
	}
}

// IAP writes no nbf, and go-oidc checks one only beside the exp check this verifier skips, so
// Verify holds an nbf, where one is written, to the skew it holds an iat to: one further ahead is
// not yet valid, and one within the skew or past verifies.
func TestIAPVerifierHoldsAnNbfToTheSkew(t *testing.T) {
	rig := newIAPRig(t)
	if _, err := rig.verify(t, rig.signing, func(c map[string]any) {
		c["nbf"] = time.Now().Add(IAPClockSkew + 10*time.Second).Unix()
	}); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("nbf ahead past the skew: err = %v, want %v", err, ErrNotYetValid)
	}
	for name, nbf := range map[string]time.Time{
		"nbf ahead within the skew": time.Now().Add(IAPClockSkew - 10*time.Second),
		"nbf past":                  time.Now().Add(-time.Minute),
	} {
		if _, err := rig.verify(t, rig.signing, func(c map[string]any) { c["nbf"] = nbf.Unix() }); err != nil {
			t.Errorf("%s: %v, want it verified", name, err)
		}
	}
}

// A time claim IAP signed that is not a time from 1970 to 9999 is malformed, never read: go-oidc
// turns a number past int64's range into an int64 with no check, so an iat or nbf of 1e19 read as
// far in the past and verified, and an exp of 1e19 read as expired. Each is ErrMalformed, never
// verified and never expired; the controls, the same claims at the range's two ends, are read as
// times.
func TestIAPVerifierRefusesATimeOutOfRangeAsMalformed(t *testing.T) {
	rig := newIAPRig(t)
	for _, tc := range []struct {
		claim string
		value any
	}{
		{"iat", 1e19}, {"iat", -1e19}, {"iat", -1}, {"iat", 1e300}, {"iat", 253402300800},
		{"nbf", 1e19}, {"nbf", -1e19}, {"nbf", 1e300},
		{"exp", 1e19}, {"exp", -1e19}, {"exp", -1}, {"exp", 1e300},
	} {
		claims, err := rig.verify(t, rig.signing, func(c map[string]any) { c[tc.claim] = tc.value })
		var expired *IAPExpiredError
		if !errors.Is(err, ErrMalformed) || errors.Is(err, ErrExpired) || errors.As(err, &expired) {
			t.Errorf("%s %v: claims = %+v, err = %v; want %v", tc.claim, tc.value, claims, err, ErrMalformed)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   error
	}{
		{"iat and nbf at 0", func(c map[string]any) { c["iat"], c["nbf"] = 0, 0 }, nil},
		{"exp at the range's end", func(c map[string]any) { c["exp"] = 253402300799 }, nil},
		{"exp at 0", func(c map[string]any) { c["exp"] = 0 }, ErrExpired},
	} {
		if _, err := rig.verify(t, rig.signing, tc.change); !errors.Is(err, tc.want) {
			t.Errorf("control, %s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}
