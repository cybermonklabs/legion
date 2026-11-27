// packages/envoy/internal/cipher/proxy/intermediate.go
// The intermediate a broker pod signs its leaves with when its settings name a pool of
// Certificate Authority Service (BROKER_PROXY_CA_POOL): the pod makes an ECDSA P-256 key in its
// own memory, sends a CSR for it to projects.locations.caPools.certificates.create with its own
// Workload Identity token, and signs with the answer. The key is never written anywhere. The
// pool's issuance policy fixes what the certificate may be - 7 days at most, an authority of path
// length 0, the names of the profiles file's hosts - and the CSR asks for exactly that. The pod
// asks for the next one when the one it holds has less than 2 days left, and goes on asking,
// with a backoff, until it gets one.
package proxy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/urljoin"
)

// intermediateLifetime is the life a pod asks for its intermediate, Infisical Agent Proxy's
// value: a key that leaks from a broker pod's memory signs for 7 days at most. The pool's policy
// cuts a longer request to it, so this is the request agreeing with the policy and not the bound.
const intermediateLifetime = 7 * 24 * time.Hour

// IntermediateRenewBefore is how long before its intermediate ends a pod asks for the next: 2
// days lets an outage of the service last a day and still leaves the pod a full day of leaves.
const IntermediateRenewBefore = 2 * 24 * time.Hour

// The retry of a failed request: a second after the failure, then twice as long each time, to
// at most 5 minutes between two requests, which keeps a pod that cannot get one from asking more
// than twelve times an hour while still taking one within minutes of the service answering.
const (
	intermediateRetryFirst = time.Second
	intermediateRetryMax   = 5 * time.Minute
)

// intermediateRequestTimeout bounds one certificate request, so a service that takes the
// connection and never answers is a failed request the backoff retries rather than a pod that
// waits forever with no intermediate.
const intermediateRequestTimeout = 30 * time.Second

// privateCAEndpoint is Certificate Authority Service's own API, which a Pool with no Endpoint
// asks.
const privateCAEndpoint = "https://privateca.googleapis.com"

// maxIntermediateAnswer is the most of an answer the pod reads: a certificate and its chain are
// a few kilobytes, so an answer past this is a mistake rather than a certificate.
const maxIntermediateAnswer = 1 << 20

// The extensions a CSR asks for: an authority, of path length 0, that signs certificates, for
// servers alone, for the hosts' names.
var (
	oidKeyUsage         = []int{2, 5, 29, 15}
	oidBasicConstraints = []int{2, 5, 29, 19}
	oidNameConstraints  = []int{2, 5, 29, 30}
	oidExtKeyUsage      = []int{2, 5, 29, 37}
)

// Pool is where a pod gets its intermediate.
type Pool struct {
	// Name is the pool's resource name, projects/<project>/locations/<location>/caPools/<pool>
	// (BROKER_PROXY_CA_POOL).
	Name string
	// Endpoint is the service's base URL, and "" is Certificate Authority Service's own. The
	// local stack names its stand-in here (BROKER_PROXY_TEST_CA_URL).
	Endpoint string
	// Token is the pod's own credential, each request's bearer token, and nil sends none: the
	// local stand-in reads none, and a stack on a box with a person's application default
	// credentials must not hand them to it.
	Token oauth2.TokenSource
	// HTTP is the client the requests go out on, and nil is a client of its own.
	HTTP *http.Client
}

// NewPoolAuthority is an authority with no certificate yet, which Intermediates fills: it signs
// nothing and is not ready until the pool's first answer.
func NewPoolAuthority(o AuthorityOptions) *Authority { return newAuthority(o) }

// Intermediates gets the pod its intermediate from pool and renews it, until ctx ends: at once
// when the authority holds none, and renewBefore before the end of the one it holds after that
// (IntermediateRenewBefore; the local stack's negative control sets 0), but never sooner than
// intermediateRetryMax after it took that one (renewalTime). Each failed request, the answers it
// refuses included, is logged and asked again after the backoff. Each certificate it takes is
// logged by take.
func (a *Authority) Intermediates(ctx context.Context, pool Pool, renewBefore time.Duration) {
	var taken time.Time
	for {
		if current := a.Certificate(); current != nil {
			if !a.clock.SleepUntil(ctx, renewalTime(current, renewBefore, taken)) {
				return
			}
		}
		wait := intermediateRetryFirst
		for {
			err := a.renew(ctx, pool)
			if err == nil {
				taken = a.clock.Now()
				break
			}
			if ctx.Err() != nil {
				return
			}
			a.log.Warn("proxy intermediate request failed", "pool", pool.Name, "error", err.Error(), "retry_in", wait.String())
			if !a.clock.SleepUntil(ctx, a.clock.Now().Add(wait)) {
				return
			}
			wait = min(2*wait, intermediateRetryMax)
		}
	}
}

// renewalTime is when a pod asks for the next intermediate: before ahead of cert's end, whatever
// cert's life, but no sooner than intermediateRetryMax after taken, the moment the pod took cert.
// An intermediate of 2 days or less has less than 2 days left the moment it arrives, and the floor
// is what keeps a pool that answers such short ones from being asked again in a loop: it is the
// backoff's own bound on how often a pod asks, at most twelve times an hour. The backoff's first
// wait, a second, would let a short answer be asked for again every second.
func renewalTime(cert *x509.Certificate, before time.Duration, taken time.Time) time.Time {
	due := cert.NotAfter.Add(-before)
	if floor := taken.Add(intermediateRetryMax); due.Before(floor) {
		return floor
	}
	return due
}

// renew asks pool once for an intermediate for a new key and takes the answer.
func (a *Authority) renew(ctx context.Context, pool Pool) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("make the intermediate's key: %w", err)
	}
	csr, err := intermediateCSR(key, requestedNames(a.hosts))
	if err != nil {
		return fmt.Errorf("make the CSR: %w", err)
	}
	cert, chain, err := pool.create(ctx, csr)
	if err != nil {
		return err
	}
	if public, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !public.Equal(&key.PublicKey) {
		return errors.New("the answer certifies another key than the request's")
	}
	s, root, err := chainOf(cert, key, chain)
	if err != nil {
		return fmt.Errorf("the answer is %w", err)
	}
	if err := a.take(s, root); err != nil {
		return fmt.Errorf("the answer is refused: %w", err)
	}
	return nil
}

// requestedNames is the name constraint each bound host asks for: an exact host itself, and a
// "*." pattern its domain, which permits each name under it (RFC 5280, section 4.2.1.10). Sorted
// and each once, so one profiles file asks for one list.
func requestedNames(hosts []string) []string {
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, strings.TrimPrefix(host, "*."))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// intermediateCSR is key's CSR asking to be an authority of path length 0 that signs
// certificates for servers alone (serverAuth its one extended key usage), constrained to names
// and to no IP address (checkBounds says why), the constraints critical. The four extensions are
// encoded by the standard library's own certificate encoder, on a throwaway certificate of that
// shape, and copied into the request.
func intermediateCSR(key *ecdsa.PrivateKey, names []string) ([]byte, error) {
	shape := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1, 0),
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
		KeyUsage:            x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:         []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		PermittedDNSDomains: names, PermittedDNSDomainsCritical: true,
		ExcludedIPRanges: everyAddress(),
	}
	der, err := x509.CreateCertificate(rand.Reader, shape, shape, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	encoded, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	var asked []pkix.Extension
	for _, extension := range encoded.Extensions {
		if extension.Id.Equal(oidKeyUsage) || extension.Id.Equal(oidExtKeyUsage) || extension.Id.Equal(oidBasicConstraints) || extension.Id.Equal(oidNameConstraints) {
			asked = append(asked, extension)
		}
	}
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "Cipher proxy intermediate"}, ExtraExtensions: asked,
	}, key)
}

// create is one projects.locations.caPools.certificates.create call for csr: a certificate id
// of this request's own, which a pool of the Enterprise tier requires, and a request id of its
// own. Each attempt is a new key, a new CSR and new ids (renew), so no two requests are the same
// request and the request id deduplicates nothing. It answers the certificate and its chain,
// issuer first and root last.
func (p Pool) create(ctx context.Context, csr []byte) (*x509.Certificate, []*x509.Certificate, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, err
	}
	query := url.Values{
		"certificateId": {fmt.Sprintf("proxy-%d-%s", time.Now().Unix(), hex.EncodeToString(random))},
		"requestId":     {uuid.NewString()},
	}
	body, err := json.Marshal(map[string]string{
		"pemCsr":   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})),
		"lifetime": strconv.FormatInt(int64(intermediateLifetime/time.Second), 10) + "s",
	})
	if err != nil {
		return nil, nil, err
	}
	// The resource is joined onto the endpoint (urljoin.Join), which keeps the endpoint's path and
	// query, and each segment of the pool's name is escaped (url.PathEscape), so none of it reads
	// to the join as a query, a fragment or an escape. An endpoint Join cannot use fails in fixed
	// words: it can carry user information and a query (BROKER_PROXY_TEST_CA_URL), and url.Parse's
	// error quotes it whole.
	asked, err := urljoin.Join(cmp.Or(p.Endpoint, privateCAEndpoint), "v1/"+escapedSegments(p.Name)+"/certificates?"+query.Encode())
	if err != nil {
		return nil, nil, errors.New("the pool's endpoint (BROKER_PROXY_TEST_CA_URL) is not an absolute URL with a host (the URL is not printed)")
	}
	ctx, cancel := context.WithTimeout(ctx, intermediateRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, asked.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("the pool's certificates URL does not parse (the URL is not printed)")
	}
	request.Header.Set("Content-Type", "application/json")
	if p.Token != nil {
		token, err := p.Token.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("the pod's own token for the pool: %w", err)
		}
		token.SetAuthHeader(request)
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		// net/http's error can quote what the pool sent, and names the URL with its user
		// information and query: redact.Unanswered names the request by the URL asked, without.
		return nil, nil, fmt.Errorf("ask the pool: %w", redact.Unanswered(err, request, ""))
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxIntermediateAnswer+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read the pool's answer: %w", err)
	}
	if len(answer) > maxIntermediateAnswer {
		return nil, nil, fmt.Errorf("the pool's answer is past %d bytes", maxIntermediateAnswer)
	}
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("the pool answered %s: %s", response.Status, googleError(answer))
	}
	var issued struct {
		PEMCertificate      string   `json:"pemCertificate"`
		PEMCertificateChain []string `json:"pemCertificateChain"`
	}
	if err := json.Unmarshal(answer, &issued); err != nil {
		return nil, nil, fmt.Errorf("the pool's answer is no certificate: %w", err)
	}
	cert, err := onePEMCertificate(issued.PEMCertificate)
	if err != nil {
		return nil, nil, fmt.Errorf("the pool's pemCertificate: %w", err)
	}
	chain := make([]*x509.Certificate, len(issued.PEMCertificateChain))
	for i, text := range issued.PEMCertificateChain {
		if chain[i], err = onePEMCertificate(text); err != nil {
			return nil, nil, fmt.Errorf("the pool's pemCertificateChain[%d]: %w", i, err)
		}
	}
	return cert, chain, nil
}

// escapedSegments is name, a resource name, with each of its segments escaped (url.PathEscape).
func escapedSegments(name string) string {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// onePEMCertificate is the one PEM certificate text holds.
func onePEMCertificate(text string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(text))
	if block == nil || block.Type != "CERTIFICATE" || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("not one PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// googleError is what a Google API's refusal says, {"error": {"status", "message"}}, and the
// first bytes of a body in any other shape. It is cut short, since it goes in a log line.
func googleError(body []byte) string {
	var refusal struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &refusal) == nil && refusal.Error.Status != "" {
		text = refusal.Error.Status + ": " + refusal.Error.Message
	}
	if runes := []rune(text); len(runes) > 300 {
		text = string(runes[:300]) + "…"
	}
	return text
}
