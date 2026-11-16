package oidc

import (
	"context"
	"fmt"
)

// GoogleIssuer is the canonical issuer of a Google identity token: the token a Google machine's
// metadata server, or Cloud Run's, mints for an audience it is asked for.
const GoogleIssuer = "https://accounts.google.com"

// IssuesGoogleIdentity reports whether raw names this verifier's issuer, including Google's
// scheme-less spelling when the verifier is Google's. Verify accepts that spelling too. This
// reads no signature and grants no admission; VerifyGoogleIdentity still checks the token.
func (v *Verifier) IssuesGoogleIdentity(raw string) bool {
	claims, err := decodeClaims(raw)
	return err == nil && (claims.Issuer == v.issuer || v.issuer == GoogleIssuer && claims.Issuer == "accounts.google.com")
}

// VerifyGoogleIdentity is Verify for a Google identity token, which names its caller by an
// address rather than by its subject: it also reads Claims.Email, Claims.EmailVerified and
// Claims.HostedDomain from the verified token, as a domain-mode CodeFlow reads them
// (readWorkspaceClaims). A token minted without format=full carries no email, and the claims then
// name no account. It refuses nothing Verify does not: whether the account may call is the
// caller's rule. Every error wraps one of Verify's classes; a payload that is not a claim set is
// ErrMalformed.
func (v *Verifier) VerifyGoogleIdentity(ctx context.Context, raw string) (Claims, error) {
	claims, err := v.Verify(ctx, raw)
	if err != nil {
		return Claims{}, err
	}
	if err := readWorkspaceClaims(raw, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return claims, nil
}
