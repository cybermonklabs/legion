// packages/envoy/internal/cipher/proxyrules/proxyrulestest/castand_test.go
// The certificate authority stand-in's rows: it answers the one request a broker pod sends
// Certificate Authority Service, under its policy and not the request's, and it can be told to
// fail every request and to answer again.
package proxyrulestest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const testPool = "projects/example-project/locations/us-west1/caPools/proxy"

// caRequest is the body a broker pod posts: a CSR for a key of its own that asks to be an
// authority of path length 0 for names, and the lifetime it asks for.
func caRequest(t *testing.T, names []string, lifetime string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The extensions a CSR asks for are the ones a certificate of this shape carries, so they
	// are encoded by the standard library's own certificate encoder and copied into the request.
	shape := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
		KeyUsage:            x509.KeyUsageCertSign,
		PermittedDNSDomains: names, PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, shape, shape, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "a broker pod's intermediate"}, ExtraExtensions: parsed.Extensions,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{
		"pemCsr":   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})),
		"lifetime": lifetime,
	})
	if err != nil {
		t.Fatal(err)
	}
	return key, body
}

// startCAStand serves a CA stand on a plain Service, as cmd/dev-proxy-stands serves it.
func startCAStand(t *testing.T, o CAOptions) (*CAStand, *Service) {
	t.Helper()
	stand, err := NewCAStand(o)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.AnswerWith(stand.Answer)
	return stand, service
}

type caAnswer struct {
	Name                string   `json:"name"`
	PEMCertificate      string   `json:"pemCertificate"`
	PEMCertificateChain []string `json:"pemCertificateChain"`
}

func postCertificate(t *testing.T, service *Service, body []byte) (int, []byte) {
	t.Helper()
	resp, err := http.Post(service.URL()+"/v1/"+testPool+"/certificates?certificateId=proxy-1", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, answer
}

func issued(t *testing.T, answer []byte) (*x509.Certificate, []*x509.Certificate) {
	t.Helper()
	var got caAnswer
	if err := json.Unmarshal(answer, &got); err != nil {
		t.Fatalf("the answer %s: %v", answer, err)
	}
	parse := func(text string) *x509.Certificate {
		block, _ := pem.Decode([]byte(text))
		if block == nil {
			t.Fatalf("no PEM block in %q", text)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	var chain []*x509.Certificate
	for _, text := range got.PEMCertificateChain {
		chain = append(chain, parse(text))
	}
	if got.Name != testPool+"/certificates/proxy-1" {
		t.Errorf("the answer names the certificate %q; want it under the pool by its certificateId", got.Name)
	}
	return parse(got.PEMCertificate), chain
}

// TestTheCAStandIssuesUnderItsPolicy: a request for a 30-day authority of two names gets a
// certificate for its own key of 7 days at most, an authority of path length 0 whose one
// extended key usage is serverAuth, and the names the policy holds rather than the ones the
// request asked for, starting 5 minutes before its issue as a pool whose policy sets that
// backdate issues it, signed by the stand's root, which the answer's chain ends with.
func TestTheCAStandIssuesUnderItsPolicy(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	stand, service := startCAStand(t, CAOptions{Names: []string{"s.airlock.test"}, Now: func() time.Time { return now }})
	key, body := caRequest(t, []string{"s.airlock.test", "elsewhere.example"}, "2592000s")
	status, answer := postCertificate(t, service, body)
	if status != http.StatusOK {
		t.Fatalf("the certificate request = %d %s; want 200", status, answer)
	}
	cert, chain := issued(t, answer)
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Error("the certificate is not for the request's key")
	}
	if !cert.IsCA || cert.MaxPathLen != 0 || !cert.MaxPathLenZero || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("the certificate is CA %v, path length %d (zero %v), key usage %v; want an authority of path length 0 that signs certificates", cert.IsCA, cert.MaxPathLen, cert.MaxPathLenZero, cert.KeyUsage)
	}
	if !slices.Equal(cert.PermittedDNSDomains, []string{"s.airlock.test"}) || !cert.PermittedDNSDomainsCritical {
		t.Errorf("the certificate permits %v (critical %v); want the policy's one name, critical", cert.PermittedDNSDomains, cert.PermittedDNSDomainsCritical)
	}
	if got := fmt.Sprint(cert.ExcludedIPRanges); got != "[0.0.0.0/0 ::/0]" || len(cert.PermittedIPRanges) > 0 {
		t.Errorf("the certificate excludes %s and permits %v of IP addresses; want every address excluded and none permitted", got, cert.PermittedIPRanges)
	}
	if got := fmt.Sprint(cert.ExtKeyUsage); got != "[serverAuth]" || len(cert.UnknownExtKeyUsage) > 0 {
		t.Errorf("the certificate's extended key usage is %s (and %v); want serverAuth alone", got, cert.UnknownExtKeyUsage)
	}
	if start := now.Add(-caBackdate); !cert.NotBefore.Equal(start) || !cert.NotAfter.Equal(start.Add(caMaxLifetime)) {
		t.Errorf("the certificate runs %v to %v; want 5 minutes before the stand's clock and 7 days on", cert.NotBefore, cert.NotAfter)
	}
	if len(chain) != 1 || !chain[0].Equal(stand.Root().Certificate()) {
		t.Fatalf("the chain is %d certificate(s); want the stand's root alone", len(chain))
	}
	if err := cert.CheckSignatureFrom(stand.Root().Certificate()); err != nil {
		t.Errorf("the certificate is not signed by the stand's root: %v", err)
	}
	if got := stand.Issued(); len(got) != 1 || !got[0].Equal(cert) {
		t.Errorf("the stand records %d issued certificate(s); want the one it answered", len(got))
	}
}

// TestTheCAStandTakesTheRequestsNamesWithNoPolicyNames: a policy that names none issues the
// names the request asked for, and a shorter lifetime than the policy's is kept, from the
// backdated start.
func TestTheCAStandTakesTheRequestsNamesWithNoPolicyNames(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	_, service := startCAStand(t, CAOptions{Now: func() time.Time { return now }})
	_, body := caRequest(t, []string{"a.example", "b.example"}, "3600s")
	status, answer := postCertificate(t, service, body)
	if status != http.StatusOK {
		t.Fatalf("the certificate request = %d %s; want 200", status, answer)
	}
	cert, _ := issued(t, answer)
	if !slices.Equal(cert.PermittedDNSDomains, []string{"a.example", "b.example"}) {
		t.Errorf("the certificate permits %v; want the request's two names", cert.PermittedDNSDomains)
	}
	if !cert.NotAfter.Equal(now.Add(-caBackdate).Add(time.Hour)) {
		t.Errorf("the certificate ends %v; want the hour the request asked for, from 5 minutes before its issue", cert.NotAfter)
	}
}

// TestTheCAStandFailsUntilToldToAnswer: POST /fail makes every request fail 503 with
// Google's error shape and issues nothing, POST /answer ends it, and POST /policy replaces
// the names the next certificate carries.
func TestTheCAStandFailsUntilToldToAnswer(t *testing.T) {
	stand, service := startCAStand(t, CAOptions{Names: []string{"s.airlock.test"}})
	control := func(path, body string) {
		t.Helper()
		resp, err := http.Post(service.URL()+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d; want 200", path, resp.StatusCode)
		}
	}
	control("/fail", "")
	_, body := caRequest(t, []string{"s.airlock.test"}, "604800s")
	status, answer := postCertificate(t, service, body)
	var refusal struct {
		Error struct {
			Code   int    `json:"code"`
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &refusal); err != nil || status != http.StatusServiceUnavailable || refusal.Error.Status != "UNAVAILABLE" {
		t.Fatalf("a request while failing = %d %s; want 503 UNAVAILABLE in Google's error shape", status, answer)
	}
	if len(stand.Issued()) != 0 {
		t.Fatal("the stand issued a certificate while failing")
	}
	control("/answer", "")
	control("/policy", `{"names": ["s.airlock.test", "u.airlock.test"]}`)
	status, answer = postCertificate(t, service, body)
	if status != http.StatusOK {
		t.Fatalf("a request after /answer = %d %s; want 200", status, answer)
	}
	cert, _ := issued(t, answer)
	if !slices.Equal(cert.PermittedDNSDomains, []string{"s.airlock.test", "u.airlock.test"}) {
		t.Errorf("the certificate after /policy permits %v; want the policy's two names", cert.PermittedDNSDomains)
	}
}

// TestTheCAStandRefusesARequestItCannotIssue: no certificateId, which the Enterprise tier
// requires, a CSR whose signature is not its key's, and a lifetime that is no duration.
func TestTheCAStandRefusesARequestItCannotIssue(t *testing.T) {
	stand, service := startCAStand(t, CAOptions{})
	_, body := caRequest(t, []string{"s.airlock.test"}, "604800s")
	resp, err := http.Post(service.URL()+"/v1/"+testPool+"/certificates", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a request with no certificateId = %d; want 400", resp.StatusCode)
	}
	_, unsigned := caRequest(t, []string{"s.airlock.test"}, "604800s")
	var doc map[string]string
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(doc["pemCsr"]))
	block.Bytes[len(block.Bytes)-1] ^= 0xff
	doc["pemCsr"] = string(pem.EncodeToMemory(block))
	forged, _ := json.Marshal(doc)
	for name, request := range map[string][]byte{
		"a CSR whose signature is broken": forged,
		"a lifetime that is no duration":  bytes.Replace(body, []byte(`"604800s"`), []byte(`"a week"`), 1),
	} {
		if status, answer := postCertificate(t, service, request); status != http.StatusBadRequest {
			t.Errorf("%s = %d %s; want 400", name, status, answer)
		}
	}
	if len(stand.Issued()) != 0 {
		t.Fatal("the stand issued a certificate for a request it should refuse")
	}
}
