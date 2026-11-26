package cipher

import (
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

// The daemon's own JWS signers: byte-compatible with the broker's real verifier code
// (packages/envoy/internal/cipher/record/sign.go and internal/cipher/proof/sign.go) — same
// go-jose construction (EmbedJWK, the typ header, ES256), same claim names, same iat/exp
// relationship. This package holds no import of packages/envoy: the daemon's production binary
// shares no code path with the broker, only the wire contract.

const (
	requestTyp = "agent-secrets-request+jwt"
	proofTyp   = "agent-secrets-proof+jwt"

	// requestLifetimeSeconds bounds how far past iat a request object's exp sits: the broker's
	// record.VerifyRequestObject refuses an exp later than iat+600.
	requestLifetimeSeconds = 600
)

// authorizationDetail is one entry of a request object's RFC 9396 authorization_details, mirrored
// from envoy's record.AuthorizationDetail wire shape. A token login sends exactly one
// "launcher_credential" entry, which names a service; a role's request for secrets sends one
// "agent_secret" entry for each name, to inject (record.SecretDetails), with no locations, since
// a role's grant names no target.
type authorizationDetail struct {
	Type       string   `json:"type"`
	Identifier string   `json:"identifier"`
	Actions    []string `json:"actions,omitempty"`
	Service    string   `json:"service,omitempty"`
}

// requestClaims is a credential-request object's JSON payload, mirrored from envoy's
// record.requestClaims. Reason is empty for a token login (envoy's own sign.go leaves it empty
// too) and so never appears on the wire there, and no person approves either request this package
// signs, so it carries no login_hint. LifetimeSeconds is a role's grant's lifetime, nil for a
// token login.
type requestClaims struct {
	Issuer               string                `json:"iss"`
	Audience             string                `json:"aud"`
	JTI                  string                `json:"jti"`
	IssuedAt             int64                 `json:"iat"`
	Expires              int64                 `json:"exp"`
	AuthorizationDetails []authorizationDetail `json:"authorization_details"`
	Reason               string                `json:"reason,omitempty"`
	LifetimeSeconds      *int                  `json:"lifetime_seconds,omitempty"`
}

// proofClaims is a proof's JSON payload, mirrored from envoy's proof.claims: exactly one of
// LauncherID (a launcher proof, the daemon's own) and EnrollmentID (a session proof, a role's
// forwarder's) names who proves they hold the key.
type proofClaims struct {
	JTI          string `json:"jti"`
	IssuedAt     int64  `json:"iat"`
	Method       string `json:"htm"`
	URL          string `json:"htu"`
	EnrollmentID string `json:"eid,omitempty"`
	LauncherID   string `json:"lid,omitempty"`
}

// thumbprint is RFC 7638 over the public JWK, base64url without padding — the value a request
func thumbprint(pub *ecdsa.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// signRequestObject signs the request object a token login posts beside the daemon's
// ServiceAccount token: one launcher_credential detail naming host and the service
// launcherService, signed by the key the credential is bound to, which proves the daemon holds it.
// Byte-compatible with envoy's record.Sign.
func signRequestObject(key *ecdsa.PrivateKey, audience, host string, now time.Time) (string, error) {
	return signRequest(key, requestClaims{
		Audience:             audience,
		AuthorizationDetails: []authorizationDetail{{Type: "launcher_credential", Identifier: host, Service: launcherService}},
	}, now)
}

// signSecretRequest signs a session's request object for names, granted for lifetime seconds
// under reason, with no target: one agent_secret detail per name, to inject, as envoy's
// record.SecretDetails builds them. Byte-compatible with envoy's record.Sign.
func signSecretRequest(key *ecdsa.PrivateKey, audience string, names []string, reason string, lifetime int, now time.Time) (string, error) {
	details := make([]authorizationDetail, len(names))
	for i, name := range names {
		details[i] = authorizationDetail{Type: "agent_secret", Identifier: name, Actions: []string{"inject"}}
	}
	return signRequest(key, requestClaims{Audience: audience, AuthorizationDetails: details, Reason: reason, LifetimeSeconds: &lifetime}, now)
}

// signRequest signs claims as a request object: the key's thumbprint as iss, a fresh jti, iat at
// now and exp requestLifetimeSeconds later.
func signRequest(key *ecdsa.PrivateKey, claims requestClaims, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: requestTyp},
	})
	if err != nil {
		return "", err
	}
	iss, err := thumbprint(&key.PublicKey)
	if err != nil {
		return "", err
	}
	iat := now.Unix()
	claims.Issuer, claims.JTI, claims.IssuedAt, claims.Expires = iss, uuid.NewString(), iat, iat+requestLifetimeSeconds
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

// signLauncherProof signs a launcher proof identifying lid (the launcher credential's own id) as
// the key's subject. Byte-compatible with envoy's proof.SignLauncher.
func signLauncherProof(key *ecdsa.PrivateKey, lid, method, url string, now time.Time) (string, error) {
	return signProof(key, proofClaims{LauncherID: lid}, method, url, now)
}

// signSessionProof signs a session proof identifying eid (the role's enrollment id) as the key's
// subject. Byte-compatible with envoy's proof.Sign.
func signSessionProof(key *ecdsa.PrivateKey, eid, method, url string, now time.Time) (string, error) {
	return signProof(key, proofClaims{EnrollmentID: eid}, method, url, now)
}

// signProof signs claims, whose subject the caller set, over method and url at now.
func signProof(key *ecdsa.PrivateKey, claims proofClaims, method, url string, now time.Time) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: proofTyp},
	})
	if err != nil {
		return "", err
	}
	claims.JTI, claims.IssuedAt, claims.Method, claims.URL = uuid.NewString(), now.Unix(), method, url
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
