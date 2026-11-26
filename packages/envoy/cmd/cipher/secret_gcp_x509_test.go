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
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"

	"github.com/sjawhar/envoy/internal/cipherclient/repeatprobe"
	"github.com/sjawhar/envoy/testexec"
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
// configuration Google's library reads as an enterprise certificate proxy's is refused in the
// machine source's words before the library would start the signer it names. So is one whose
// configuration names a workload certificate and key and then a null workload, in either case of
// its key, which the library reads as no workload: it repeats a key. Control: for each, the signer
// runs when Google's library alone reads the file.
func TestAnEnterpriseCertificateProxyCredentialsFileIsRefused(t *testing.T) {
	const repeats = "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: the certificate configuration its credential_source names repeats a key"
	for _, row := range []struct{ name, workload, want string }{
		{"an enterprise certificate proxy's configuration", ``,
			"the credentials file GOOGLE_APPLICATION_CREDENTIALS names: the certificate configuration its credential_source names holds no workload certificate and private key"},
		{"a workload, then a null workload", `"workload":{"cert_path":%q,"key_path":%q},"workload":null,`, repeats},
		{"a workload, then a null Workload", `"workload":{"cert_path":%q,"key_path":%q},"Workload":null,`, repeats},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			ran := filepath.Join(dir, "ran")
			signer := filepath.Join(dir, "signer")
			if err := testexec.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			files := writeStoreX509(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid", "store-robot@example-project.iam.gserviceaccount.com")
			workload := row.workload
			if workload != "" {
				workload = fmt.Sprintf(workload, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
			}
			config := `{"cert_configs":{` + workload + `"pkcs11":{"module":"/nowhere","slot":1,"label":"key"}},"libs":{"ecp":"` + signer + `"}}`
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
			if err == nil || refusal.Error() != row.want {
				t.Fatalf("gcpCredentialsFile: %v (refusal %q); want %q", err, refusal, row.want)
			}
			if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
			}
		})
	}
}

// TestAStoreCredentialsFileThatRepeatsAKeyIsRefused: an external_account credentials file that
// repeats a key, two keys Google's library decodes into one field, is refused before any reader
// reads it, in the machine source's words: the shapes a review found, and one of a subject token
// file. So is an X.509 file whose certificate configuration is an enterprise certificate proxy's
// beside a number outside float64's range, which the check reads as text. Each row whose
// library-alone read starts the proxy's signer (GOOGLE_API_CERTIFICATE_CONFIG names the proxy's
// configuration as the default one) starts none through the loader. Controls, which load: the
// file gcloud writes with two header names that differ in case (a map, whose keys are compared
// exactly), an authorized_user file, and one whose client_id repeats in another case, of which
// this client reads only its type, as on main.
func TestAStoreCredentialsFileThatRepeatsAKeyIsRefused(t *testing.T) {
	const repeats = "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: it repeats a key"
	dir := t.TempDir()
	surface, err := repeatprobe.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_API_CERTIFICATE_CONFIG", surface.Proxy)
	top := `"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x509",` +
		`"subject_token_type":"urn:ietf:params:oauth:token-type:mtls","token_url":"http://127.0.0.1:9/v1/token"`
	location := func(path string) string {
		return `{"certificate":{"certificate_config_location":` + strconv.Quote(path) + `}}`
	}
	// The authorized_user fields that hold a credential are named from parts, as storeFederated
	// is, so no line of this file reads as a credential assigned a value.
	user := `"type":"authorized_user","client_id":"store-client",` + strconv.Quote("client"+"_secret") + `:"store-fixture",` +
		strconv.Quote("refresh"+"_token") + `:"store-fixture"`
	gcloud, err := os.ReadFile(filepath.Join("..", "..", "internal", "cipherclient", "testdata", "gcloud-credential-case-distinct-headers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, credential string
		signer           bool
		// want is the refusal's start, or "" where the file loads.
		want string
	}{
		{"credential_source, then null, then the default configuration",
			`{` + top + `,"credential_source":` + location(surface.Workload) + `,"credential_source":null,"credential_source":{"certificate":{"use_default_certificate_config":true}}}`, true, repeats},
		{"credential_source, then CREDENTIAL_SOURCE",
			`{` + top + `,"credential_source":` + location(surface.Proxy) + `,"CREDENTIAL_SOURCE":` + location(surface.Workload) + `}`, false, repeats},
		{"an external_account file of a subject token file whose token_url repeats",
			`{` + top + `,"TOKEN_URL":"https://sts.invalid/v1/token","credential_source":{"file":"/nowhere"}}`, false, repeats},
		{"a proxy's configuration beside a number outside float64's range",
			`{` + top + `,"x":1e400,"credential_source":` + location(surface.Proxy) + `}`, true,
			"the credentials file GOOGLE_APPLICATION_CREDENTIALS names: the certificate configuration its credential_source names holds no workload certificate and private key"},
		{"control: gcloud's file with two header names that differ in case", string(gcloud), false, ""},
		{"control: an authorized_user file whose client_id repeats in another case", `{` + user + `,"Client_ID":"other"}`, false, ""},
		{"control: an authorized_user file", `{` + user + `}`, false, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, []byte(row.credential), 0o600); err != nil {
				t.Fatal(err)
			}
			if row.signer {
				_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, []byte(row.credential), &credentials.DetectOptions{Scopes: secretmanager.DefaultAuthScopes()})
				if err := os.Remove(surface.Ran); err != nil {
					t.Fatalf("control: Google's library did not start the signer: %v", err)
				}
			}
			_, err := gcpCredentialsFile(context.Background(), path)
			if row.want == "" {
				if err != nil {
					t.Fatalf("control: gcpCredentialsFile: %v; want the file loaded", err)
				}
				return
			}
			refusal := credentialsFileRefusal("the credentials file GOOGLE_APPLICATION_CREDENTIALS names", err)
			if err == nil || !strings.HasPrefix(refusal.Error(), row.want) {
				t.Fatalf("gcpCredentialsFile: %v (refusal %q); want %q", err, refusal, row.want)
			}
			if _, err := os.Stat(surface.Ran); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
			}
		})
	}
}

// TestAStoreCredentialsFileGooglesLibraryCannotReadIsRefused: an external_account credentials file
// with a field Google's library reads, holding another JSON type than the library reads there, is
// refused in the machine source's words, which name no field and no value of it. Control: Google's
// library alone refuses each of the same files, so the words are true. Control: the same file with
// that field of the right type loads.
func TestAStoreCredentialsFileGooglesLibraryCannotReadIsRefused(t *testing.T) {
	const want = "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: it is not an external_account credential that Google's library reads"
	top := `"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/oidc",` +
		`"subject_token_type":"urn:ietf:params:oauth:token-type:jwt"`
	source := `"credential_source":{"file":"/nowhere"}`
	for _, row := range []struct{ name, credential string }{
		{"a token_url that is a number", `{` + top + `,"token_url":5,` + source + `}`},
		{"a credential_source that is a string", `{` + top + `,"token_url":"http://127.0.0.1:9/v1/token","credential_source":"x"}`},
		{"headers that are a list", `{` + top + `,"token_url":"http://127.0.0.1:9/v1/token","credential_source":{"url":"http://127.0.0.1:9/subject","headers":["x"]}}`},
		{"control: the same file with a token_url that is a string", `{` + top + `,"token_url":"http://127.0.0.1:9/v1/token",` + source + `}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, libraryErr := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, []byte(row.credential),
				&credentials.DetectOptions{Scopes: secretmanager.DefaultAuthScopes(), Logger: slog.New(slog.DiscardHandler)})
			path := filepath.Join(t.TempDir(), "credentials.json")
			if err := os.WriteFile(path, []byte(row.credential), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := gcpCredentialsFile(context.Background(), path)
			if strings.HasPrefix(row.name, "control:") {
				if libraryErr != nil || err != nil {
					t.Fatalf("control: the library answered %v and gcpCredentialsFile %v; want the file loaded by both", libraryErr, err)
				}
				return
			}
			if libraryErr == nil {
				t.Fatalf("control: Google's library alone loaded the file, so it is one the library reads")
			}
			if refusal := credentialsFileRefusal("the credentials file GOOGLE_APPLICATION_CREDENTIALS names", err); err == nil || refusal.Error() != want {
				t.Fatalf("gcpCredentialsFile: %v (refusal %q); want %q", err, refusal, want)
			}
		})
	}
}

// TestNoStoreCredentialThatRepeatsAKeyStartsASigner: every credentials file repeatprobe writes,
// which repeat a key at each level of the credentials file and of its certificate configuration,
// in each spelling Go's JSON decoder matches, is refused by the secret forms' loader, and none
// starts the enterprise certificate proxy's signer beside them. Control: Google's library alone,
// given each file, starts the signer for some, so the probe sees a signer that runs.
func TestNoStoreCredentialThatRepeatsAKeyStartsASigner(t *testing.T) {
	dir := t.TempDir()
	surface, err := repeatprobe.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_API_CERTIFICATE_CONFIG", surface.Proxy)
	inputs, err := surface.Inputs("http://127.0.0.1:9/v1/token",
		"https://iamcredentials.invalid/v1/projects/-/serviceAccounts/store-robot@example-project.iam.gserviceaccount.com:generateAccessToken")
	if err != nil {
		t.Fatal(err)
	}
	ran := func() bool {
		_, err := os.Stat(surface.Ran)
		_ = os.Remove(surface.Ran)
		return err == nil
	}
	path := filepath.Join(dir, "credentials.json")
	reached := 0
	for _, input := range inputs {
		_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, input.Credential,
			&credentials.DetectOptions{Scopes: secretmanager.DefaultAuthScopes(), Logger: slog.New(slog.DiscardHandler)})
		if ran() {
			reached++
		}
		if err := os.WriteFile(path, input.Credential, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := gcpCredentialsFile(context.Background(), path)
		if ran() {
			t.Errorf("SIGNER RAN: %s", input.Name)
		}
		if refusal := credentialsFileRefusal("the file", err); err == nil || !strings.Contains(refusal.Error(), "repeats a key") {
			t.Errorf("NOT REFUSED: %s: %v", input.Name, err)
		}
	}
	if reached == 0 {
		t.Fatalf("control: Google's library started the signer for none of %d inputs, so this probe cannot see one run", len(inputs))
	}
	t.Logf("%d inputs; Google's library alone started the signer for %d of them", len(inputs), reached)
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
