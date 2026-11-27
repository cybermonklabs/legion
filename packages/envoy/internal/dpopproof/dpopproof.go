// packages/envoy/internal/dpopproof/dpopproof.go

// Package dpopproof signs RFC 9449 DPoP proofs. It is the one signer that the broker's Go client
// (internal/cipherclient) and the broker's airlock proxy (internal/cipher/proxy) share, so a proof
// the proxy makes for a minted capability's request is made as a client makes its own. It sits
// outside internal/cipher because both sides call it.
package dpopproof

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

// Sign is one proof of key (typ dpop+jwt, ES256, the public key embedded) naming a fresh jti, htm,
// htu and the time (iat), and nonce and ath when they are not empty. htu is what the resource
// server checks against its own origin plus the request's path, so a caller that sends a request
// to another address than that origin signs the origin's URL, never the one it dials.
func Sign(key *ecdsa.PrivateKey, htm, htu, nonce, ath string) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key},
		(&jose.SignerOptions{EmbedJWK: true}).WithType("dpop+jwt"))
	if err != nil {
		return "", err
	}
	claims := map[string]any{"jti": uuid.NewString(), "htm": htm, "htu": htu, "iat": time.Now().Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if ath != "" {
		claims["ath"] = ath
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

// TokenHash is the ath claim naming token: the base64url-encoded SHA-256 of its value (RFC 9449
// Section 4.2).
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
