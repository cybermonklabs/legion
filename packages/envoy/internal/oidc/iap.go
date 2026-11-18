package oidc

import (
	"context"
	"errors"
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
	// ignoringExpiry is the same check on the same key set with no expiry check, which Verify asks
	// only of an assertion the first check called expired: go-oidc reports an expired payload
	// whatever its signature, and classify ranks a payload's exp ahead of the signature, so
	// ErrExpired alone does not say that IAP signed the assertion.
	ignoringExpiry *gooidc.IDTokenVerifier
}

// NewIAPVerifier returns the verifier of IAP's assertions to audience, checking each signature
// against keys: NewKeysAt(IAPKeySetURL) in a deployment, a set the test serves in a test. It reads
// nothing; the first Verify reads the set.
func NewIAPVerifier(audience string, keys *Keys) *IAPVerifier {
	// ES256 alone, the one algorithm IAP signs with, so an assertion under any other is refused
	// before a key is asked for.
	algorithms := []string{string(jose.ES256)}
	return &IAPVerifier{
		verifier: &Verifier{
			// Now runs IAPClockSkew behind this process's clock, so go-oidc's expiry check
			// takes an assertion until the skew past its exp.
			verifier: gooidc.NewVerifier(IAPIssuer, keySet{keys: keys}, &gooidc.Config{
				ClientID:             audience,
				SupportedSigningAlgs: algorithms,
				Now:                  func() time.Time { return time.Now().Add(-IAPClockSkew) },
			}),
			issuer:   IAPIssuer,
			audience: audience,
		},
		ignoringExpiry: gooidc.NewVerifier(IAPIssuer, keySet{keys: keys}, &gooidc.Config{
			ClientID:             audience,
			SupportedSigningAlgs: algorithms,
			SkipExpiryCheck:      true,
		}),
	}
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
// skew, or ErrMalformed for an assertion with no iat or no exp. An assertion past its exp is an
// *IAPExpiredError only once its signature verifies against IAP's key set and every other check
// passes (expired); one whose signature does not is ErrSignature, or ErrIssuerUnavailable when
// the key set cannot be read.
func (v *IAPVerifier) Verify(ctx context.Context, raw string) (Claims, error) {
	token, claims, err := v.verifier.verify(ctx, raw)
	if errors.Is(err, ErrExpired) {
		return Claims{}, v.expired(ctx, raw)
	}
	if err != nil {
		return Claims{}, err
	}
	return assertionClaims(token, claims)
}

// expired judges an assertion the first check called expired, by the same check with no expiry
// check. go-oidc reads the issuer and audience before the exp, so the first check has passed them
// already, and what this one adds is the algorithm and the signature against IAP's key set. An
// assertion that passes, with an exp and an iat that pass, is an *IAPExpiredError carrying its
// claims; one that does not is refused for what failed, an algorithm other than ES256 or a
// signature IAP's keys do not verify being ErrSignature (the class Verify gives a fresh assertion
// signed so), a key set that could not be read ErrIssuerUnavailable, and an ended context
// ErrCancelled, each decided here and not by classify, which would call the assertion expired
// again.
func (v *IAPVerifier) expired(ctx context.Context, raw string) error {
	keyCtx, keySetErr := withKeySetError(ctx)
	token, err := v.ignoringExpiry.Verify(keyCtx, stripSpaces(raw))
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("%w: %v", ErrCancelled, ctx.Err())
		case errors.Is(keySetErr.err, ErrKeySet):
			return fmt.Errorf("%w: %w", ErrIssuerUnavailable, keySetErr.err)
		default:
			return fmt.Errorf("%w: the assertion is past its exp, and IAP's keys do not verify it", ErrSignature)
		}
	}
	if token.Expiry.IsZero() {
		return fmt.Errorf("%w: the assertion carries no exp", ErrMalformed)
	}
	claims, err := assertionClaims(token, Claims{Subject: token.Subject, Issuer: token.Issuer, Audience: token.Audience, Expiry: token.Expiry})
	if err != nil {
		return err
	}
	return &IAPExpiredError{Claims: claims}
}

// assertionClaims holds a verified assertion to its iat and reads the account it names from the
// payload the signature covers.
func assertionClaims(token *gooidc.IDToken, claims Claims) (Claims, error) {
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
