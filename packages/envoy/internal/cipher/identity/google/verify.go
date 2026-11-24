package google

import (
	"context"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/oidc"
)

// token is one verified Google identity token: the identity claims it carries (ReadIdentity, the
// one reading of Google's claim shapes) and the times its registered claims give it. IssuedAt is
// zero when the token has no iat claim.
type token struct {
	Identity Identity
	Subject  string
	Expiry   time.Time
	IssuedAt time.Time
}

// tokens verifies Google identity tokens against one OIDC verifier and reads the email and Compute
// Engine claims from the verified token.
type tokens struct{ verifier *oidc.Verifier }

// verify verifies raw's issuer, audience, signature and expiry and reads the identity claims of
// the verified token: the account, whether the address is verified, and the Compute Engine
// instance it was minted on when it carries one. It refuses nothing of its own — a token naming no
// account, an unverified address and a token minted on no instance are each a value it answers,
// since what each means is its caller's rule — so every error it returns is an OIDC verifier error
// or a malformed token. Every claim it answers is read from the payload the verified signature
// covers (oidc.Verifier.VerifyClaims), never from the token's text.
func (v tokens) verify(ctx context.Context, raw string) (token, error) {
	var registered jwt.Claims
	var private map[string]any
	claims, err := v.verifier.VerifyClaims(ctx, raw, &registered, &private)
	if err != nil {
		return token{}, err
	}
	verified := token{Identity: ReadIdentity(private), Subject: claims.Subject, Expiry: claims.Expiry}
	if registered.IssuedAt != nil {
		verified.IssuedAt = registered.IssuedAt.Time()
	}
	return verified, nil
}

// noVerifiedAccount is the refusal of a sound token that names no verified account: the email
// claim is what names a Google caller.
const noVerifiedAccount = "the Google identity token names no verified account, so it names no caller (ask for it with format=full)"

// callerVerifier is a machine's Google identity token as an API or NATS caller's: the account is
// its verified email address.
type callerVerifier struct {
	issuer string
	tokens tokens
}

// CallerVerifier verifies a caller's Google identity token for audience, on the issuer's one key
// set.
func (p *Provider) CallerVerifier(audience string) identity.CallerVerifier {
	return callerVerifier{issuer: p.config.Issuer, tokens: tokens{verifier: oidc.NewWithKeys(p.config.Issuer, audience, p.config.Keys)}}
}

func (v callerVerifier) Kind() identity.Kind { return Kind }
func (v callerVerifier) Issuer() string      { return v.issuer }

// VerifyCaller answers the token's verified email address as its account, and
// *identity.NoAccountError for a sound token naming none or an unverified one.
func (v callerVerifier) VerifyCaller(ctx context.Context, raw string) (identity.Caller, error) {
	verified, err := v.tokens.verify(ctx, raw)
	if err != nil {
		return identity.Caller{}, err
	}
	if verified.Identity.Account == "" || !verified.Identity.EmailVerified {
		return identity.Caller{}, &identity.NoAccountError{Reason: noVerifiedAccount}
	}
	return identity.Caller{
		Account: identity.Account{Kind: Kind, Address: verified.Identity.Account}, Subject: verified.Subject,
		IssuedAt: verified.IssuedAt, Expiry: verified.Expiry,
	}, nil
}
