package oidc

import (
	"context"
	"fmt"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// IAPIssuer is the issuer of the assertion Identity-Aware Proxy signs for each request it lets
// through to an app, in the request's X-Goog-IAP-JWT-Assertion header.
const IAPIssuer = "https://cloud.google.com/iap"

// IAPKeySetURL is where Google publishes the keys IAP signs its assertions with, as a JWK set. IAP
// publishes no discovery document, so the set is read from this fixed address (NewKeysAt), the one
// Google documents (https://cloud.google.com/iap/docs/signed-headers-howto).
const IAPKeySetURL = "https://www.gstatic.com/iap/verify/public_key-jwk"

// IAPClockSkew is how far IAP's clock and this process's may disagree, the 30 seconds Google
// documents for an assertion's exp and iat alike: an assertion is taken until this long after its
// exp, and from this long before its iat (or its nbf, which IAP does not write).
const IAPClockSkew = 30 * time.Second

// iapGoogleAccountSubject begins the sub of an assertion IAP signs for a Google account; an
// external identity's sub begins otherwise.
const iapGoogleAccountSubject = "accounts.google.com:"

// IAPExpiredError is IAPVerifier.Verify's refusal of an assertion IAP signed, and that is sound
// in every respect but its age: its ES256 signature verifies against IAP's key set, it is IAP's to
// this verifier's audience, its iat is present and not ahead, and its exp is past. It wraps
// ErrExpired, and it carries the claims the signature covers, so a caller can tell whether the
// person they name would still be admitted; it admits nobody on them. An assertion that is expired
// and wrong in any other way is refused for that, never with this.
type IAPExpiredError struct {
	Claims Claims
}

func (e *IAPExpiredError) Error() string {
	return fmt.Sprintf("%v at %s", ErrExpired, e.Claims.Expiry.UTC().Format(time.RFC3339))
}

// Unwrap makes the error ErrExpired, its class.
func (e *IAPExpiredError) Unwrap() error { return ErrExpired }

// IAPVerifier verifies the assertions IAP signs for one audience, over one key cache.
type IAPVerifier struct {
	verifier *Verifier
}

// NewIAPVerifier returns the verifier of IAP's assertions to audience, checking each signature
// against keys: NewKeysAt(IAPKeySetURL) in a deployment, a set the test serves in a test. It reads
// nothing; the first Verify reads the set.
//
// go-oidc checks the algorithm (ES256 alone, the one IAP signs with, so an assertion under any
// other is refused before a key is asked for), the signature, the issuer and the audience, and
// skips the exp check: Verify checks the verified exp and iat itself, so an assertion is called
// expired only once everything else about it holds. skipExpiry keeps classify to that too.
func NewIAPVerifier(audience string, keys *Keys) *IAPVerifier {
	return &IAPVerifier{verifier: &Verifier{
		verifier: gooidc.NewVerifier(IAPIssuer, keySet{keys: keys}, &gooidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: []string{string(jose.ES256)},
			SkipExpiryCheck:      true,
		}),
		issuer:     IAPIssuer,
		audience:   audience,
		skipExpiry: true,
	}}
}

// Verify checks raw, an X-Goog-IAP-JWT-Assertion header's value, as Google documents
// (https://cloud.google.com/iap/docs/signed-headers-howto): signed with ES256 by a key of IAP's
// set, issued by IAPIssuer to this verifier's audience, its exp no more than IAPClockSkew past
// and its iat no more than IAPClockSkew ahead. It returns the account IAP signed in, read from
// the payload the signature covers: Subject is sub, Email is email, which IAP writes with no
// prefix, and HostedDomain is hd, the account's Workspace domain.
//
// An assertion carries no email_verified. IAP names a Google account by the address of the
// account it signed in, and Google tells an app to take that address as the person, so
// EmailVerified is true for an assertion whose sub names a Google account and false for any
// other, an external identity's.
//
// The checks run in this order, and an assertion is refused for the first that fails: the
// compact serialization, then go-oidc's algorithm, signature, issuer and audience (Verifier.verify,
// whose refusals classify names without ever calling one expired); then on the verified payload
// an exp present (ErrMalformed), an iat present (ErrMalformed) and not further ahead than the skew
// (ErrNotYetValid), an nbf, where one is written, not further ahead than the skew
// (ErrNotYetValid), and claims of the right types (ErrMalformed). Only an assertion that passes
// all of them and whose exp is more than the skew past is an *IAPExpiredError.
func (v *IAPVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	token, claims, err := v.verifier.verify(ctx, raw)
	if err != nil {
		return Claims{}, err
	}
	// go-oidc checks nbf only beside the exp it skips here, so it is read from the payload.
	var times struct {
		NotBefore *float64 `json:"nbf"`
	}
	if err := token.Claims(&times); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	now := time.Now()
	switch {
	case token.Expiry.IsZero():
		return Claims{}, fmt.Errorf("%w: the assertion carries no exp", ErrMalformed)
	case token.IssuedAt.IsZero():
		return Claims{}, fmt.Errorf("%w: the assertion carries no iat", ErrMalformed)
	case token.IssuedAt.After(now.Add(IAPClockSkew)):
		return Claims{}, fmt.Errorf("%w: issued at %s", ErrNotYetValid, token.IssuedAt.UTC().Format(time.RFC3339))
	case times.NotBefore != nil && time.Unix(int64(*times.NotBefore), 0).After(now.Add(IAPClockSkew)):
		return Claims{}, fmt.Errorf("%w: not before %s", ErrNotYetValid, time.Unix(int64(*times.NotBefore), 0).UTC().Format(time.RFC3339))
	}
	if err := readWorkspaceClaims(token, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	claims.EmailVerified = strings.HasPrefix(claims.Subject, iapGoogleAccountSubject)
	if token.Expiry.Add(IAPClockSkew).Before(now) {
		return Claims{}, &IAPExpiredError{Claims: claims}
	}
	return claims, nil
}
