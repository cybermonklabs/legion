package oidc

import (
	"context"
	"fmt"
)

// GoogleIssuer is the canonical issuer of a Google identity token: the token a Google machine's
// metadata server, or Cloud Run's, mints for an audience it is asked for.
const GoogleIssuer = "https://accounts.google.com"

// googleSchemelessIssuer is the second spelling of Google's issuer, which Google documents and
// go-oidc accepts for a verifier of GoogleIssuer.
const googleSchemelessIssuer = "accounts.google.com"

// namesIssuer reports whether iss is this verifier's issuer, taking Google's scheme-less spelling
// as Google's, as the go-oidc verifier underneath does. Verify's classification and Issues both
// read it, so a token Verify would accept is never called another issuer's.
func (v *Verifier) namesIssuer(iss string) bool {
	return iss == v.issuer || v.issuer == GoogleIssuer && iss == googleSchemelessIssuer
}

// CanonicalIssuer is the one spelling of iss that a subject is recorded and compared under:
// Google's scheme-less spelling is Google's issuer, as namesIssuer takes it, and every other
// issuer is its own spelling. Dispatch names a replaced account by it and the broker records a
// sign-in's and an approval's account by it, so the two compare one spelling.
func CanonicalIssuer(iss string) string {
	if iss == googleSchemelessIssuer {
		return GoogleIssuer
	}
	return iss
}

// VerifyGoogleIdentity is Verify for a Google identity token, which names its caller by an
// address rather than by its subject: it also reads Claims.Email, Claims.EmailVerified and
// Claims.HostedDomain from the verified token's payload, the one the signature covers, as a
// domain-mode CodeFlow reads them (readWorkspaceClaims), and never from the token's text. A token
// minted without format=full carries no email, and the claims then name no account. It refuses
// nothing Verify does not: whether the account may call is the caller's rule. Every error wraps
// one of Verify's classes; a payload that is not a claim set is ErrMalformed.
func (v *Verifier) VerifyGoogleIdentity(ctx context.Context, raw string) (Claims, error) {
	token, claims, err := v.verify(ctx, raw)
	if err != nil {
		return Claims{}, err
	}
	if err := readWorkspaceClaims(token, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return claims, nil
}
