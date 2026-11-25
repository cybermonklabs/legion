// packages/envoy/internal/cipherclient/externalaccount_repeated_test.go
//
// A credentials file or a certificate configuration that repeats a key is refused before anything
// reads it. Go's JSON decoder matches a key without case and keeps the last of the keys that match,
// and what "keeps" means differs by the type a reader decodes into: a null leaves a struct as it
// was but sets a pointer to nil, and a map keeps each spelling. So two readers of one such file,
// this client's check and Google's library, can read two different credentials. Each row runs the
// real reader against local stand-ins.

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/auth/credentials"

	"github.com/sjawhar/envoy/internal/cipherclient/repeatprobe"
)

// TestACredentialThatRepeatsAKeyIsRefused: an X.509 credential that repeats a key, exactly or in
// another case, at its top level or deeper, or whose certificate configuration does, mints nothing,
// and the refusal names the file that repeats it. Control: the same credential with no key
// repeated mints.
func TestACredentialThatRepeatsAKeyIsRefused(t *testing.T) {
	const credentialRepeats = "the credentials file " + credentialsFileVar + " names repeats a key"
	const configRepeats = "the certificate configuration its credential_source names repeats a key"
	for _, row := range []struct {
		name string
		// edit changes the credential's bytes, or the certificate configuration's at path.
		edit func(t *testing.T, credential string, f x509Files) string
		want string
	}{
		{"control: no key repeated", func(_ *testing.T, credential string, _ x509Files) string {
			return credential
		}, ""},
		{"a top-level key repeated exactly", func(_ *testing.T, credential string, _ x509Files) string {
			return strings.Replace(credential, `{`, `{"token_url":"https://sts.invalid/v1/token",`, 1)
		}, credentialRepeats},
		{"a top-level key repeated in another case", func(_ *testing.T, credential string, _ x509Files) string {
			return strings.Replace(credential, `{`, `{"TYPE":"external_account",`, 1)
		}, credentialRepeats},
		// encoding/json folds ſ (U+017F) to S, as strings.EqualFold does.
		{"a top-level key repeated in a Unicode case fold", func(_ *testing.T, credential string, _ x509Files) string {
			return strings.Replace(credential, `{`, `{"credential_ſource":null,`, 1)
		}, credentialRepeats},
		{"a key of the credential_source repeated in another case", func(_ *testing.T, credential string, f x509Files) string {
			return strings.Replace(credential, `"certificate":{`, `"certificate":{"Certificate_Config_Location":"`+f.config+`",`, 1)
		}, credentialRepeats},
		{"a key of the certificate configuration repeated in another case", func(t *testing.T, credential string, f x509Files) string {
			rewrite(t, f.config, `{"cert_configs":{"workload":{"cert_path":"`+f.cert+`","key_path":"`+f.key+`"},"WORKLOAD":{"cert_path":"`+f.cert+`","key_path":"`+f.key+`"}}}`)
			return credential
		}, configRepeats},
		{"a key of the certificate configuration's workload repeated exactly", func(t *testing.T, credential string, f x509Files) string {
			rewrite(t, f.config, `{"cert_configs":{"workload":{"cert_path":"`+f.cert+`","cert_path":"`+f.cert+`","key_path":"`+f.key+`"}}}`)
			return credential
		}, configRepeats},
	} {
		t.Run(row.name, func(t *testing.T) {
			sts, _ := federatedExchange(t)
			impersonation, _ := impersonationStandIn(t)
			iam, _ := federationIAM(t)
			files := writeX509Credential(t, t.TempDir(), sts, impersonation)
			data, err := os.ReadFile(files.credential)
			if err != nil {
				t.Fatal(err)
			}
			identity := &ExternalAccountIdentity{CredentialsJSON: []byte(row.edit(t, string(data), files)),
				ServiceAccount: "robot@example.iam.gserviceaccount.com", Audience: testAudience, HTTP: HTTPClient(), IAMCredentialsURL: iam}
			token, _, err := identity.Token(context.Background())
			switch {
			case row.want == "" && (err != nil || token != "robot-identity-token"):
				t.Fatalf("control: Token = %q, %v; want the identity token", token, err)
			case row.want != "" && (err == nil || !strings.Contains(err.Error(), row.want)):
				t.Fatalf("Token = %q, %v; want %q", token, err, row.want)
			case row.want != "" && strings.Contains(err.Error(), files.config):
				t.Fatalf("Token: %v names the configuration's path", err)
			}
		})
	}
}

// TestACredentialSourceThatNamesBothConfigurationsIsRefused: a certificate source that names a
// certificate configuration and asks for the default one too is one Google's library refuses, so
// this client refuses it before reading either, with a reason of its own.
func TestACredentialSourceThatNamesBothConfigurationsIsRefused(t *testing.T) {
	files := writeX509Credential(t, t.TempDir(), "http://127.0.0.1:9", "https://iamcredentials.invalid")
	original, err := os.ReadFile(files.credential)
	if err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(string(original), `"certificate":{`, `"certificate":{"use_default_certificate_config":true,`, 1)
	if data == string(original) {
		t.Fatalf("the credential %s names no certificate to add the default configuration to", original)
	}
	identity := &ExternalAccountIdentity{CredentialsJSON: []byte(data), ServiceAccount: "robot@example.iam.gserviceaccount.com", Audience: testAudience, HTTP: HTTPClient()}
	_, _, err = identity.Token(context.Background())
	if want := "its credential_source names a certificate configuration and asks for the default one too"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Token: %v; want %q", err, want)
	}
}

// TestACredentialsFileThatRepeatsAKeyRunsNoSigner: the two credentials files a review found read
// one way by this client's check and another by Google's library, each beside an enterprise
// certificate proxy's configuration whose signer only marks that it ran. Each is refused, and the
// signer does not run. Control: for each, the library alone, given the bytes this client used to
// give it, starts the signer.
//
//   - credential_source with a certificate configuration of the proxy's, then CREDENTIAL_SOURCE
//     with a workload one: the check took the last, and the copy without the impersonation URL,
//     written from a map, put CREDENTIAL_SOURCE first, so the library took the proxy's.
//   - credential_source with a workload configuration, then null, then one asking for the default
//     configuration, which GOOGLE_API_CERTIFICATE_CONFIG names as the proxy's: the null left the
//     check's struct as it was, and set the library's pointer to nil before the last.
func TestACredentialsFileThatRepeatsAKeyRunsNoSigner(t *testing.T) {
	for _, row := range []struct {
		name string
		// sources is the credential_source keys and values, in order, given the proxy's
		// configuration and a workload one.
		sources func(ecp, workload string) string
		// stripped is whether the control gives the library the copy without the impersonation URL.
		stripped bool
		// defaultIsProxy points GOOGLE_API_CERTIFICATE_CONFIG at the proxy's configuration.
		defaultIsProxy bool
	}{
		{"credential_source, then CREDENTIAL_SOURCE", func(ecp, workload string) string {
			return `"credential_source":{"certificate":{"certificate_config_location":"` + ecp + `"}},"CREDENTIAL_SOURCE":{"certificate":{"certificate_config_location":"` + workload + `"}}`
		}, true, false},
		{"credential_source, then null, then the default configuration", func(_, workload string) string {
			return `"credential_source":{"certificate":{"certificate_config_location":"` + workload + `"}},"credential_source":null,"credential_source":{"certificate":{"use_default_certificate_config":true}}`
		}, false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			files := writeX509Credential(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid")
			ecp, ran := proxyConfiguration(t, dir)
			if row.defaultIsProxy {
				t.Setenv(certificateConfigVar, ecp)
			}
			data, err := os.ReadFile(files.credential)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, "credential_source")
			bare, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			credential := strings.TrimSuffix(string(bare), "}") + "," + row.sources(ecp, files.config) + "}"

			control := []byte(credential)
			if row.stripped {
				if control, err = withoutImpersonation(control); err != nil {
					t.Fatal(err)
				}
			}
			_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, control,
				&credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)})
			if err := os.Remove(ran); err != nil {
				t.Fatalf("control: Google's library did not start the signer: %v", err)
			}

			identity := &ExternalAccountIdentity{CredentialsJSON: []byte(credential), ServiceAccount: "robot@example.iam.gserviceaccount.com",
				Audience: testAudience, HTTP: HTTPClient()}
			_, _, err = identity.Token(context.Background())
			if want := "repeats a key"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Token: %v; want %q", err, want)
			}
			if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
			}
		})
	}
}

// TestAMachineCredentialsFileThatRepeatsAKeyIsAnError: the machine source finds no identity in a
// credentials file that repeats a key, and stops there, as for a file that is not JSON: the
// error names the variable and not the path, and the metadata server is not asked.
func TestAMachineCredentialsFileThatRepeatsAKeyIsAnError(t *testing.T) {
	externalAccountFile(t, "http://127.0.0.1:9", "https://iamcredentials.invalid")
	path := os.Getenv(credentialsFileVar)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewrite(t, path, strings.Replace(string(data), `{`, `{"Type":"external_account",`, 1))
	metadata := startDetectMetadata(t)
	source, _, err := DetectMachineIdentity(context.Background(), testAudience)
	if err == nil || source != nil || !strings.Contains(err.Error(), credentialsFileVar+" names repeats a key") || strings.Contains(err.Error(), path) {
		t.Fatalf("DetectMachineIdentity = %#v, %v; want an error that the file repeats a key, naming %s and not its path", source, err, credentialsFileVar)
	}
	if n := metadata.requests.Load(); n != 0 {
		t.Fatalf("the metadata server saw %d requests; a broken credentials file stops before it", n)
	}
}

// TestARepeatedKeyIsFoundAtAnyDepthInAnyCase: repeatsKey finds two keys of one object that Go's
// JSON decoder reads as one, as encoding/json folds them (strings.EqualFold), wherever the object
// is. Controls: one key in two objects, or at two depths, repeats nothing.
func TestARepeatedKeyIsFoundAtAnyDepthInAnyCase(t *testing.T) {
	for _, row := range []struct {
		name, data string
		repeats    bool
	}{
		{"a key repeated exactly", `{"a":1,"a":2}`, true},
		{"a key repeated in another case", `{"type":"x","TYPE":"y"}`, true},
		{"a key repeated with ſ for s", `{"credential_source":1,"credential_ſource":2}`, true},
		{"a key repeated with the Kelvin sign for k", `{"key_path":1,"\u212aey_path":2}`, true},
		{"a key repeated through an escape", `{"a":1,"\u0041":2}`, true},
		{"a key repeated after a null", `{"a":{"b":1},"a":null}`, true},
		{"a key repeated in a nested object", `{"a":{"b":{"c":1,"C":2}}}`, true},
		{"a key repeated in an object in an array", `{"a":[1,{"c":1,"c":2}]}`, true},
		{"a key repeated after a nested object", `{"a":{"x":[{}]},"b":[],"A":1}`, true},
		{"control: one key in two objects", `{"a":{"k":1},"b":{"k":2}}`, false},
		{"control: one key at two depths", `{"k":{"k":{"K":1}}}`, false},
		{"control: keys that differ by more than case", `{"credential_source":1,"credential_sources":2}`, false},
		{"control: a value that repeats a key's text", `{"a":"a","b":["a","a"]}`, false},
		{"control: no object", `[1,"a",null]`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			repeats, err := repeatsKey([]byte(row.data))
			if err != nil || repeats != row.repeats {
				t.Fatalf("repeatsKey(%s) = %t, %v; want %t", row.data, repeats, err, row.repeats)
			}
		})
	}
	for _, data := range []string{``, `{`, `{"a":1`, `{"a" 1}`, `{} {}`, `{}x`} {
		if _, err := repeatsKey([]byte(data)); err == nil {
			t.Errorf("repeatsKey(%q) answered no error; want one, as for data that is not one JSON value", data)
		}
	}
}

// TestACredentialSourceReadsAsGooglesLibraryReadsIt: credentialFields decodes credential_source
// as the pointer Google's library decodes it into, so a null drops what came before it, as the
// library's does, and certificateConfig its workload the same way.
func TestACredentialSourceReadsAsGooglesLibraryReadsIt(t *testing.T) {
	fields, err := readCredentialFields([]byte(`{"credential_source":{"certificate":{"certificate_config_location":"/workload.json"}},` +
		`"credential_source":null,"credential_source":{"certificate":{"use_default_certificate_config":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if certificate := fields.certificate(); certificate == nil || certificate.Config != "" || !certificate.UseDefault {
		t.Fatalf("the certificate source read %+v; want the default configuration and no location, as the library reads it", certificate)
	}
	var config certificateConfig
	if err := json.Unmarshal([]byte(`{"cert_configs":{"workload":{"cert_path":"/c","key_path":"/k"},"workload":null}}`), &config); err != nil {
		t.Fatal(err)
	}
	if config.CertConfigs.Workload != nil {
		t.Fatalf("the workload read %+v; want none, as the library reads it", config.CertConfigs.Workload)
	}
}

// TestNoCredentialThatRepeatsAKeyStartsASigner: every credentials file repeatprobe writes, which
// repeat a key at each level of the credentials file and of its certificate configuration, in each
// spelling Go's JSON decoder matches, is refused as one that repeats a key, and none starts the
// enterprise certificate proxy's signer beside them. Control: Google's library alone, given each
// file and the copy of it without the impersonation URL, starts the signer for some, so the probe
// sees a signer that runs.
func TestNoCredentialThatRepeatsAKeyStartsASigner(t *testing.T) {
	surface, err := repeatprobe.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(certificateConfigVar, surface.Proxy)
	inputs, err := surface.Inputs("http://127.0.0.1:9/v1/token",
		"https://iamcredentials.invalid/v1/projects/-/serviceAccounts/robot@example.iam.gserviceaccount.com:generateAccessToken")
	if err != nil {
		t.Fatal(err)
	}
	ran := func() bool {
		_, err := os.Stat(surface.Ran)
		_ = os.Remove(surface.Ran)
		return err == nil
	}
	reached := 0
	for _, input := range inputs {
		library := false
		for _, data := range [][]byte{input.Credential, stripped(input.Credential)} {
			_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, data,
				&credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)})
			library = ran() || library
		}
		if library {
			reached++
		}
		identity := &ExternalAccountIdentity{CredentialsJSON: input.Credential, ServiceAccount: "robot@example.iam.gserviceaccount.com",
			Audience: testAudience, HTTP: HTTPClient()}
		_, _, err := identity.Token(context.Background())
		if ran() {
			t.Errorf("SIGNER RAN: %s", input.Name)
		}
		if err == nil || !strings.Contains(err.Error(), "repeats a key") {
			t.Errorf("NOT REFUSED: %s: %v", input.Name, err)
		}
	}
	if reached == 0 {
		t.Fatalf("control: Google's library started the signer for none of %d inputs, so this probe cannot see one run", len(inputs))
	}
	t.Logf("%d inputs; Google's library alone started the signer for %d of them", len(inputs), reached)
}

// stripped is data without its service_account_impersonation_url, as withoutImpersonation writes
// it, or data itself where that fails.
func stripped(data []byte) []byte {
	out, err := withoutImpersonation(data)
	if err != nil {
		return data
	}
	return out
}

// proxyConfiguration writes in dir an enterprise certificate proxy's certificate configuration,
// whose signer only creates the file ran, and answers the configuration's path and ran's.
func proxyConfiguration(t *testing.T, dir string) (config, ran string) {
	t.Helper()
	ran = filepath.Join(dir, "ran")
	signer := filepath.Join(dir, "signer")
	rewrite(t, signer, "#!/bin/sh\ntouch '"+ran+"'\n")
	if err := os.Chmod(signer, 0o700); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(dir, "proxy_config.json")
	rewrite(t, config, `{"cert_configs":{"pkcs11":{"module":"/nowhere","slot":1,"label":"key"}},"libs":{"ecp":"`+signer+`"}}`)
	return config, ran
}

// rewrite puts content in the file at path.
func rewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
