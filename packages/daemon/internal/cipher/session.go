package cipher

// session.go is a role's own side of the broker's session routes, which a Legion role's GitHub
// forwarder (cmd/legion's `legion forwarder`) calls with the key the role's shim made in its key
// directory and the enrollment id the daemon wrote beside it: it asks for a grant of the role's
// secrets (POST /v1/requests), reads the grant's release (POST /v1/grants/{id}/values), which for
// a secret behind the broker's proxy holds placeholders and never a value, and ends the grant
// (POST /v1/grants/{id}/revoke). Every call is authenticated with a session proof the role's key
// signs (signSessionProof), as the `agent-secrets` client's are. No placeholder, key or proxy
// password goes into an error or a log line.

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The two files of a role's key directory (AGENT_SECRETS_KEY_DIR): the shim's `agent-secrets
// keygen` writes the key as PKCS#8 PEM, and the shim writes the enrollment id the daemon hands it
// (internal/shim's writeEnrollment).
const (
	SessionKeyFile        = "key.pem"
	SessionEnrollmentFile = "enrollment"
)

// ErrNotEnrolled is LoadSession's answer for a key directory that holds no enrollment id yet: the
// daemon enrolls a role's key once its agent has registered, so a forwarder started with the
// generation waits for the file.
var ErrNotEnrolled = errors.New("the key directory holds no enrollment yet")

// Session is one role's enrollment as its own processes hold it: the broker's base URL (the
// request object's audience, and the base every proof's htu is built on), the role's key and the
// enrollment id. HTTP is the client every call goes through; nil is a client that reaches the
// broker directly, never through a proxy its environment names, since the forwarder that calls
// it is the role's proxy itself.
type Session struct {
	URL          string
	Key          *ecdsa.PrivateKey
	EnrollmentID string
	HTTP         *http.Client
}

// LoadSession reads the role's key and enrollment id out of keyDir, for the broker at brokerURL.
// A directory with no enrollment file is ErrNotEnrolled; every other failure names the file and
// never quotes what it holds.
func LoadSession(brokerURL, keyDir string) (*Session, error) {
	if brokerURL == "" {
		return nil, errors.New("no broker URL: AGENT_SECRETS_URL is unset")
	}
	parsed, err := url.Parse(brokerURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("the broker URL is no http(s) URL with a host")
	}
	raw, err := os.ReadFile(filepath.Join(keyDir, SessionEnrollmentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotEnrolled
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(keyDir, SessionEnrollmentFile), err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return nil, ErrNotEnrolled
	}
	keyPath := filepath.Join(keyDir, SessionKeyFile)
	encoded, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no PKCS#8 PEM key", keyPath)
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s holds no PKCS#8 key", keyPath)
	}
	key, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds no ECDSA key", keyPath)
	}
	return &Session{URL: strings.TrimRight(brokerURL, "/"), Key: key, EnrollmentID: id}, nil
}

// RequestAnswer is POST /v1/requests's answer, and GET /v1/requests/{id}'s, as a session reads
// it: the request's id, its state (granted, pending or denied), the grant it made, the lifetime
// the broker gave that grant in seconds, nil for one that lasts until the session ends, and the
// broker's one sentence about a denial.
type RequestAnswer struct {
	RequestID       string  `json:"request_id"`
	State           string  `json:"state"`
	GrantID         *string `json:"grant_id"`
	LifetimeSeconds *int    `json:"lifetime_seconds"`
	Detail          *string `json:"detail"`
}

// Release is POST /v1/grants/{id}/values's answer as a session behind the proxy reads it: when the
// grant expires, nil for one that lasts until its session ends, and how its names behind the
// proxy are reached. Values, the names released as their values, are not read: a role's GitHub
// secrets are behind the proxy, and a forwarder that met a value would refuse it.
type Release struct {
	ExpiresAt *time.Time        `json:"expires_at"`
	Values    map[string]string `json:"values"`
	Proxy     *ProxyRelease     `json:"airlock"`
}

// ProxyRelease is the release's proxy object (the broker's AirlockRelease): the proxy's URL, the
// certificate authority that signs the certificate it shows for each host it carries, the
// password every connection to it opens with (one of the grant's own placeholders) and, by name,
// what stands in for each secret.
type ProxyRelease struct {
	ProxyURL      string               `json:"proxy_url"`
	CACertificate string               `json:"ca_certificate"`
	ProxyPassword string               `json:"proxy_password"`
	Names         map[string]ProxyName `json:"names"`
}

// ProxyName is one secret behind the proxy: its released shape with a placeholder where each part
// of its value would be, each slot's placeholder, and the hosts its kind binds, which a client
// sends through the proxy.
type ProxyName struct {
	Value string            `json:"value"`
	Slots map[string]string `json:"slots"`
	Hosts []string          `json:"hosts"`
}

// Request asks for names for lifetime seconds under reason: POST /v1/requests with a request
// object the role's key signs and no target, so a GitHub secret's grant holds one placeholder for
// each account its App is installed on.
func (s *Session) Request(ctx context.Context, names []string, reason string, lifetime int) (RequestAnswer, error) {
	object, err := signSecretRequest(s.Key, s.URL, names, reason, lifetime, time.Now())
	if err != nil {
		return RequestAnswer{}, fmt.Errorf("sign the request object: %w", err)
	}
	body := struct {
		Request   string  `json:"request"`
		SessionID *string `json:"session_id"`
	}{Request: object}
	var answer RequestAnswer
	if err := s.call(ctx, http.MethodPost, "/v1/requests", "", body, &answer); err != nil {
		return RequestAnswer{}, err
	}
	return answer, nil
}

// WaitRequest reads request id, holding the read up to wait while it is pending (GET
// /v1/requests/{id}?wait=<seconds>, which the broker caps at 20 seconds).
func (s *Session) WaitRequest(ctx context.Context, id string, wait time.Duration) (RequestAnswer, error) {
	if err := pathSegment("a request id", id); err != nil {
		return RequestAnswer{}, err
	}
	seconds := min(max(int(wait/time.Second), 1), 20)
	var answer RequestAnswer
	if err := s.call(ctx, http.MethodGet, "/v1/requests/"+id, "wait="+strconv.Itoa(seconds), nil, &answer); err != nil {
		return RequestAnswer{}, err
	}
	return answer, nil
}

// Values releases grant id: POST /v1/grants/{id}/values with no body, the release as the grant
// holds it.
func (s *Session) Values(ctx context.Context, id string) (Release, error) {
	if err := pathSegment("a grant id", id); err != nil {
		return Release{}, err
	}
	var release Release
	if err := s.call(ctx, http.MethodPost, "/v1/grants/"+id+"/values", "", nil, &release); err != nil {
		return Release{}, err
	}
	return release, nil
}

// Revoke ends grant id: POST /v1/grants/{id}/revoke. A grant that is no longer live is ended
// already, and answers nil.
func (s *Session) Revoke(ctx context.Context, id string) error {
	if err := pathSegment("a grant id", id); err != nil {
		return err
	}
	err := s.call(ctx, http.MethodPost, "/v1/grants/"+id+"/revoke", "", nil, nil)
	var refused *APIError
	if errors.As(err, &refused) && refused.Code == "GRANT_NOT_LIVE" {
		return nil
	}
	return err
}

// call sends one session call, its proof signed over method and the route without query, through
// exchange on s.HTTP, directClient where s names none, under ctx, reading the answer within
// answerBytes, and decodes a 2xx answer into into, or decodes nothing where into is nil. body is
// JSON-encoded where it is not nil. Any other answer is the broker's refusal as an *APIError.
func (s *Session) call(ctx context.Context, method, path, query string, body any, into any) error {
	proof, err := signSessionProof(s.Key, s.EnrollmentID, method, s.URL+path, time.Now())
	if err != nil {
		return fmt.Errorf("sign the proof: %w", err)
	}
	target := s.URL + path
	if query != "" {
		target += "?" + query
	}
	client := s.HTTP
	if client == nil {
		client = directClient
	}
	var encoded []byte
	if body != nil {
		if encoded, err = json.Marshal(body); err != nil {
			return err
		}
	}
	status, _, raw, cut, err := exchange(ctx, client, method, target, path, encoded, answerBytes, func(req *http.Request) { req.Header.Set("Proof", proof) })
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return refusal(status, raw, cut)
	}
	if into == nil {
		return nil
	}
	if cut {
		return fmt.Errorf("broker %s %s: the answer is longer than %d bytes", method, path, answerBytes)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("broker %s %s: the answer is not the route's shape", method, path)
	}
	return nil
}

// directClient reaches the broker with no proxy, whatever the process's environment names, and
// bounds each call: a forwarder's own HTTPS_PROXY names the forwarder itself.
var directClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second},
}

// pathSegment refuses an id that would not be one path segment as written.
func pathSegment(what, id string) error {
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return fmt.Errorf("%s that is no single path segment", what)
	}
	return nil
}
