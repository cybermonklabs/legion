package oidc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// iapTestAudience is the audience IAP signs for a load balancer's backend service, in the form
// Google documents.
const iapTestAudience = "/projects/123456789012/global/backendServices/9876543210"

// iapRig is a key set served on this machine in IAP's place, which no discovery document names,
// and the verifier of assertions to iapTestAudience over it, through keys.
type iapRig struct {
	server   *oidctest.Issuer
	signing  *oidctest.Key
	keys     *Keys
	verifier *IAPVerifier
}

func newIAPRig(t *testing.T) iapRig {
	t.Helper()
	server := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	server.Publish(signing)
	keys := NewKeysAt(server.URL() + "/keys")
	return iapRig{server: server, signing: signing, keys: keys, verifier: NewIAPVerifier(iapTestAudience, keys)}
}

// iapClaims is an assertion's claim set as IAP writes it for a Google account of the Workspace
// example.com: its issuer, the audience as a string, sub with its namespace, email without one, and
// hd, issued a minute ago for ten minutes.
func iapClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   IAPIssuer,
		"aud":   iapTestAudience,
		"sub":   "accounts.google.com:118000000000000000001",
		"email": "A.Person@example.com",
		"hd":    "example.com",
		"iat":   now.Add(-time.Minute).Unix(),
		"exp":   now.Add(9 * time.Minute).Unix(),
	}
}

func (r iapRig) verify(t *testing.T, key *oidctest.Key, change func(map[string]any)) (Claims, error) {
	t.Helper()
	claims := iapClaims()
	if change != nil {
		change(claims)
	}
	return r.verifier.Verify(context.Background(), r.server.Mint(t, key, claims))
}

// An assertion IAP signed for the audience verifies against the key set read from its fixed
// address, with no discovery document asked for, and names the account by sub, email and hd. Its
// email counts as verified because the sub names a Google account; an external identity's does not.
func TestIAPVerifierReadsTheAccountOfAnAssertionIAPSigned(t *testing.T) {
	rig := newIAPRig(t)
	claims, err := rig.verify(t, rig.signing, nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "accounts.google.com:118000000000000000001" || claims.Email != "A.Person@example.com" ||
		claims.HostedDomain != "example.com" || !claims.EmailVerified || claims.Issuer != IAPIssuer {
		t.Fatalf("claims = %+v, want the Google account with its address, hd and EmailVerified", claims)
	}
	if reads := rig.server.DiscoveryRequests(); reads != 0 {
		t.Errorf("the verifier read a discovery document %d times, want 0: IAP publishes none", reads)
	}
	if reads := rig.server.JWKSFetches(); reads != 1 {
		t.Errorf("the verifier read the key set %d times, want 1", reads)
	}

	external, err := rig.verify(t, rig.signing, func(c map[string]any) {
		c["sub"] = "securetoken.google.com/example-project:external-user"
	})
	if err != nil || external.EmailVerified {
		t.Errorf("an external identity's assertion: claims = %+v, err = %v; want it read with EmailVerified false", external, err)
	}
}

// Each assertion IAP would not have signed, or signed for another app, is refused with its class,
// and each refusal has a control beside it that differs in that one respect and verifies.
func TestIAPVerifierRefusesWhatIAPDidNotSignForThisAudience(t *testing.T) {
	rig := newIAPRig(t)
	// A forger's key under IAP's own key id: the key set resolves and only the signature fails.
	forged := oidctest.NewES256Key(t, rig.signing.KeyID())
	if _, err := rig.verify(t, forged, nil); !errors.Is(err, ErrSignature) {
		t.Errorf("an assertion signed by another key under IAP's key id: err = %v, want %v", err, ErrSignature)
	}
	// An RS256 key the set even publishes: IAP signs with ES256 alone.
	rsaKey := oidctest.NewKey(t, "rsa-key")
	rig.server.Publish(rsaKey)
	if _, err := rig.verify(t, rsaKey, nil); !errors.Is(err, ErrSignature) {
		t.Errorf("an RS256 assertion under a published key: err = %v, want %v", err, ErrSignature)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   error
	}{
		{"another audience", func(c map[string]any) { c["aud"] = "/projects/123456789012/global/backendServices/1" }, ErrAudience},
		{"another issuer", func(c map[string]any) { c["iss"] = "https://accounts.google.com" }, ErrIssuer},
		{"expired past the skew", func(c map[string]any) { c["exp"] = time.Now().Add(-IAPClockSkew - 10*time.Second).Unix() }, ErrExpired},
		{"issued ahead past the skew", func(c map[string]any) { c["iat"] = time.Now().Add(IAPClockSkew + 10*time.Second).Unix() }, ErrNotYetValid},
		{"no iat", func(c map[string]any) { delete(c, "iat") }, ErrMalformed},
	} {
		if _, err := rig.verify(t, rig.signing, tc.change); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// The controls: the skew Google documents is allowed on either side.
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"expired within the skew", func(c map[string]any) { c["exp"] = time.Now().Add(-IAPClockSkew + 10*time.Second).Unix() }},
		{"issued ahead within the skew", func(c map[string]any) { c["iat"] = time.Now().Add(IAPClockSkew - 10*time.Second).Unix() }},
		{"the audience as a one-element array", func(c map[string]any) { c["aud"] = []string{iapTestAudience} }},
	} {
		if _, err := rig.verify(t, rig.signing, tc.change); err != nil {
			t.Errorf("%s: %v, want it verified", tc.name, err)
		}
	}
}

// IAP's key set that cannot be read reaches no verdict: the assertion is refused as the issuer
// being unavailable, never as a forgery.
func TestIAPVerifierReportsAnUnreadableKeySetAsUnavailable(t *testing.T) {
	rig := newIAPRig(t)
	// The read once the key set answers again is due at once: this is the verdict after a
	// recovery, not the spacing between failed reads (TestAKeySetThatFailsAtOnceIsReadOncePerSpacing).
	rig.keys.FailureSpacing = 0
	rig.server.FailKeys(503)
	if _, err := rig.verify(t, rig.signing, nil); !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("an assertion while the key set answers 503: err = %v, want %v", err, ErrIssuerUnavailable)
	}
	rig.server.FailKeys(0)
	if _, err := rig.verify(t, rig.signing, nil); err != nil {
		t.Fatalf("the same assertion once the key set answers: %v", err)
	}
}

// An expired assertion IAP signed is refused with an *IAPExpiredError that carries the account the
// signature covers, so its caller can tell whether that person would still be admitted; it is
// ErrExpired, and Reason names it so. The domain is the caller's rule, so an account of another
// Workspace is carried as it is.
func TestIAPVerifierCarriesTheAccountOfAnExpiredAssertionIAPSigned(t *testing.T) {
	rig := newIAPRig(t)
	for _, hd := range []string{"example.com", "other.example"} {
		exp := time.Now().Add(-IAPClockSkew - time.Minute).Truncate(time.Second)
		claims, err := rig.verify(t, rig.signing, func(c map[string]any) {
			c["hd"], c["exp"] = hd, exp.Unix()
		})
		var expired *IAPExpiredError
		if !errors.As(err, &expired) || !errors.Is(err, ErrExpired) || Reason(err) != "expired" {
			t.Fatalf("hd %s: claims = %+v, err = %v; want an *IAPExpiredError, which is %v", hd, claims, err, ErrExpired)
		}
		got := expired.Claims
		if got.Subject != "accounts.google.com:118000000000000000001" || got.Email != "A.Person@example.com" || got.HostedDomain != hd ||
			!got.EmailVerified || got.Issuer != IAPIssuer || !got.Expiry.Equal(exp) {
			t.Errorf("hd %s: the expired assertion's claims = %+v, want its account, hd, issuer and exp", hd, got)
		}
		if !reflect.DeepEqual(claims, Claims{}) {
			t.Errorf("hd %s: Verify returned claims %+v beside its refusal, want none", hd, claims)
		}
	}
}
