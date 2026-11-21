// Package oidctest is a local OIDC issuer for tests of the code that verifies
// OIDC tokens: an httptest server answering discovery and JWKS for keys the
// test controls, the minting side a Kubernetes pod's projected service-account
// token would come from, and, once EnableCodeFlow arms it, the authorization,
// token and revocation endpoints a person's sign-in goes through
// (authorization code with optional PKCE S256, refresh token, RFC 7009
// revocation), and, once EnableDeviceFlow arms it, the device authorization
// endpoint and the device code grant a device with no browser signs in
// through (RFC 8628). It is an ordinary
// package rather than a _test.go file so the packages that need it —
// internal/oidc, internal/dispatch/api, internal/dispatch/routes,
// internal/dispatch/store, cmd/dispatch and cmd/listener — can all import it;
// Go cannot share test-only code across packages. cmd/dev-oidc-issuer serves it
// outside any test, as the local development stand-in for a cluster's issuer.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// TB is the part of *testing.T this package uses. Taking the interface keeps
// the testing package out of a non-test build.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Key is one signing key of the issuer: an RSA key it signs with RS256 (NewKey), or a P-256 key it
// signs with ES256 (NewES256Key). A key exists as soon as it is generated; it is only verifiable
// once Publish puts it in the served set.
type Key struct {
	kid  string
	alg  jose.SignatureAlgorithm
	priv crypto.Signer
}

// KeyID is the key's `kid`, which a test needs to mint a foreign key under the
// same id — the one shape where the key set resolves and only the signature fails.
func (k *Key) KeyID() string { return k.kid }

// NewKey generates an RS256 signing key the issuer does not serve until Publish.
func NewKey(t TB, kid string) *Key {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key %s: %v", kid, err)
	}
	return &Key{kid: kid, alg: jose.RS256, priv: priv}
}

// NewES256Key generates a P-256 signing key the issuer signs with ES256, as Identity-Aware Proxy
// signs its assertions, and does not serve until Publish.
func NewES256Key(t TB, kid string) *Key {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key %s: %v", kid, err)
	}
	return &Key{kid: kid, alg: jose.ES256, priv: priv}
}

// Issuer serves a discovery document and a key set, both under the test's control.
type Issuer struct {
	server *httptest.Server
	mux    *http.ServeMux

	mu                sync.Mutex
	issuerClaim       string
	discoveryStatus   int
	discoveryDelay    time.Duration
	discoveryRequests int
	served            []jose.JSONWebKey
	jwksRequests      int
	keysStatus        int
	keysDelay         time.Duration
	omitCodeFlow      bool
	omitRevocation    bool
	codeFlow          *codeFlow
}

// New starts an issuer on a random loopback port, serving no keys yet. The
// server is closed at cleanup.
func New(t TB) *Issuer {
	t.Helper()
	issuer := &Issuer{mux: http.NewServeMux()}
	mux := issuer.mux
	mux.HandleFunc("/.well-known/openid-configuration", issuer.handleDiscovery)
	mux.HandleFunc("/keys", issuer.handleKeys)
	mux.HandleFunc("GET /authorize", issuer.handleAuthorize)
	mux.HandleFunc("POST /device_authorization", issuer.handleDeviceAuthorization)
	mux.HandleFunc("POST /token", issuer.handleToken)
	mux.HandleFunc("POST /revoke", issuer.handleRevoke)
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	issuer.issuerClaim = issuer.server.URL
	return issuer
}

// URL is the issuer's base URL, which is also the `iss` it advertises and mints
// until AdvertiseIssuer says otherwise.
func (i *Issuer) URL() string { return i.server.URL }

// Handle serves handler for pattern (an http.ServeMux pattern) alongside the
// issuer's own endpoints, so a caller can add a route of its own at the
// issuer's URL: cmd/dev-oidc-issuer's control route that queues the next
// sign-in.
func (i *Issuer) Handle(pattern string, handler http.Handler) {
	i.mux.Handle(pattern, handler)
}

// PublishKey generates a key and serves it — the common case, for a test that
// only needs one working signing key.
func (i *Issuer) PublishKey(t TB, kid string) *Key {
	t.Helper()
	key := NewKey(t, kid)
	i.Publish(key)
	return key
}

// Publish adds a key to the served set.
func (i *Issuer) Publish(key *Key) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.served = append(i.served, jose.JSONWebKey{
		Key:       key.priv.Public(),
		KeyID:     key.kid,
		Algorithm: string(key.alg),
		Use:       "sig",
	})
}

// AdvertiseIssuer makes the discovery document name a different issuer than the
// URL it is served from.
func (i *Issuer) AdvertiseIssuer(issuer string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.issuerClaim = issuer
}

// FailDiscovery makes the discovery document answer with an HTTP status; a
// status of 0 makes it answer the document again.
func (i *Issuer) FailDiscovery(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.discoveryStatus = status
}

// DelayDiscovery makes the discovery document wait d before answering, or
// until the client hangs up, so a test can hold one discovery read in flight
// while others arrive.
func (i *Issuer) DelayDiscovery(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.discoveryDelay = d
}

// DiscoveryRequests counts the discovery document's reads, whatever it
// answered, so a test can prove when discovery happened and how often.
func (i *Issuer) DiscoveryRequests() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.discoveryRequests
}

// DelayKeys makes the key-set endpoint wait before answering. A test about a
// caller that goes away mid-verification needs this: go-oidc selects between
// the caller's ctx.Done() and its own inflight fetch, and against a local
// issuer that answers in microseconds both cases are ready at once, so Go
// picks between them at random. Over a network the fetch is the slow one,
// which is the case the test means to describe.
func (i *Issuer) DelayKeys(d time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keysDelay = d
}

// FailKeys makes the key-set endpoint answer with an HTTP status instead of
// the set; a status of 0 makes it answer the set again. It is an issuer whose
// JWKS cannot be read while its discovery document still reads, the outage a
// verifier reports as the issuer being unavailable rather than as a bad
// signature.
func (i *Issuer) FailKeys(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.keysStatus = status
}

// JWKSFetches counts key-set reads, so a test can prove a refresh happened.
func (i *Issuer) JWKSFetches() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.jwksRequests
}

// Claims is a valid projected service-account token's claim set, for a test to
// spoil one claim at a time.
func (i *Issuer) Claims(subject, audience string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": i.URL(),
		"sub": subject,
		"aud": []string{audience},
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// Mint signs claims with key, producing the compact token a pod would present.
func (i *Issuer) Mint(t TB, key *Key, claims map[string]any) string {
	t.Helper()
	raw, err := sign(key, claims)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return raw
}

func sign(key *Key, claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal claims: %w", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: key.alg, Key: jose.JSONWebKey{Key: key.priv, KeyID: key.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("new signer: %w", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", fmt.Errorf("sign claims: %w", err)
	}
	return signed.CompactSerialize()
}

func (i *Issuer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	i.discoveryRequests++
	status, issuer, omitCodeFlow, omitRevocation, delay := i.discoveryStatus, i.issuerClaim, i.omitCodeFlow, i.omitRevocation, i.discoveryDelay
	i.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		http.Error(w, "discovery unavailable", status)
		return
	}
	document := map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              i.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{string(jose.RS256)},
	}
	if !omitCodeFlow {
		document["authorization_endpoint"] = i.server.URL + "/authorize"
		document["token_endpoint"] = i.server.URL + "/token"
		document["device_authorization_endpoint"] = i.server.URL + "/device_authorization"
		document["grant_types_supported"] = []string{"authorization_code", "refresh_token", deviceCodeGrantType}
	}
	if !omitRevocation {
		document["revocation_endpoint"] = i.server.URL + "/revoke"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(document)
}

func (i *Issuer) handleKeys(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.jwksRequests++
	set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), i.served...)}
	delay, status := i.keysDelay, i.keysStatus
	i.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 {
		http.Error(w, "key set unavailable", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(set)
}
