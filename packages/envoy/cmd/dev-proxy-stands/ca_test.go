// packages/envoy/cmd/dev-proxy-stands/ca_test.go
// The command's own half of the ca stand: it is served at the address the report names, signs
// under the root the report's file holds, issues the names POST /policy gives, dates each
// certificate by the clock --clock-offset-file moves, and fails and answers again on its two
// routes. What the stand issues under its policy is the stand-in package's own rows.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// postCSR sends the ca stand one certificate request for a new key, and answers the status and
// the certificate, when there is one.
func postCSR(t *testing.T, url string) (int, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "a pod"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{
		"pemCsr":   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})),
		"lifetime": "604800s",
	})
	resp, err := http.Post(url+"/v1/projects/example-project/locations/us-west1/caPools/proxy/certificates?certificateId=proxy-1", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var issued struct {
		PEMCertificate string `json:"pemCertificate"`
	}
	if err := json.Unmarshal(answer, &issued); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(issued.PEMCertificate))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, cert
}

func TestTheCAStandIssuesUnderItsNamesAndTheMovedClock(t *testing.T) {
	dir := t.TempDir()
	clock := filepath.Join(dir, "clock-offset")
	if err := os.WriteFile(clock, []byte("48h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := startStands(t, "--dir", dir, "--clock-offset-file", clock)
	ca := out.Stands[standCA]
	if ca.URL == "" || ca.CertFile != "" {
		t.Fatalf("the report's ca stand is %+v; want a plain URL", ca)
	}
	rootPEM, err := os.ReadFile(out.ProxyRootFile)
	if err != nil {
		t.Fatalf("the report's proxy_root_file %q: %v", out.ProxyRootFile, err)
	}
	block, _ := pem.Decode(rootPEM)
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body string) {
		t.Helper()
		resp, err := http.Post(ca.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d; want 200", path, resp.StatusCode)
		}
	}
	post("/policy", `{"names": ["s.airlock.test", "t.airlock.test"]}`)
	status, cert := postCSR(t, ca.URL)
	if status != http.StatusOK {
		t.Fatalf("the certificate request = %d; want 200", status)
	}
	if err := cert.CheckSignatureFrom(root); err != nil {
		t.Errorf("the certificate is not signed by the root the report names: %v", err)
	}
	if !slices.Equal(cert.PermittedDNSDomains, []string{"s.airlock.test", "t.airlock.test"}) {
		t.Errorf("the certificate permits %v; want the two names of the policy", cert.PermittedDNSDomains)
	}
	if ahead := time.Until(cert.NotBefore); ahead < 47*time.Hour || ahead > 49*time.Hour {
		t.Errorf("the certificate starts %v from now; want the 48 hours the clock file moves it", ahead)
	}

	post("/fail", "")
	if status, _ := postCSR(t, ca.URL); status != http.StatusServiceUnavailable {
		t.Errorf("a request after /fail = %d; want 503", status)
	}
	post("/answer", "")
	if status, _ := postCSR(t, ca.URL); status != http.StatusOK {
		t.Errorf("a request after /answer = %d; want 200", status)
	}
}
