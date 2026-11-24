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
// verified, picks the verifier whose Issuer is exactly that string, so a token is verified once
// and never by a verifier of another kind; a token of an issuer no verifier holds is refused.
type PlatformTokens struct {
	Verifiers []identity.CallerVerifier
}

// Verify answers the caller raw names once the verifier of its issuer has verified it: the
// account, and when the token was issued and expires. A sound token naming no account its kind
// reads one from (*identity.NoAccountError) is refused in the verifier's brief words.
func (p PlatformTokens) Verify(ctx context.Context, raw string) (identity.Caller, error) {
	issuer, err := unverifiedIssuer(raw)
	if err != nil {
		return identity.Caller{}, err
	}
	for _, verifier := range p.Verifiers {
		if verifier.Issuer() != issuer {
			continue
		}
		caller, err := verifier.VerifyCaller(ctx, raw)
		var none *identity.NoAccountError
		if errors.As(err, &none) {
			return identity.Caller{}, fmt.Errorf("natscallout: %s", none.Brief)
		}
		if err != nil {
			return identity.Caller{}, err
		}
		return caller, nil
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
