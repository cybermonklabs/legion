package natscallout

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// PlatformTokens verifies a service's platform identity token for the NATS audiences, as
// Deps.ServiceToken, with the identity registry's NATS verifiers (identity.UseNATS): one per
// kind of token the broker sets a NATS audience for. The token's own issuer, read before it is
// verified, picks the verifiers whose Issuer is exactly that string, so a token is never judged
// by a verifier of another issuer; a token of an issuer no verifier holds is refused. Where two
// providers hold one issuer — a local stack whose stand-in issuer mints both a cluster's and a
// machine's tokens — each is asked in rank order until one verifies the token, and the refusal
// of a token none of them takes is the first one's own.
type PlatformTokens struct {
	Verifiers []identity.CallerVerifier
}

// Verify answers the caller raw names once a verifier of its issuer has verified it: the
// account, and when the token was issued and expires. A sound token naming no account its kind
// reads one from (*identity.NoAccountError) is refused in the verifier's brief words.
func (p PlatformTokens) Verify(ctx context.Context, raw string) (identity.Caller, error) {
	issuer, err := unverifiedIssuer(raw)
	if err != nil {
		return identity.Caller{}, err
	}
	var refusal error
	for _, verifier := range p.Verifiers {
		if verifier.Issuer() != issuer {
			continue
		}
		caller, err := verifier.VerifyCaller(ctx, raw)
		if err == nil {
			return caller, nil
		}
		var none *identity.NoAccountError
		if errors.As(err, &none) {
			err = fmt.Errorf("natscallout: %s", none.Brief)
		}
		if refusal == nil {
			refusal = err
		}
	}
	if refusal != nil {
		return identity.Caller{}, refusal
	}
	return identity.Caller{}, fmt.Errorf("natscallout: no NATS audience of this broker takes tokens of the issuer %q", issuer)
}

// unverifiedIssuer is the 'iss' raw names, read without verifying it: it only picks the verifier.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("natscallout: the service token is no JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("natscallout: the service token's claims do not decode")
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", errors.New("natscallout: the service token's claims do not decode")
	}
	return claims.Issuer, nil
}
