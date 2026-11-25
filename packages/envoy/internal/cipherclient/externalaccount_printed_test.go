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
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
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
	// x509Edited is the credential_source of an X.509 credential whose files are in a directory
	// named secret, after edit changed them; it names the trust chain file edit answers, if any.
	x509Edited := func(t *testing.T, edit func(t *testing.T, f x509Files) (chain string)) map[string]any {
		t.Helper()
		dir := filepath.Join(t.TempDir(), secret)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		files := writeX509Credential(t, dir, sts, "https://iamcredentials.invalid")
		certificate := map[string]any{"certificate_config_location": files.config}
		if chain := edit(t, files); chain != "" {
			certificate["trust_chain_path"] = chain
		}
		return map[string]any{"certificate": certificate}
	}
	// write puts content in the file at path; chainOf writes a trust chain of content beside the
	// certificate f names, and answers its path.
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	chainOf := func(t *testing.T, f x509Files, content func(cert string) string) string {
		t.Helper()
		cert, err := os.ReadFile(f.cert)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(f.cert), "chain.pem")
		write(t, path, content(string(cert)))
		return path
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
		// Google's library fails on each of these before it sends the exchange, or in its TLS
		// handshake, so each is named, never as "the request failed": a call that got no answer.
		{"a certificate that does not parse", func(t *testing.T) map[string]any {
			return x509Edited(t, func(t *testing.T, f x509Files) string { write(t, f.cert, "not a certificate "+secret); return "" })
		}, "the certificate its certificate configuration names is not a PEM certificate"},
		{"a private key that does not parse", func(t *testing.T) map[string]any {
			return x509Edited(t, func(t *testing.T, f x509Files) string { write(t, f.key, "not a key "+secret); return "" })
		}, "the private key its certificate configuration names is not the certificate's PEM private key"},
		{"a trust chain that does not parse", func(t *testing.T) map[string]any {
			return x509Edited(t, func(t *testing.T, f x509Files) string {
				return chainOf(t, f, func(string) string { return "-----BEGIN CERTIFICATE-----\n" + secret + "\n-----END CERTIFICATE-----\n" })
			})
		}, "the trust chain its credential_source names is not PEM certificates"},
		{"a trust chain that holds the certificate below its top", func(t *testing.T) map[string]any {
			return x509Edited(t, func(t *testing.T, f x509Files) string {
				return chainOf(t, f, func(cert string) string { return cert + cert })
			})
		}, "the trust chain its credential_source names holds the certificate below its top"},
		{"control: a trust chain whose top is the certificate", func(t *testing.T) map[string]any {
			return x509Edited(t, func(t *testing.T, f x509Files) string {
				return chainOf(t, f, func(cert string) string { return cert })
			})
		}, "the token service the credential names: the server answered HTTP 400 invalid_grant"},
		{"control: the token service refuses the exchange", func(t *testing.T) map[string]any {
			return map[string]any{"file": subjectTokenFile(t, "the.workload.credential")}
		}, "the token service the credential names: the server answered HTTP 400 invalid_grant"},
		// Google's library makes an X.509 certificate's exchange on its own client, which no
		// refusal recorder wraps, and quotes the answer in its error's text; this row fails when a
		// release of the library words that text differently (libraryAnswer).
		{"control: the token service refuses an X.509 certificate's exchange", func(t *testing.T) map[string]any {
			return x509Missing(t, nil, "")
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

// TestARefusalAtIAMCredentialsImpersonationNamesIt: the exchange's federated token of a credential
// with a subject token file is spent at its service_account_impersonation_url, IAM Credentials'
// generateAccessToken, and a refusal there names IAM Credentials, its status and its registered
// code, and never the rest of its body, as it did before Google's library read the file.
func TestARefusalAtIAMCredentialsImpersonationNamesIt(t *testing.T) {
	const secret = "hunter2pw-impersonation-4be7"
	sts, _ := federatedExchange(t)
	impersonation := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"echo: ` + secret + `","status":"PERMISSION_DENIED"}}`))
	})
	iam, _ := accountIAM(t)
	externalAccountFile(t, sts, "https://iamcredentials.googleapis.com")
	source, _, err := externalAccountFor(testAudience)
	if err != nil {
		t.Fatal(err)
	}
	identity := source.(*ExternalAccountIdentity)
	identity.IAMCredentialsURL = iam
	identity.HTTP = iamCredentialsAt(t, impersonation)
	_, _, err = identity.Token(context.Background())
	want := "mint an access token as robot@example.iam.gserviceaccount.com with the external_account credential " + credentialsFileVar +
		" names: IAM Credentials: the server answered HTTP 403 PERMISSION_DENIED"
	if err == nil || err.Error() != want {
		t.Fatalf("Token: %v; want %q", err, want)
	}
}

// TestOnlyTheLibrarysOwnQuoteIsReadAsAnAnswer: an X.509 certificate's exchange goes out on Google's
// library's own client, which no refusal recorder wraps, so a refusal is read from the library's
// error, and only where that error is, from its first word to its last, the library's quote of the
// security token service's answer (libraryAnswer), over as many lines as the body runs. An error
// worded otherwise, or one that quotes those words further in, prints as a request that failed and
// never as an answer nobody recorded. A credential that names no token_url is exchanged at the
// library's default, Google's security token service, and its refusal names that service.
func TestOnlyTheLibrarysOwnQuoteIsReadAsAnAnswer(t *testing.T) {
	const mtls = "https://sts.mtls.googleapis.com/v1/token"
	for _, row := range []struct{ name, tokenURL, text, want string }{
		{"the library's quote of a refusal", mtls, `credentials: status code 403: {"error":"access_denied","error_description":"echo"}`,
			"Google's security token service: the server answered HTTP 403 access_denied"},
		{"the library's quote of a body of many lines", mtls, "credentials: status code 400: {\n  \"error\": \"invalid_target\",\n  \"error_description\": \"echo\"\n}\n",
			"Google's security token service: the server answered HTTP 400 invalid_target"},
		{"a credential that names no token_url", "", `credentials: status code 403: {"error":"access_denied"}`,
			"Google's security token service: the server answered HTTP 403 access_denied"},
		{"control: a token_url of another service", "https://sts.example/v1/token", `credentials: status code 403: {"error":"access_denied"}`,
			"the token service the credential names: the server answered HTTP 403 access_denied"},
		{"another wording", mtls, `credentials: HTTP 403: {"error":"access_denied"}`, "the request failed"},
		{"an error that quotes the library's words further in", mtls, `a certificate parse error containing credentials: status code 403: {"error":"access_denied"}`,
			"the request failed"},
	} {
		t.Run(row.name, func(t *testing.T) {
			var fields credentialFields
			fields.TokenURL = row.tokenURL
			fields.Source.Certificate = &certificateSource{}
			if got := externalAccountFailure(fields, &oauthrefusal.Recorder{}, errors.New(row.text)); got != row.want {
				t.Fatalf("externalAccountFailure(%q) = %q; want %q", row.text, got, row.want)
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

// TestAnEnterpriseCertificateProxyConfigurationRunsNothing: a certificate configuration that Google's
// library reads as an enterprise certificate proxy's, with a signer to run (`libs.ecp`), is refused
// before the library reads it: the library would start that program with no setting that allows it,
// as an executable credential_source needs, and no time limit, and leave it running. So is one that
// names a workload certificate and key and then a null workload, in either case of its key: Go's
// JSON decoder keeps the first in a struct and takes the null in a pointer, which is how the library
// reads it, so the library sees no workload. Control: for each, the library alone, given the same
// credential, starts the signer.
func TestAnEnterpriseCertificateProxyConfigurationRunsNothing(t *testing.T) {
	for _, row := range []struct{ name, workload string }{
		{"an enterprise certificate proxy's configuration", ``},
		{"a workload, then a null workload", `"workload":{"cert_path":%q,"key_path":%q},"workload":null,`},
		{"a workload, then a null Workload", `"workload":{"cert_path":%q,"key_path":%q},"Workload":null,`},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			ran := filepath.Join(dir, "ran")
			signer := filepath.Join(dir, "signer")
			if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			files := writeX509Credential(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid")
			workload := row.workload
			if workload != "" {
				workload = fmt.Sprintf(workload, files.cert, files.key)
			}
			config := `{"cert_configs":{` + workload + `"pkcs11":{"module":"/nowhere","slot":1,"label":"key"}},"libs":{"ecp":"` + signer + `"}}`
			if err := os.WriteFile(files.config, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(files.credential)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data,
				&credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)})
			if err := os.Remove(ran); err != nil {
				t.Fatalf("control: Google's library did not start the signer the configuration names: %v", err)
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
		})
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
// transcript keeps: the exchange's request carries the subject token and its answer the
// federated access token, and the impersonation URL's request carries the federated access token
// and its answer the account's. A mint prints none of them, whatever that variable says. Control:
// the library's own mint of the same credential, with its default logger, prints each, so the
// probe sees the library's log at this variable.
func TestAnExternalAccountMintPrintsNothingWhenGoogleLibrariesLog(t *testing.T) {
	const subjectToken = "subject-token-externallog-5d1e"
	const federated = "federated-token-externallog-a07c"
	const account = "account-token-externallog-91b2"
	t.Setenv("GOOGLE_SDK_GO_LOGGING_LEVEL", "debug")
	sts := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + accessField + `":"` + federated + `","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600}`))
	})
	impersonation := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"` + account + `","expireTime":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	})
	iam := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"robot-identity-token"}`))
	})
	var credential map[string]any
	if err := json.Unmarshal(externalAccountJSON(t, sts, map[string]any{"file": subjectTokenFile(t, subjectToken)}), &credential); err != nil {
		t.Fatal(err)
	}
	credential["service_account_impersonation_url"] = impersonation + "/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken"
	data, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}

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
	for _, logged := range []string{subjectToken, federated, account} {
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
	for _, leaked := range []string{subjectToken, federated, account} {
		if strings.Contains(out, leaked) {
			t.Errorf("the mint printed %q to stderr:\n%s", leaked, out)
		}
	}
}
