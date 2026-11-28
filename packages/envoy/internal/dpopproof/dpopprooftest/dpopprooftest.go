// Package dpopprooftest reads a DPoP proof as a broker's token endpoint reads the one a CI run's
// trade carries, for the tests of both sides that send it: internal/cipherclient's, which holds
// the client's key, and cmd/cipher's, which drives the built command and cannot name the key it
// makes. It is an ordinary package rather than a _test.go file so both suites read one copy; Go
// cannot share test-only code across packages.
package dpopprooftest

import (
	"crypto/ecdsa"
	"fmt"
	"net/http"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"authelia.com/provider/oauth2/token/jose"
)

// TradeRefusal is why proof is not a DPoP proof that a broker's token endpoint at tokenURL takes
// for a trade, or "" when it is one: RFC 9449's own parser reads it (its typ, its one signature by
// the public key it carries, and its claims), and its htm, htu and iat are those of a POST to
// tokenURL signed within the last minute. signer is the public key the proof carries and is
// signed by, nil when the proof does not parse or carries no ECDSA key; a test that knows the key
// the proof must be signed with compares it.
func TradeRefusal(proof, tokenURL string) (why string, signer *ecdsa.PublicKey) {
	parsed, err := rfc9449.ParseProof(proof, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return oauth2.ErrorToRFC6749Error(err).HintField, nil
	}
	signer, _ = parsed.JWK.Key.(*ecdsa.PublicKey)
	switch {
	case parsed.Method != http.MethodPost || parsed.URL != tokenURL:
		return fmt.Sprintf("its htm %q and htu %q are not POST and %q", parsed.Method, parsed.URL, tokenURL), signer
	case time.Since(parsed.IssuedAt) > time.Minute || time.Until(parsed.IssuedAt) > time.Second:
		return fmt.Sprintf("its iat %s is not within the last minute", parsed.IssuedAt), signer
	}
	return "", signer
}
