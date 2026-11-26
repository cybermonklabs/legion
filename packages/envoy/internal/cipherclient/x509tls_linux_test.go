// packages/envoy/internal/cipherclient/x509tls_linux_test.go
//
// An X.509 external_account credential's exchange presents its certificate to the security token
// service over TLS, as Google's mTLS endpoint requires, whichever way the credential names its
// certificate configuration. cloud.google.com/go/auth makes that call on a client of its own that
// trusts the system's roots alone, so each row mints in a child process of this test binary whose
// SSL_CERT_FILE names the stand-in's certificate: crypto/x509 reads that variable once, when it
// first loads the system's roots, and on Linux only, hence this file's name.

package cipherclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// x509ChildCredential and x509ChildIAM carry, to the child, the credential file it mints with and
// the IAM Credentials stand-in it mints at. The first set makes the test binary the child.
const (
	x509ChildCredential = "CIPHERCLIENT_X509_CHILD_CREDENTIAL"
	x509ChildIAM        = "CIPHERCLIENT_X509_CHILD_IAM"
)

// TestAnX509CredentialPresentsItsCertificateToTheTokenService: the security token service stand-in
// requires a TLS client certificate and records the one it was shown; an X.509 credential mints
// through it and shows the credential's own certificate, whether the credential names its
// certificate configuration (certificate_config_location, as `create-cred-config
// --credential-cert-configuration-output-file` writes it) or takes the default one
// (use_default_certificate_config, as `create-cred-config` writes it otherwise, which
// GOOGLE_API_CERTIFICATE_CONFIG names here). A client that presents no certificate is refused in
// the handshake, so the mint fails. The federated token is spent on generateIdToken itself
// (federationIAM), and the credential's service_account_impersonation_url is never called. The
// service's refusal of such an exchange prints its status and registered code, read from the text
// of the library's error (libraryAnswer), and never the rest of its body.
func TestAnX509CredentialPresentsItsCertificateToTheTokenService(t *testing.T) {
	if path := os.Getenv(x509ChildCredential); path != "" {
		mintAsX509Child(t, path)
		return
	}
	const secret = "hunter2pw-x509refusal-8c41"
	for _, row := range []struct {
		name               string
		useDefault, refuse bool
		want               string
	}{
		{"certificate_config_location", false, false, "minted robot-identity-token"},
		{"use_default_certificate_config", true, false, "minted robot-identity-token"},
		{"a refusal of the exchange", false, true, "refused: mint a federated access token with the external_account credential " +
			credentialsFileVar + " names, to act as robot@example.iam.gserviceaccount.com: the token service the credential names: the server answered HTTP 403 access_denied"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			var shown [][]byte
			sts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				for _, cert := range r.TLS.PeerCertificates {
					shown = append(shown, cert.Raw)
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if row.refuse {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"echo: ` + secret + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"` + accessField + `":"not-a-secret-federated-access-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
			}))
			sts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
			sts.StartTLS()
			t.Cleanup(sts.Close)
			impersonation, impersonated := impersonationStandIn(t)
			iam, _ := federationIAM(t)
			dir := t.TempDir()
			files := writeX509Credential(t, dir, sts.URL, impersonation)
			env := []string{x509ChildIAM + "=" + iam}
			if row.useDefault {
				rewriteCertificateSource(t, files.credential, map[string]any{"use_default_certificate_config": true})
				env = append(env, "GOOGLE_API_CERTIFICATE_CONFIG="+files.config)
			}
			roots := filepath.Join(dir, "roots.pem")
			if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: sts.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}

			child := exec.Command(os.Args[0], "-test.run", "^"+strings.Split(t.Name(), "/")[0]+"$", "-test.count=1", "-test.v")
			child.Env = append(os.Environ(), append(env, x509ChildCredential+"="+files.credential, "SSL_CERT_FILE="+roots, "SSL_CERT_DIR="+dir)...)
			out, err := child.CombinedOutput()
			if err != nil || !bytes.Contains(out, []byte(row.want)) {
				t.Fatalf("the child did not print %q (%v):\n%s", row.want, err, out)
			}
			if bytes.Contains(out, []byte(secret)) {
				t.Fatalf("the child printed the answer's body:\n%s", out)
			}
			cert, err := os.ReadFile(files.cert)
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(cert)
			mu.Lock()
			defer mu.Unlock()
			if len(shown) == 0 || !bytes.Equal(shown[0], block.Bytes) {
				t.Fatalf("the security token service was shown %d certificates, the first not the credential's own", len(shown))
			}
			if n := impersonated.Load(); n != 0 {
				t.Fatalf("the service_account_impersonation_url saw %d requests; want none", n)
			}
		})
	}
}

// mintAsX509Child is the child's half: one mint with the credential at path, whose token or error
// it prints on stdout for the parent to read.
func mintAsX509Child(t *testing.T, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: "robot@example.iam.gserviceaccount.com",
		Audience: testAudience, HTTP: HTTPClient(), IAMCredentialsURL: os.Getenv(x509ChildIAM)}
	token, _, err := identity.Token(context.Background())
	if err != nil {
		fmt.Println("refused:", err)
		return
	}
	fmt.Println("minted", token)
}

// rewriteCertificateSource replaces the certificate block of the credential file at path with
// certificate.
func rewriteCertificateSource(t *testing.T, path string, certificate map[string]any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credential map[string]any
	if err := json.Unmarshal(data, &credential); err != nil {
		t.Fatal(err)
	}
	credential["credential_source"] = map[string]any{"certificate": certificate}
	if data, err = json.Marshal(credential); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
