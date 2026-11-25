// packages/envoy/internal/cipherclient/detect_test.go
//
// DetectMachineIdentity finds the identity a machine can trade at the broker with no setting of
// its own, for the Google exchange audience the broker's settings name: an external_account
// credential file with a service account to act as, or the Compute Engine metadata server, in
// that order. Each row runs the real readers against local stand-ins: a credentials file
// GOOGLE_APPLICATION_CREDENTIALS names, and a metadata server GCE_METADATA_HOST points at.

package cipherclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// testAudience is the broker's Google exchange audience in these rows, which a metadata or an
// external_account identity token must carry.
const testAudience = "agent-secrets-vm"

// detectMetadata is a Compute Engine metadata server that names robot@ as the instance's
// service account and counts every request, so a row asserts it was never asked.
type detectMetadata struct{ requests atomic.Int64 }

// startDetectMetadata serves a detectMetadata and points GCE_METADATA_HOST at it for the rest of t.
func startDetectMetadata(t *testing.T) *detectMetadata {
	t.Helper()
	m := &detectMetadata{}
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		m.requests.Add(1)
		w.Header().Set("Metadata-Flavor", "Google")
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/email" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("machine@example.iam.gserviceaccount.com"))
	})
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(base, "http://"))
	return m
}

// closedAddress is a loopback address nothing listens on.
func closedAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// externalAccountFile writes an external_account credentials file whose subject token is in a
// file of its own, whose security token service is sts and whose service account to act as is
// robot@ at impersonation, and points GOOGLE_APPLICATION_CREDENTIALS at it.
func externalAccountFile(t *testing.T, sts, impersonation string) {
	t.Helper()
	dir := t.TempDir()
	subject := filepath.Join(dir, "subject")
	if err := os.WriteFile(subject, []byte("the.workload.credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:jwt",
		"token_url":                         sts + "/v1/token",
		"service_account_impersonation_url": impersonation + "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken",
		"credential_source":                 map[string]any{"file": subject},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
}

// impersonationStandIn is a service_account_impersonation_url that answers every call with the
// account's own access token, not-a-secret-robot-access-token, and counts them: a client that
// called it would spend that token on generateIdToken, and a row's bearer check names the cause.
func impersonationStandIn(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	base := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"not-a-secret-robot-access-token","expireTime":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	})
	return base, &calls
}

// federatedExchange is a security token service that answers every exchange with the federated
// access token not-a-secret-federated-access-token, and the form of the last exchange it was sent.
func federatedExchange(t *testing.T) (string, func() url.Values) {
	t.Helper()
	var mu sync.Mutex
	var last url.Values
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("the security token service read no form: %v", err)
		}
		mu.Lock()
		last = r.PostForm
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"not-a-secret-federated-access-token","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	})
	return base, func() url.Values {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// checkExchange fails t unless exchange carried subject as the subject token of type subjectType,
// for the cloud-platform scope the federated token is asked for.
func checkExchange(t *testing.T, exchange url.Values, subject, subjectType string) {
	t.Helper()
	for field, want := range map[string]string{"subject_token": subject, "subject_token_type": subjectType, "scope": poolScope} {
		if got := exchange.Get(field); got != want {
			t.Errorf("the security token service was sent %s %q; want %q", field, got, want)
		}
	}
}

const (
	// x509PoolProvider is the workload identity pool provider an X.509 credential in these rows
	// federates through.
	x509PoolProvider = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x509"
	// x509SubjectTokenType is the subject_token_type such a credential names: the security token
	// service reads the certificate, and Google's own endpoint the TLS client certificate.
	x509SubjectTokenType = "urn:ietf:params:oauth:token-type:mtls"
)

// x509Files are the files of an X.509 workload identity federation credential, by path, and the
// subject token the security token service must be sent for it: the certificate's DER, base64,
// in a JSON array.
type x509Files struct {
	credential, config, cert, key string
	subjectToken                  string
}

// writeX509Credential writes in dir the files `gcloud iam workload-identity-pools
// create-cred-config --credential-cert-path --credential-cert-configuration-output-file` leaves on
// a machine: a self-signed certificate and its key, the certificate configuration that names both,
// and an external_account credential whose credential_source names that configuration
// (certificate_config_location), whose security token service is sts and whose service account to
// act as is robot@ at impersonation.
func writeX509Credential(t *testing.T, dir, sts, impersonation string) x509Files {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "robot-machine"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	files := x509Files{
		credential: filepath.Join(dir, "credentials.json"), config: filepath.Join(dir, "certificate_config.json"),
		cert: filepath.Join(dir, "cert.pem"), key: filepath.Join(dir, "key.pem"),
	}
	subject, err := json.Marshal([]string{base64.StdEncoding.EncodeToString(der)})
	if err != nil {
		t.Fatal(err)
	}
	files.subjectToken = string(subject)
	config, err := json.Marshal(map[string]any{"cert_configs": map[string]any{"workload": map[string]any{"cert_path": files.cert, "key_path": files.key}}})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := json.Marshal(map[string]any{
		"type":                              "external_account",
		"audience":                          x509PoolProvider,
		"subject_token_type":                x509SubjectTokenType,
		"token_url":                         sts + "/v1/token",
		"service_account_impersonation_url": impersonation + "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken",
		"credential_source":                 map[string]any{"certificate": map[string]any{"certificate_config_location": files.config}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		files.cert:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		files.key:        pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		files.config:     config,
		files.credential: credential,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// reasons is each skipped source's reason, in order.
func reasons(skipped []Skipped) []string {
	out := make([]string, len(skipped))
	for i, s := range skipped {
		out[i] = s.Reason
	}
	return out
}

func TestMachineIdentityIsFoundWithNoSetting(t *testing.T) {
	t.Run("external_account", func(t *testing.T) {
		var mintPath, mintBearer string
		var mintRequest map[string]any
		sts, exchange := federatedExchange(t)
		impersonation, impersonated := impersonationStandIn(t)
		iam := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			mintPath, mintBearer = r.URL.Path, r.Header.Get("Authorization")
			mintRequest = readJSONBody(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"robot-identity-token"}`))
		})
		externalAccountFile(t, sts, impersonation)
		metadata := startDetectMetadata(t)

		source, skipped, err := DetectMachineIdentity(context.Background(), testAudience)
		if err != nil {
			t.Fatal(err)
		}
		external, ok := source.(*ExternalAccountIdentity)
		if !ok {
			t.Fatalf("source = %#v (skipped %v); want the external_account credential", source, reasons(skipped))
		}
		if external.ServiceAccount != "robot@example.iam.gserviceaccount.com" || external.Audience != testAudience {
			t.Fatalf("external_account identity acts as %q for %q; want robot@example.iam.gserviceaccount.com for %s", external.ServiceAccount, external.Audience, testAudience)
		}
		external.IAMCredentialsURL = iam
		token, tokenType, err := external.Token(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if token != "robot-identity-token" || tokenType != oauthwire.SubjectTokenTypeGoogle {
			t.Fatalf("Token = %q, %q; want the identity token generateIdToken minted, as a Google identity token", token, tokenType)
		}
		checkExchange(t, exchange(), "the.workload.credential", "urn:ietf:params:oauth:token-type:jwt")
		if want := "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateIdToken"; mintPath != want {
			t.Fatalf("generateIdToken was called at %s, want %s", mintPath, want)
		}
		// The federated token is spent on generateIdToken itself, as a workload identity pool's
		// is: the federation's principal holds roles/iam.workloadIdentityUser on the account,
		// which carries getOpenIdToken, and the account needs no role on itself.
		if mintBearer != "Bearer not-a-secret-federated-access-token" {
			t.Fatalf("generateIdToken was called with %q; want the federated access token the security token service answered", mintBearer)
		}
		if n := impersonated.Load(); n != 0 {
			t.Fatalf("the service_account_impersonation_url saw %d requests; want none: the account's own access token is never asked for", n)
		}
		if mintRequest["audience"] != testAudience || mintRequest["includeEmail"] != true {
			t.Fatalf("generateIdToken was asked %v; want audience %s and includeEmail", mintRequest, testAudience)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a credentials file found first asks it nothing", n)
		}
	})

	// The credential `gcloud iam workload-identity-pools create-cred-config` writes for an X.509
	// certificate: its credential_source names a certificate configuration, which names the
	// certificate and its key, and the security token service reads the certificate as the
	// subject token. The stand-in is plain http, so this row reads what the exchange sends;
	// x509tls_linux_test.go reads the certificate its TLS presents.
	t.Run("external_account of an X.509 certificate", func(t *testing.T) {
		sts, exchange := federatedExchange(t)
		impersonation, impersonated := impersonationStandIn(t)
		var mintBearer string
		iam := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			mintBearer = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"robot-identity-token"}`))
		})
		files := writeX509Credential(t, t.TempDir(), sts, impersonation)
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", files.credential)
		metadata := startDetectMetadata(t)

		source, skipped, err := DetectMachineIdentity(context.Background(), testAudience)
		if err != nil {
			t.Fatal(err)
		}
		external, ok := source.(*ExternalAccountIdentity)
		if !ok || external.ServiceAccount != "robot@example.iam.gserviceaccount.com" {
			t.Fatalf("source = %#v (skipped %v); want the X.509 external_account credential acting as robot@example.iam.gserviceaccount.com", source, reasons(skipped))
		}
		external.IAMCredentialsURL = iam
		token, tokenType, err := external.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if token != "robot-identity-token" || tokenType != oauthwire.SubjectTokenTypeGoogle {
			t.Fatalf("Token = %q, %q; want the identity token generateIdToken minted, as a Google identity token", token, tokenType)
		}
		checkExchange(t, exchange(), files.subjectToken, x509SubjectTokenType)
		if got := exchange().Get("audience"); got != x509PoolProvider {
			t.Fatalf("the security token service was sent audience %q; want the credential's pool provider, %s", got, x509PoolProvider)
		}
		if mintBearer != "Bearer not-a-secret-federated-access-token" {
			t.Fatalf("generateIdToken was called with %q; want the federated access token the security token service answered", mintBearer)
		}
		if n := impersonated.Load(); n != 0 {
			t.Fatalf("the service_account_impersonation_url saw %d requests; want none", n)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a credentials file found first asks it nothing", n)
		}
	})

	t.Run("credentials file does not parse", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
		metadata := startDetectMetadata(t)
		source, _, err := DetectMachineIdentity(context.Background(), testAudience)
		// The error names the variable and not its value: the file holds a credential, and for a
		// person who pasted the credential itself into the variable its path is the credential.
		if err == nil || source != nil || !strings.Contains(err.Error(), "GOOGLE_APPLICATION_CREDENTIALS") || strings.Contains(err.Error(), path) {
			t.Fatalf("DetectMachineIdentity = %#v, %v; want an error naming GOOGLE_APPLICATION_CREDENTIALS, which is present but broken, and not its path", source, err)
		}
		if n := metadata.requests.Load(); n != 0 {
			t.Fatalf("the metadata server saw %d requests; a broken credentials file stops before it", n)
		}
	})

	t.Run("metadata server", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		startDetectMetadata(t)
		source, skipped, err := DetectMachineIdentity(context.Background(), testAudience)
		if err != nil {
			t.Fatal(err)
		}
		if metadata, ok := source.(*MetadataIdentity); !ok || metadata.Audience != testAudience {
			t.Fatalf("source = %#v (skipped %v); want the metadata server's identity for %s", source, reasons(skipped), testAudience)
		}
	})

	t.Run("none", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
		t.Setenv("GCE_METADATA_HOST", closedAddress(t))
		start := time.Now()
		source, skipped, err := DetectMachineIdentity(context.Background(), testAudience)
		elapsed := time.Since(start)
		if err != nil || source != nil {
			t.Fatalf("DetectMachineIdentity = %#v, %v; want no identity and no error", source, err)
		}
		want := []string{"GOOGLE_APPLICATION_CREDENTIALS is unset", "no Google metadata server"}
		if got := reasons(skipped); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("skipped = %q; want %q", got, want)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("finding no identity took %s; a laptop must learn it has none within 2 s", elapsed)
		}
	})
}
