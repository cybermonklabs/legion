// packages/envoy/internal/cipherclient/externalaccount_printed_test.go
//
// What an external_account identity prints of its audience and its account, in every trade's log
// line and error that names it: the audience (the broker's Google exchange audience, text a
// broker's settings answer wrote) only where it is a plain http(s) URL internal/redact prints, and
// the account only where it is a service account's email or unique id, the account the
// credentials file GOOGLE_APPLICATION_CREDENTIALS names otherwise.
package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/auth/credentials"
)

// TestAnExternalAccountIdentityPrintsItsAudienceAndAccountOnlyWhereTheyAreWhatTheyName: an
// external_account identity whose audience carries a credential, and whose account is no service
// account's, names neither in Describe, nor in the error of a mint the security token service or
// IAM Credentials refuses. Control: a plain audience and a service account's email print whole.
func TestAnExternalAccountIdentityPrintsItsAudienceAndAccountOnlyWhereTheyAreWhatTheyName(t *testing.T) {
	const secret = "hunter2pw-externalaccount-3f81a0"
	refusing := func(t *testing.T) string {
		return newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		})
	}
	trading := func(t *testing.T) string {
		return newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"` + accessField + `":"federated","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
		})
	}
	identity := func(t *testing.T, sts, audience, account string) *ExternalAccountIdentity {
		t.Helper()
		dir := t.TempDir()
		subject := filepath.Join(dir, "subject")
		if err := os.WriteFile(subject, []byte("the.workload.credential"), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{
			"type":               "external_account",
			"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
			"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
			"token_url":          sts + "/v1/token",
			"credential_source":  map[string]any{"file": subject},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: account, Audience: audience, HTTP: HTTPClient(), IAMCredentialsURL: refusing(t)}
	}
	for _, row := range []struct {
		name, audience, account string
		want                    []string
	}{
		{"an audience with a user and a password", "https://tokuser9f3:" + secret + "@aud.example", "fleet-robot@example-project.iam.gserviceaccount.com",
			[]string{"the Google exchange audience the broker names", "fleet-robot@example-project.iam.gserviceaccount.com"}},
		{"an account that is no service account's", "https://aud.example", secret,
			[]string{"https://aud.example", "the service account " + credentialsFileVar + " names (not printed"}},
		{"control: a plain audience and a service account's email", "https://aud.example", "fleet-robot@example-project.iam.gserviceaccount.com",
			[]string{"https://aud.example", "fleet-robot@example-project.iam.gserviceaccount.com"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			refused := identity(t, refusing(t), row.audience, row.account)
			_, _, stsErr := refused.Token(context.Background())
			minted := identity(t, trading(t), row.audience, row.account)
			_, _, iamErr := minted.Token(context.Background())
			if stsErr == nil || iamErr == nil {
				t.Fatalf("Token = %v, %v; want the refusals of the security token service and of IAM Credentials", stsErr, iamErr)
			}
			for what, text := range map[string]string{"Describe": refused.Describe(), "the security token service's refusal": stsErr.Error(), "IAM Credentials' refusal": iamErr.Error()} {
				for _, leaked := range []string{secret, "tokuser9f3"} {
					if strings.Contains(text, leaked) {
						t.Errorf("%s %q holds %q", what, text, leaked)
					}
				}
			}
			for _, want := range row.want {
				if !strings.Contains(refused.Describe(), want) {
					t.Errorf("Describe %q does not say %q", refused.Describe(), want)
				}
			}
			if account := row.want[len(row.want)-1]; !strings.Contains(stsErr.Error()+iamErr.Error(), account) {
				t.Errorf("the refusals %q and %q do not name the account as %q", stsErr, iamErr, account)
			}
		})
	}
}

// TestAnExternalAccountMintsFailureSaysWhatFailed: an external_account credential whose exchange
// fails says which step failed in words of this client's own: a subject token file its
// credential_source names that cannot be read, or an X.509 credential's trust chain, certificate
// configuration, certificate or private key, prints the file call's reason, never its path; a
// refusal of the token service names that service and the registered code its answer carried,
// never the rest of the body. Control: the registered code prints, so the check sees the answer.
func TestAnExternalAccountMintsFailureSaysWhatFailed(t *testing.T) {
	const secret = "hunter2pw-externalmint-62c0"
	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"echo: ` + secret + `"}`))
	})
	// x509Missing is the credential_source of an X.509 credential whose files are in a directory
	// named secret, less the one missing names; chain names a trust chain file, which is never
	// written, where it is set.
	x509Missing := func(t *testing.T, missing func(x509Files) string, chain string) map[string]any {
		t.Helper()
		dir := filepath.Join(t.TempDir(), secret)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		files := writeX509Credential(t, dir, sts, "https://iamcredentials.invalid")
		if missing != nil {
			if err := os.Remove(missing(files)); err != nil {
				t.Fatal(err)
			}
		}
		certificate := map[string]any{"certificate_config_location": files.config}
		if chain != "" {
			certificate["trust_chain_path"] = filepath.Join(dir, chain)
		}
		return map[string]any{"certificate": certificate}
	}
	// x509Configured is the credential_source of an X.509 credential whose files are in a
	// directory named secret, and whose certificate configuration holds config.
	x509Configured := func(t *testing.T, config string) map[string]any {
		t.Helper()
		source := x509Missing(t, nil, "")
		path := source["certificate"].(map[string]any)["certificate_config_location"].(string)
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		return source
	}
	for _, row := range []struct {
		name   string
		source func(t *testing.T) map[string]any
		want   string
	}{
		{"a subject token file that is not there", func(t *testing.T) map[string]any {
			return map[string]any{"file": filepath.Join(t.TempDir(), secret)}
		}, "read the subject token file its credential_source names: open: no such file or directory"},
		{"a subject token file that is a directory", func(t *testing.T) map[string]any {
			dir := filepath.Join(t.TempDir(), secret)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			return map[string]any{"file": dir}
		}, "read the subject token file its credential_source names: read: is a directory"},
		{"a credential_source of no kind", func(t *testing.T) map[string]any {
			return map[string]any{}
		}, "the external_account credential " + credentialsFileVar + " names is not one this client reads"},
		{"a subject token url that refuses", func(t *testing.T) map[string]any {
			source := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"echo: ` + secret + `"}`))
			})
			return map[string]any{"url": source + "/token"}
		}, "the subject token source its credential_source names: the server answered HTTP 403 access_denied"},
		{"a certificate configuration that is not there", func(t *testing.T) map[string]any {
			return x509Missing(t, func(f x509Files) string { return f.config }, "")
		}, "read the certificate configuration its credential_source names: open: no such file or directory"},
		{"a certificate configuration that is not JSON", func(t *testing.T) map[string]any {
			return x509Configured(t, "not json "+secret)
		}, "the certificate configuration its credential_source names is not JSON"},
		{"a certificate configuration that names no private key", func(t *testing.T) map[string]any {
			return x509Configured(t, `{"cert_configs":{"workload":{"cert_path":"/`+secret+`"}}}`)
		}, "the certificate configuration its credential_source names holds no workload certificate and private key"},
		{"a trust chain that is not there", func(t *testing.T) map[string]any {
			return x509Missing(t, nil, "chain.pem")
		}, "read the trust chain its credential_source names: open: no such file or directory"},
		{"a certificate that is not there", func(t *testing.T) map[string]any {
			return x509Missing(t, func(f x509Files) string { return f.cert }, "")
		}, "read the certificate its certificate configuration names: open: no such file or directory"},
		{"a private key that is not there", func(t *testing.T) map[string]any {
			return x509Missing(t, func(f x509Files) string { return f.key }, "")
		}, "read the private key its certificate configuration names: open: no such file or directory"},
		{"control: the token service refuses the exchange", func(t *testing.T) map[string]any {
			return map[string]any{"file": subjectTokenFile(t, "the.workload.credential")}
		}, "the token service the credential names: the server answered HTTP 400 invalid_grant"},
	} {
		t.Run(row.name, func(t *testing.T) {
			identity := &ExternalAccountIdentity{CredentialsJSON: externalAccountJSON(t, sts, row.source(t)), ServiceAccount: "deploy-bot@example-project.iam.gserviceaccount.com",
				Audience: "https://aud.example", HTTP: HTTPClient()}
			_, _, err := identity.Token(context.Background())
			if err == nil {
				t.Fatal("the exchange minted a token")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "error_description") {
				t.Errorf("the error %q holds the path or the answer's body", err)
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("the error %q does not say %q", err, row.want)
			}
		})
	}
}

// externalAccountJSON is an external_account credential whose security token service is sts and
// whose subject token comes from source, with no service account to act as.
func externalAccountJSON(t *testing.T, sts string, source map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          sts + "/v1/token",
		"credential_source":  source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// subjectTokenFile is a file, in a directory of its own, holding token: the subject token of a
// credential_source of the file kind.
func subjectTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subject")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAnEnterpriseCertificateProxyConfigurationRunsNothing: a certificate configuration with no
// workload certificate and key, but an enterprise certificate proxy's signer to run (`libs.ecp`),
// is refused before Google's library reads it: the library would start that program with no
// setting that allows it, as an executable credential_source needs, and no time limit, and leave
// it running. Control: the signer the configuration names runs when started.
func TestAnEnterpriseCertificateProxyConfigurationRunsNothing(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	signer := filepath.Join(dir, "signer")
	if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := writeX509Credential(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid")
	config := `{"cert_configs":{"pkcs11":{"module":"/nowhere","slot":1,"label":"key"}},"libs":{"ecp":"` + signer + `"}}`
	if err := os.WriteFile(files.config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(signer).CombinedOutput(); err != nil {
		t.Fatalf("control: the signer did not run: %v %s", err, out)
	}
	if err := os.Remove(ran); err != nil {
		t.Fatalf("control: the signer ran and left no mark: %v", err)
	}

	data, err := os.ReadFile(files.credential)
	if err != nil {
		t.Fatal(err)
	}
	identity := &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: "robot@example.iam.gserviceaccount.com",
		Audience: testAudience, HTTP: HTTPClient()}
	_, _, err = identity.Token(context.Background())
	if want := "the certificate configuration its credential_source names holds no workload certificate and private key"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Token: %v; want %q", err, want)
	}
	if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
	}
}

// captureStderr is what fn wrote to os.Stderr. A file has no buffer for a long log to fill, as a
// pipe does; os.Stderr is restored however fn ends.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	captured, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = captured
	func() {
		defer func() { os.Stderr = stderr }()
		fn()
	}()
	captured.Close()
	printed, err := os.ReadFile(captured.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(printed)
}

// TestAnExternalAccountMintPrintsNothingWhenGoogleLibrariesLog: GOOGLE_SDK_GO_LOGGING_LEVEL makes
// Google's Go libraries log each request and answer with its body to stderr, which a session's
// transcript keeps, and the exchange's request carries the subject token and its answer the
// federated access token. A mint prints neither, whatever that variable says. Control: the
// library's own mint of the same credential, with its default logger, prints both, so the probe
// sees the library's log at this variable.
func TestAnExternalAccountMintPrintsNothingWhenGoogleLibrariesLog(t *testing.T) {
	const subjectToken = "subject-token-externallog-5d1e"
	const federated = "federated-token-externallog-a07c"
	t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + accessField + `":"` + federated + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	})
	iam := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"robot-identity-token"}`))
	})
	data := externalAccountJSON(t, sts, map[string]any{"file": subjectTokenFile(t, subjectToken)})

	control := captureStderr(t, func() {
		creds, err := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data,
			&credentials.DetectOptions{Scopes: []string{poolScope}, Client: HTTPClient()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := creds.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	for _, logged := range []string{subjectToken, federated} {
		if !strings.Contains(control, logged) {
			t.Fatalf("control: the library's own mint at debug printed no %q, so this probe cannot see its log:\n%s", logged, control)
		}
	}

	identity := &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: "robot@example.iam.gserviceaccount.com",
		Audience: "https://aud.example", HTTP: HTTPClient(), IAMCredentialsURL: iam}
	var token string
	var mintErr error
	out := captureStderr(t, func() { token, _, mintErr = identity.Token(context.Background()) })
	if mintErr != nil || token != "robot-identity-token" {
		t.Fatalf("Token = %q, %v; want the identity token", token, mintErr)
	}
	for _, leaked := range []string{subjectToken, federated} {
		if strings.Contains(out, leaked) {
			t.Errorf("the mint printed %q to stderr:\n%s", leaked, out)
		}
	}
}
