package brokertoken

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"authelia.com/provider/oauth2/token/jose"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// ConnectionProofMethod and ConnectionProofTarget are the 'htm' and 'htu' a NATS connection's
// proof names (oauthwire), carried here so a reader of this package's connection check reads one
// package.
const (
	ConnectionProofMethod = oauthwire.ConnectionProofMethod
	ConnectionProofTarget = oauthwire.ConnectionProofTarget
)

// ProofClaimError is a connection proof refused for one of its claims, which Claim names: the
// header's 'typ' or 'alg', or the payload's 'jti', 'htm', 'htu', 'iat', 'ath' or 'nonce', and
// 'jws' or 'signature' for a proof that is no signed JWS of its embedded key at all. It is
// ErrProofInvalid, and carries nothing of the token or the proof.
type ProofClaimError struct {
	Claim string
}

func (e *ProofClaimError) Error() string { return ErrProofInvalid.Error() + ": " + e.Claim }

func (e *ProofClaimError) Unwrap() error { return ErrProofInvalid }

// VerifyConnectionProof checks proof, a compact JWS, as the proof of key of a NATS connection
// presenting token, the connection to which the server sent nonce. It holds the proof to what
// RFC 9449 Section 4.3 holds an HTTP request's to, with the connection in the request's place: the
// library's own structural parse (rfc9449.ParseProof: 'typ' dpop+jwt, an algorithm of
// proofAlgorithms, an embedded public key that signed it, a 'jti'), then 'htm'
// ConnectionProofMethod and 'htu' ConnectionProofTarget, an 'iat' inside the window the HTTP check
// accepts (from ProofClockSkew ahead of now until ProofLifespan plus ProofClockSkew behind it),
// 'ath' the base64url SHA-256 of token, 'nonce' the server's own nonce, and last the key's RFC 7638
// thumbprint equal to boundThumbprint, the key the broker bound the token to.
//
// It keeps no replay record: the nonce is the server's, fresh for every connection, so a proof
// captured from one connection names a nonce no other connection is sent. That holds while the
// server sends a nonce to every client, which it does whenever it has an nkey user
// (nats-server server/nkey.go, nonceRequired), and the auth service's own user is one for the
// life of a deployment.
//
// A refusal is a *ProofClaimError naming the claim (ErrProofInvalid), or ErrKeyMismatch for a
// sound proof of another key, and for a token the broker bound to no key at all.
func VerifyConnectionProof(proof, token, nonce, boundThumbprint string, now time.Time) error {
	parsed, err := rfc9449.ParseProof(proof, proofAlgorithms)
	if err != nil {
		return &ProofClaimError{Claim: parseRefusalClaim(proof, err)}
	}
	sum := sha256.Sum256([]byte(token))
	switch {
	case parsed.Method != ConnectionProofMethod:
		return &ProofClaimError{Claim: "htm"}
	case parsed.URL != ConnectionProofTarget:
		return &ProofClaimError{Claim: "htu"}
	case parsed.IssuedAt.After(now.Add(ProofClockSkew)) || now.After(parsed.IssuedAt.Add(ProofLifespan+ProofClockSkew)):
		return &ProofClaimError{Claim: "iat"}
	case parsed.AccessTokenHash == "" ||
		subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(parsed.AccessTokenHash)) != 1:
		return &ProofClaimError{Claim: "ath"}
	case nonce == "" || subtle.ConstantTimeCompare([]byte(nonce), []byte(parsed.Nonce)) != 1:
		return &ProofClaimError{Claim: "nonce"}
	case boundThumbprint == "" || parsed.Thumbprint != boundThumbprint:
		return ErrKeyMismatch
	}
	return nil
}

// quotedClaim is the claim a library refusal's hint names in quotes ("The DPoP proof is missing
// the required 'jti' claim.").
var quotedClaim = regexp.MustCompile(`'([a-z]+)'`)

// parseRefusalClaim names what rfc9449.ParseProof refused proof for. The header's algorithm and
// type are read here first, unverified, because the library reports an algorithm outside the
// allowed set as a parse failure that names no claim; every other refusal names its claim in the
// hint, quoted, and a bad signature or a proof that is no JWS at all names none.
func parseRefusalClaim(proof string, err error) string {
	if header, _, found := strings.Cut(proof, "."); found {
		var fields struct {
			Alg string `json:"alg"`
			Typ string `json:"typ"`
		}
		if raw, decodeErr := base64.RawURLEncoding.DecodeString(header); decodeErr == nil && json.Unmarshal(raw, &fields) == nil {
			if !slices.Contains(proofAlgorithms, jose.SignatureAlgorithm(fields.Alg)) {
				return "alg"
			}
			if fields.Typ != "dpop+jwt" {
				return "typ"
			}
		}
	}
	hint := oauth2.ErrorToRFC6749Error(err).HintField
	if match := quotedClaim.FindStringSubmatch(hint); match != nil {
		return match[1]
	}
	if strings.Contains(hint, "signature") {
		return "signature"
	}
	return "jws"
}
