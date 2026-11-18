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
// exp, and from this long before its iat.
const IAPClockSkew = 30 * time.Second

// iapGoogleAccountSubject begins the sub of an assertion IAP signs for a Google account; an
// external identity's sub begins otherwise.
const iapGoogleAccountSubject = "accounts.google.com:"

// IAPVerifier verifies the assertions IAP signs for one audience, over one key cache.
type IAPVerifier struct {
	verifier *Verifier
}

// NewIAPVerifier returns the verifier of IAP's assertions to audience, checking each signature
// against keys: NewKeysAt(IAPKeySetURL) in a deployment, a set the test serves in a test. It reads
// nothing; the first Verify reads the set.
func NewIAPVerifier(audience string, keys *Keys) *IAPVerifier {
	return &IAPVerifier{verifier: &Verifier{
		// ES256 alone, the one algorithm IAP signs with, so an assertion under any other is
		// refused before a key is asked for. Now runs IAPClockSkew behind this process's clock,
		// so go-oidc's expiry check takes an assertion until the skew past its exp.
		verifier: gooidc.NewVerifier(IAPIssuer, keySet{keys: keys}, &gooidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: []string{string(jose.ES256)},
			Now:                  func() time.Time { return time.Now().Add(-IAPClockSkew) },
		}),
		issuer:   IAPIssuer,
		audience: audience,
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
// Every error wraps one of Verify's classes, ErrNotYetValid for an iat further ahead than the
// skew, or ErrMalformed for an assertion with no iat.
func (v *IAPVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	token, claims, err := v.verifier.verify(ctx, raw)
	if err != nil {
		return Claims{}, err
	}
	switch {
	case token.IssuedAt.IsZero():
		return Claims{}, fmt.Errorf("%w: the assertion carries no iat", ErrMalformed)
	case token.IssuedAt.After(time.Now().Add(IAPClockSkew)):
		return Claims{}, fmt.Errorf("%w: issued at %s", ErrNotYetValid, token.IssuedAt.UTC().Format(time.RFC3339))
	}
	if err := readWorkspaceClaims(token, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	claims.EmailVerified = strings.HasPrefix(claims.Subject, iapGoogleAccountSubject)
	return claims, nil
}
