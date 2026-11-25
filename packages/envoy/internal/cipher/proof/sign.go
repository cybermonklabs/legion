package proof

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/sjawhar/envoy/cipheraddr"
)

const typeHeader = "agent-secrets-proof+jwt"

// claims is a session proof's payload: exactly one of EnrollmentID or LauncherID identifies who
// is proving they hold the signing key — an enrolled session (eid) or a key-bound launcher
// credential authenticating itself directly (lid, machine logins).
type claims struct {
	JTI          string `json:"jti"`
	IssuedAt     int64  `json:"iat"`
	Method       string `json:"htm"`
	URL          string `json:"htu"`
	EnrollmentID string `json:"eid,omitempty"`
	LauncherID   string `json:"lid,omitempty"`
}

func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Thumbprint is RFC 7638 over the public JWK, base64url without padding — the value the
// launcher enrols and the value every proof is compared against.
func Thumbprint(pub *ecdsa.PublicKey) (string, error) {
	return thumbprintOf(jose.JSONWebKey{Key: pub})
}

func thumbprintOf(jwk jose.JSONWebKey) (string, error) {
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// sign signs c over method, url and now, filling in jti/iat/htm/htu; the caller has already set
// c's eid or lid.
func sign(key *ecdsa.PrivateKey, method, url string, now time.Time, c claims) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, &jose.SignerOptions{
		EmbedJWK:     true,
		ExtraHeaders: map[jose.HeaderKey]any{jose.HeaderType: typeHeader},
	})
	if err != nil {
		return "", err
	}
	c.JTI = uuid.NewString()
	c.IssuedAt = now.Unix()
	c.Method = method
	c.URL = url
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return sig.CompactSerialize()
}

// Sign builds and signs a session proof identifying enrollmentID (the "eid" claim) — a session's
// own ongoing authentication.
func Sign(key *ecdsa.PrivateKey, enrollmentID, method, url string, now time.Time) (string, error) {
	return sign(key, method, url, now, claims{EnrollmentID: enrollmentID})
}

// SignLauncher builds and signs a session proof identifying launcherID directly (the "lid"
// claim) — a key-bound launcher credential's own ongoing authentication once a machine login is
// approved, with no enrollment involved at all.
func SignLauncher(key *ecdsa.PrivateKey, launcherID, method, url string, now time.Time) (string, error) {
	return sign(key, method, url, now, claims{LauncherID: launcherID})
}

// TargetURI is the htu a proof of a request to u signs: u's scheme, host and port, and path,
// without its query and fragment (RFC 9449 section 4.2) and without its user information, which
// is no part of the request's target (RFC 9110 section 4.2.4). The broker checks a proof against
// its public URL followed by the path the request reached it at (the url Verifier.Verify is given,
// internal/cipher/api's proofSubject), which holds none of the three, so a proof signed over the
// whole URL of a request to an address with a query or a user name is refused. agent-secrets, its
// helper and the token client (cipherclient) each sign TargetURI of the URL their request asks.
func TargetURI(u *url.URL) string {
	target := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	return target.String()
}

// Audience is the audience a request object signed for the broker at address names (record.Sign's
// aud): TargetURI of the address as configured, so none of the query, fragment or user information
// it may carry. The broker verifies an object for its own public URL exactly
// (record.VerifyRequestObject), which holds none of the three, so an object signed for the address's
// whole text is refused. An address that is no absolute URL with a host is urljoin.ErrBase, which
// quotes no part of it.
func Audience(address cipheraddr.Address) (string, error) {
	u, err := address.Join("")
	if err != nil {
		return "", err
	}
	return TargetURI(u), nil
}
