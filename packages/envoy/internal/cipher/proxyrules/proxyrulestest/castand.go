// packages/envoy/internal/cipher/proxyrules/proxyrulestest/castand.go
// The stand-in of Certificate Authority Service, for the one call a broker pod makes of it:
// projects.locations.caPools.certificates.create, which signs the intermediate the pod's
// proxy signs its leaves with. It signs with a test root of its own under a policy as a pool's
// issuance policy is one: at most 7 days, starting 5 minutes before its issue, an authority of
// path length 0 for servers alone (serverAuth its one extended key usage), the policy's names in
// place of whatever the request asked for, and every IPv4 and IPv6 address excluded, the
// constraints critical. BROKER_PROXY_TEST_CA_URL points a broker at it, and cmd/dev-proxy-stands
// serves it to the local stack as the `ca` stand.
package proxyrulestest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// caMaxLifetime is the longest certificate the stand issues unless CAOptions.MaxLifetime says
// less, the 7 days of the pool's maximum_lifetime: a request for longer is cut to it, as the pool
// cuts one.
const caMaxLifetime = 7 * 24 * time.Hour

// caBackdate is how long before its issue a certificate the stand issues starts, as a pool whose
// issuance policy sets backdateDuration 300s issues it: the start moves back and the requested
// lifetime is kept, so the end moves back with it.
const caBackdate = 5 * time.Minute

// caCertificatesPath is the one route of the service a broker calls:
// POST /v1/projects/{project}/locations/{location}/caPools/{pool}/certificates.
var caCertificatesPath = regexp.MustCompile(`^/v1/(projects/[^/]+/locations/[^/]+/caPools/[^/]+)/certificates$`)

// nameConstraintsOID is the X.509 extension a certificate's permitted names are in (RFC 5280,
// section 4.2.1.10), which a CSR asks for as one of its requested extensions.
var nameConstraintsOID = []int{2, 5, 29, 30}

// CAOptions is what a CAStand issues under.
type CAOptions struct {
	// Names are the policy's permitted names, which every certificate carries as its name
	// constraints whatever the request asked for, as a pool's baseline values overwrite a
	// request's. None issues the names the request asked for.
	Names []string
	// Now is the clock the certificates are dated by, and nil is time.Now. The local stack
	// moves it with the broker's own (BROKER_PROXY_TEST_CLOCK_OFFSET_FILE).
	Now func() time.Time
	// MaxLifetime is the longest certificate the stand issues, a pool whose maximum_lifetime is
	// shorter than the 7 days a pod asks for, and zero is caMaxLifetime.
	MaxLifetime time.Duration
}

// CAStand answers the certificate request, three routes that change what it answers, and
// nothing else. Install it with Service.AnswerWith(stand.Answer).
type CAStand struct {
	root *Authority
	now  func() time.Time
	// maxLifetime is CAOptions.MaxLifetime, caMaxLifetime where that is zero.
	maxLifetime time.Duration

	mu      sync.Mutex
	names   []string
	failing bool
	issued  []*x509.Certificate
}

// NewCAStand builds one, with a test root of its own, which Root answers.
func NewCAStand(o CAOptions) (*CAStand, error) {
	root, err := NewAuthority("proxy test root")
	if err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	maxLifetime := o.MaxLifetime
	if maxLifetime <= 0 {
		maxLifetime = caMaxLifetime
	}
	return &CAStand{root: root, now: now, maxLifetime: maxLifetime, names: slices.Clone(o.Names)}, nil
}

// Root is the root every certificate the stand issues chains to, the one a client trusts the
// proxy by.
func (c *CAStand) Root() *Authority { return c.root }

// Fail makes every certificate request from here on fail, 503 UNAVAILABLE as the service
// answers in an outage, and false makes them answered again.
func (c *CAStand) Fail(failing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failing = failing
}

// SetNames replaces the policy's names, which is how a change of the profiles file applies the
// pool's issuance policy before it rolls the brokers.
func (c *CAStand) SetNames(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = slices.Clone(names)
}

// Issued is every certificate the stand has issued, in order.
func (c *CAStand) Issued() []*x509.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.issued)
}

// Answer is the stand's answer: POST /fail, POST /answer and POST /policy ({"names": [...]})
// set what it answers, and the certificate request is answered under that.
func (c *CAStand) Answer(w http.ResponseWriter, _ *http.Request, seen Request) bool {
	if seen.Method != http.MethodPost {
		return false
	}
	switch seen.Path {
	case "/fail", "/answer":
		c.Fail(seen.Path == "/fail")
		writeCAJSON(w, http.StatusOK, map[string]bool{"failing": seen.Path == "/fail"})
		return true
	case "/policy":
		var body struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
			writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the policy is no JSON object of names: "+err.Error())
			return true
		}
		c.SetNames(body.Names)
		writeCAJSON(w, http.StatusOK, map[string][]string{"names": body.Names})
		return true
	}
	match := caCertificatesPath.FindStringSubmatch(seen.Path)
	if match == nil {
		return false
	}
	c.answerCertificate(w, seen, match[1])
	return true
}

// answerCertificate issues one certificate, or refuses as the service refuses.
func (c *CAStand) answerCertificate(w http.ResponseWriter, seen Request, pool string) {
	c.mu.Lock()
	failing, names := c.failing, slices.Clone(c.names)
	c.mu.Unlock()
	if failing {
		writeCAError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the stand-in Certificate Authority Service fails every request until POST /answer")
		return
	}
	query, err := url.ParseQuery(seen.Query)
	if err != nil || query.Get("certificateId") == "" {
		writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "a pool of the Enterprise tier needs a certificateId")
		return
	}
	var body struct {
		PEMCSR   string `json:"pemCsr"`
		Lifetime string `json:"lifetime"`
	}
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
		writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "the body is no certificate: "+err.Error())
		return
	}
	lifetime, err := caDuration(body.Lifetime)
	if err != nil {
		writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "lifetime: "+err.Error())
		return
	}
	csr, err := parseCSR(body.PEMCSR)
	if err != nil {
		writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "pemCsr: "+err.Error())
		return
	}
	if len(names) == 0 {
		if names, err = requestedNames(csr); err != nil {
			writeCAError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "pemCsr: "+err.Error())
			return
		}
	}
	cert, err := c.issue(csr, names, min(lifetime, c.maxLifetime))
	if err != nil {
		writeCAError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeCAJSON(w, http.StatusOK, map[string]any{
		"name":                pool + "/certificates/" + query.Get("certificateId"),
		"lifetime":            strconv.FormatInt(int64(cert.NotAfter.Sub(cert.NotBefore)/time.Second), 10) + "s",
		"pemCertificate":      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})),
		"pemCertificateChain": []string{string(c.root.CertificatePEM())},
	})
}

// issue signs the CSR's key as an authority of path length 0 for servers alone (serverAuth its
// one extended key usage), for names and for no IP address (0.0.0.0/0 and ::/0 excluded, as a
// pool's baseline values are), from caBackdate before the stand's clock for lifetime.
func (c *CAStand) issue(csr *x509.CertificateRequest, names []string, lifetime time.Duration) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	start := c.now().Add(-caBackdate)
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     csr.Subject,
		NotBefore:                   start,
		NotAfter:                    start.Add(lifetime),
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:                 []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         names,
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.root.cert, csr.PublicKey, c.root.key)
	if err != nil {
		return nil, fmt.Errorf("sign the intermediate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.issued = append(c.issued, cert)
	c.mu.Unlock()
	return cert, nil
}

// parseCSR is the request's PEM CSR, its signature checked against its own key.
func parseCSR(text string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the signature is not the key's: %w", err)
	}
	return csr, nil
}

// requestedNames reads the names the CSR asks to be constrained to. The standard library reads
// name constraints out of a certificate and not out of a request, so the request's extension is
// put on a throwaway certificate and read back from it.
func requestedNames(csr *x509.CertificateRequest) ([]string, error) {
	for _, extension := range csr.Extensions {
		if !extension.Id.Equal(nameConstraintsOID) {
			continue
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Minute), ExtraExtensions: []pkix.Extension{extension}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			return nil, fmt.Errorf("the requested name constraints: %w", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("the requested name constraints: %w", err)
		}
		return cert.PermittedDNSDomains, nil
	}
	return nil, nil
}

// caDuration reads a google.protobuf.Duration as JSON writes it: seconds, with a fraction or
// none, and an "s".
func caDuration(text string) (time.Duration, error) {
	seconds, ok := strings.CutSuffix(text, "s")
	if !ok {
		return 0, fmt.Errorf("%q is no duration such as 604800s", text)
	}
	value, err := strconv.ParseFloat(seconds, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%q is no duration such as 604800s", text)
	}
	return time.Duration(value * float64(time.Second)), nil
}

func writeCAJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeCAError answers in the shape a Google API refuses in.
func writeCAError(w http.ResponseWriter, status int, code, message string) {
	writeCAJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message, "status": code}})
}
