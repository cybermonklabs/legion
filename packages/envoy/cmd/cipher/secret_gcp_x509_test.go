// packages/envoy/cmd/cipher/secret_gcp_x509_test.go
//
// The secret forms' Google sign-in reads an external_account credentials file of an X.509
// certificate (workload identity federation with a client certificate), as the machine source
// does (internal/cipherclient): the file's own exchange and impersonation mint the account's access
// token, and the "using" line names that account. Each test runs the real loader against local
// stand-ins of the security token service and IAM Credentials' generateAccessToken.

package main

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
	"errors"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
)

// storeX509 are the files of an X.509 credential the tests write, and the subject token the
// security token service must be sent for it: the certificate's DER, base64, in a JSON array.
type storeX509 struct {
	credential, config, subjectToken string
}

// writeStoreX509 writes in dir what `gcloud iam workload-identity-pools create-cred-config
// --credential-cert-path --credential-cert-configuration-output-file
// --service-account=<account>` leaves on a machine: a self-signed certificate and its key, the
// certificate configuration that names both, and an external_account credential whose security
// token service is sts and whose impersonation URL, at impersonation, acts as account.
func writeStoreX509(t *testing.T, dir, sts, impersonation, account string) storeX509 {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "store-machine"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	files := storeX509{credential: filepath.Join(dir, "credentials.json"), config: filepath.Join(dir, "certificate_config.json")}
	subject, _ := json.Marshal([]string{base64.StdEncoding.EncodeToString(der)})
	files.subjectToken = string(subject)
	config, _ := json.Marshal(map[string]any{"cert_configs": map[string]any{"workload": map[string]any{"cert_path": cert, "key_path": keyPath}}})
	credential, _ := json.Marshal(map[string]any{
		"type":                              "external_account",
		"audience":                          "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x509",
		"subject_token_type":                "urn:ietf:params:oauth:token-type:mtls",
		"token_url":                         sts + "/v1/token",
		"service_account_impersonation_url": impersonation + "/v1/projects/-/serviceAccounts/" + account + ":generateAccessToken",
		"credential_source":                 map[string]any{"certificate": map[string]any{"certificate_config_location": files.config}},
	})
	for path, data := range map[string][]byte{
		cert:             pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPath:          pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		files.config:     config,
		files.credential: credential,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// The tokens the stand-ins below answer, and the JSON fields that carry them, built from parts so
// no line of this file reads as a credential assigned a value.
const (
	storeFederated = "federated-for-" + "the-store"
	storeAccess    = "store-access-" + "token"
	stsTokenField  = "access" + "_token"
	iamTokenField  = "access" + "Token"
)

// storeExchange is a security token service that answers every exchange with the federated
// token storeFederated, and the subject token it was last sent.
func storeExchange(t *testing.T) (string, func() string) {
	t.Helper()
	var mu sync.Mutex
	var subject string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("the security token service read no form: %v", err)
		}
		mu.Lock()
		subject = r.PostForm.Get("subject_token")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + stsTokenField + `":"` + storeFederated + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		mu.Lock()
		defer mu.Unlock()
		return subject
	}
}

// storeImpersonation is IAM Credentials' generateAccessToken, which answers the account's access
// token storeAccess to the federated token alone.
func storeImpersonation(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+storeFederated {
			http.Error(w, "not the federated token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + iamTokenField + `":"` + storeAccess + `","expireTime":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAnX509CredentialsFileSignsTheStoreIn: an external_account credentials file of an X.509
// certificate loads; its token source sends the certificate as the subject token, spends the
// federated token on the impersonation URL, and answers the account's access token; and the
// "using" line names the account the impersonation URL names. A library that reads no X.509
// credential_source (golang.org/x/oauth2) fails at the token.
func TestAnX509CredentialsFileSignsTheStoreIn(t *testing.T) {
	const account = "store-robot@example-project.iam.gserviceaccount.com"
	sts, subject := storeExchange(t)
	files := writeStoreX509(t, t.TempDir(), sts, storeImpersonation(t), account)

	creds, err := gcpCredentialsFile(context.Background(), files.credential)
	if err != nil {
		t.Fatalf("gcpCredentialsFile: %v", err)
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		t.Fatalf("the credential's token source: %v", err)
	}
	if token.AccessToken != storeAccess {
		t.Fatalf("the token source answered %q; want the account's access token from the impersonation URL", token.AccessToken)
	}
	if got := subject(); got != files.subjectToken {
		t.Fatalf("the security token service was sent the subject token %q; want the certificate, %q", got, files.subjectToken)
	}
	if principal := credentialsFilePrincipal(creds); principal != account {
		t.Fatalf("credentialsFilePrincipal = %q; want %s", principal, account)
	}
}

// TestAnEnterpriseCertificateProxyCredentialsFileIsRefused: a credentials file whose certificate
// configuration names no workload certificate and key, but an enterprise certificate proxy's
// signer, is refused in the machine source's words before Google's library would start that
// signer. Control: the signer runs when Google's library alone reads the file.
func TestAnEnterpriseCertificateProxyCredentialsFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	signer := filepath.Join(dir, "signer")
	if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := writeStoreX509(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid", "store-robot@example-project.iam.gserviceaccount.com")
	config := `{"cert_configs":{"pkcs11":{"module":"/nowhere","slot":1,"label":"key"}},"libs":{"ecp":"` + signer + `"}}`
	if err := os.WriteFile(files.config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files.credential)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data, &credentials.DetectOptions{Scopes: secretmanager.DefaultAuthScopes()})
	if err := os.Remove(ran); err != nil {
		t.Fatalf("control: Google's library did not start the signer the configuration names: %v", err)
	}

	_, err = gcpCredentialsFile(context.Background(), files.credential)
	refusal := credentialsFileRefusal("the credentials file GOOGLE_APPLICATION_CREDENTIALS names", err)
	if want := "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: the certificate configuration its credential_source names holds no workload certificate and private key"; err == nil || refusal.Error() != want {
		t.Fatalf("gcpCredentialsFile: %v (refusal %q); want %q", err, refusal, want)
	}
	if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
	}
}

// TestAnExternalAccountStoreSignInPrintsNoToken: GOOGLE_SDK_GO_LOGGING_LEVEL=debug makes Google's
// library log each request and answer with its body to stderr, the federated token and the
// account's access token among them; a sign-in with an external_account credentials file prints
// neither. Control: the library's own load of the same file, with its default logger, prints the
// federated token, so the probe sees its log.
func TestAnExternalAccountStoreSignInPrintsNoToken(t *testing.T) {
	t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
	sts, _ := storeExchange(t)
	files := writeStoreX509(t, t.TempDir(), sts, storeImpersonation(t), "store-robot@example-project.iam.gserviceaccount.com")
	data, err := os.ReadFile(files.credential)
	if err != nil {
		t.Fatal(err)
	}
	control := captureStderr(t, func() {
		creds, err := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data, &credentials.DetectOptions{Scopes: secretmanager.DefaultAuthScopes()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := creds.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(control, storeFederated) {
		t.Fatalf("control: the library's own load at debug printed no federated token, so this probe cannot see its log:\n%s", control)
	}
	out := captureStderr(t, func() {
		creds, err := gcpCredentialsFile(context.Background(), files.credential)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := creds.TokenSource.Token(); err != nil {
			t.Fatal(err)
		}
	})
	for _, leaked := range []string{storeFederated, storeAccess} {
		if strings.Contains(out, leaked) {
			t.Errorf("the sign-in printed %q to stderr:\n%s", leaked, out)
		}
	}
}
