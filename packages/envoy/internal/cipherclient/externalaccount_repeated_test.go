// packages/envoy/internal/cipherclient/externalaccount_repeated_test.go
//
// A credentials file or a certificate configuration that repeats a key is refused before anything
// reads it: two keys of one object that Go's JSON decoder puts in one place of the types Google's
// library decodes the file into. In an object the library decodes into a struct the decoder matches
// a key with a field's name without case and keeps the last of the keys that match, and what
// "keeps" means differs by the type a reader decodes into: a null leaves a struct as it was but sets
// a pointer to nil, and a map keeps each spelling. So two readers of one such file, this client's
// check and Google's library, can read two different credentials. In an object the library decodes
// into a map, keys that differ in case are two keys. Each row runs the real reader against local
// stand-ins.

package cipherclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/auth/credentials"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient/repeatprobe"
	"github.com/sjawhar/envoy/testgo"
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

// TestARepeatedKeyIsFoundWhereTheLibraryDecodesOnePlace: repeatsKey finds two keys of one object
// that the decoder puts in one place of the types Google's library decodes the file into, wherever
// the object is: in a struct, two keys that name one field as strings.EqualFold matches them; in a
// map, two equal keys. Controls: header names that differ in case (a map), keys that name no field
// and what is under them, one field at two depths, and a number outside float64's range, which
// the decoder skips.
func TestARepeatedKeyIsFoundWhereTheLibraryDecodesOnePlace(t *testing.T) {
	file, config := credentialFieldsType, certificateConfigType
	for _, row := range []struct {
		name    string
		schema  reflect.Type
		data    string
		repeats bool
	}{
		{"a field repeated exactly", file, `{"type":"a","type":"b"}`, true},
		{"a field repeated in another case", file, `{"type":"x","TYPE":"y"}`, true},
		{"a field repeated with ſ for s", file, `{"credential_source":{},"credential_ſource":{}}`, true},
		{"a field repeated through an escape", file, `{"type":"a","\u0074ype":"b"}`, true},
		{"a field repeated after a null", file, `{"credential_source":{"file":"a"},"credential_source":null}`, true},
		{"a field of the certificate repeated in another case", file, `{"credential_source":{"certificate":{"trust_chain_path":"a","TRUST_CHAIN_PATH":"b"}}}`, true},
		{"a header repeated exactly", file, `{"credential_source":{"headers":{"X-Example":"a","X-Example":"b"}}}`, true},
		{"a field of the configuration repeated with the Kelvin sign for k", config, `{"cert_configs":{"workload":{"key_path":"a","\u212aey_path":"b"}}}`, true},
		{"a field of the configuration repeated in another case", config, `{"cert_configs":{},"Cert_Configs":{}}`, true},
		{"control: headers whose names differ in case", file, `{"credential_source":{"headers":{"X-Example":"e","x-example":"e"}}}`, false},
		{"control: a key that names no field, in another case", file, `{"extension":1,"EXTENSION":2}`, false},
		{"control: keys repeated under a key that names no field", file, `{"extension":{"type":1,"type":2}}`, false},
		// enterprise-certificate-proxy's util.LoadSignerBinaryPath decodes libs.ecp, but only for a
		// configuration with no workload, which certificateFailure refuses before the library reads it.
		{"control: keys repeated under libs, of which this client reads nothing", config, `{"libs":{"ecp":"a","ecp":"b"}}`, false},
		{"control: one field at two depths", file, `{"type":"a","credential_source":{"format":{"type":"b"}}}`, false},
		{"control: an object where the library decodes a string, which it refuses", file, `{"type":{"a":1,"a":2}}`, false},
		{"control: a number outside float64's range", file, `{"type":"external_account","unknown_extension":1e400}`, false},
		{"control: no object", file, `[1,"a",null]`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			repeats, err := repeatsKey([]byte(row.data), row.schema)
			if err != nil || repeats != row.repeats {
				t.Fatalf("repeatsKey(%s) = %t, %v; want %t", row.data, repeats, err, row.repeats)
			}
		})
	}
	for _, data := range []string{``, `{`, `{"a":1`, `{"a" 1}`, `{} {}`, `{}x`, `{"a":1,}`, `{"a":[1}`} {
		if _, err := repeatsKey([]byte(data), file); err == nil {
			t.Errorf("repeatsKey(%q) answered no error; want one, as for data that is not one JSON value", data)
		}
	}
	// Data after the value is repeatsKey's own error, and a repeat found before a defect answers
	// true with no error.
	if _, err := repeatsKey([]byte(`{} {}`), file); err == nil || err.Error() != "more than one JSON value" {
		t.Errorf("repeatsKey of two values: %v; want the error more than one JSON value", err)
	}
	if repeats, err := repeatsKey([]byte(`{"type":"a","type":"b"`), file); !repeats || err != nil {
		t.Errorf("repeatsKey of a truncated object that repeats a key = %t, %v; want true and no error", repeats, err)
	}
}

// TestAGcloudFileWithHeadersThatDifferInCaseMints: the credentials file `gcloud iam
// workload-identity-pools create-cred-config --credential-source-headers=X-Example=example,
// x-example=example` writes (Google Cloud SDK 594.0.0, bytes as it wrote them) holds two header
// names that differ in case. Google's library decodes headers into a map, which keeps them apart,
// so the file repeats no key, and the machine source mints with it: the subject token source is
// sent both headers, and the account's access token, which the impersonation URL mints, is spent on
// generateIdToken. Each Google host the file names is routed to a local stand-in.
func TestAGcloudFileWithHeadersThatDifferInCaseMints(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("testdata", "gcloud-credential-case-distinct-headers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sent []string
	subject := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = r.Header.Values("X-Example")
		mu.Unlock()
		_, _ = w.Write([]byte("the.url.subject"))
	})
	sts, exchange := federatedExchange(t)
	impersonation, impersonated := impersonationStandIn(t)
	iam, _ := accountIAM(t)
	t.Setenv(credentialsFileVar, path)
	source, reason, err := externalAccountFor(testAudience)
	if err != nil || source == nil {
		t.Fatalf("externalAccountFor = %v, %q, %v; want the file's identity", source, reason, err)
	}
	identity := source.(*ExternalAccountIdentity)
	identity.IAMCredentialsURL = iam
	identity.HTTP = routedClient(t, map[string]string{"tokens.example.invalid": subject, "sts.googleapis.com": sts, "iamcredentials.googleapis.com": impersonation})
	token, tokenType, err := identity.Token(context.Background())
	if err != nil || token != "robot-identity-token" || tokenType != oauthwire.SubjectTokenTypeGoogle {
		t.Fatalf("Token = %q, %q, %v; want the identity token", token, tokenType, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sent, []string{"example", "example"}) {
		t.Errorf("the subject token source was sent X-Example %q; want both headers the file names", sent)
	}
	if got := exchange().Get("subject_token"); got != "the.url.subject" {
		t.Errorf("the security token service was sent the subject token %q", got)
	}
	if n := impersonated.Load(); n != 1 {
		t.Errorf("the impersonation URL saw %d requests; want the one that mints the account's access token", n)
	}
}

// TestAnOutOfRangeNumberHidesNothingFromTheCheck: a number outside float64's range in a member the
// library skips, which the decoder reads as text, leaves the file one the check reads in full. A
// valid X.509 credential with such a member, at the credentials file's top level or in its
// certificate configuration, mints; one whose certificate configuration is an enterprise
// certificate proxy's is refused, and its signer does not run. Control: Google's library alone,
// given that file, starts the signer.
func TestAnOutOfRangeNumberHidesNothingFromTheCheck(t *testing.T) {
	const extension = `"unknown_extension":1e400,`
	t.Run("a credential that mints", func(t *testing.T) {
		for _, where := range []string{"credentials file", "certificate configuration"} {
			sts, _ := federatedExchange(t)
			iam, _ := federationIAM(t)
			files := writeX509Credential(t, t.TempDir(), sts, "https://iamcredentials.invalid")
			data, err := os.ReadFile(files.credential)
			if err != nil {
				t.Fatal(err)
			}
			if where == "credentials file" {
				data = []byte(strings.Replace(string(data), `{`, `{`+extension, 1))
			} else {
				config, err := os.ReadFile(files.config)
				if err != nil {
					t.Fatal(err)
				}
				rewrite(t, files.config, strings.Replace(string(config), `{`, `{`+extension, 1))
			}
			identity := &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: "robot@example.iam.gserviceaccount.com",
				Audience: testAudience, HTTP: HTTPClient(), IAMCredentialsURL: iam}
			if token, _, err := identity.Token(context.Background()); err != nil || token != "robot-identity-token" {
				t.Errorf("%s with %s: Token = %q, %v; want the identity token", where, extension, token, err)
			}
		}
	})
	t.Run("a proxy's configuration", func(t *testing.T) {
		dir := t.TempDir()
		files := writeX509Credential(t, dir, "http://127.0.0.1:9", "https://iamcredentials.invalid")
		ecp, ran := proxyConfiguration(t, dir)
		data, err := os.ReadFile(files.credential)
		if err != nil {
			t.Fatal(err)
		}
		credential := strings.Replace(strings.Replace(string(data), `{`, `{"x":1e400,`, 1), strconv.Quote(files.config), strconv.Quote(ecp), 1)
		_, _ = credentials.NewCredentialsFromJSON(credentials.ExternalAccount, stripped([]byte(credential)),
			&credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)})
		if err := os.Remove(ran); err != nil {
			t.Fatalf("control: Google's library did not start the signer: %v", err)
		}
		identity := &ExternalAccountIdentity{CredentialsJSON: []byte(credential), ServiceAccount: "robot@example.iam.gserviceaccount.com",
			Audience: testAudience, HTTP: HTTPClient()}
		_, _, err = identity.Token(context.Background())
		if want := "holds no workload certificate and private key"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Token: %v; want %q", err, want)
		}
		if _, err := os.Stat(ran); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the enterprise certificate proxy's signer ran (stat: %v)", err)
		}
	})
}

// TestTheMirroredTypesAreGooglesLibrarys: credentialFields and certificateConfig, from which
// repeatsKey reads which objects are structs and which are maps, have the shape of the types
// Google's library decodes a credentials file and a certificate configuration into, read from the
// library's own source in the module this package builds with: each field's JSON name, and a
// struct, a pointer, a map or a leaf of one kind (string, bool, int, interface and the rest) where
// the library has one. A type of the library's that the check cannot name fails it. Controls: the
// library's types with credential_source a struct and not a pointer, headers a string and not a
// map, type an any and not a string, headers a map of any and not of string, or type a named type
// the check does not know, each fail it.
func TestTheMirroredTypesAreGooglesLibrarys(t *testing.T) {
	list := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "cloud.google.com/go/auth")
	out, err := testgo.Output(list)
	if err != nil {
		t.Fatalf("go list -m cloud.google.com/go/auth: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	for _, row := range []struct {
		file, root string
		mirror     reflect.Type
	}{
		{"internal/credsfile/filetype.go", "ExternalAccountFile", credentialFieldsType},
		{"internal/transport/cert/workload_cert.go", "certificateConfig", certificateConfigType},
	} {
		types := structTypes(t, filepath.Join(dir, row.file))
		if reason := mirrorFailure(astShape(types[row.root], types), reflectShape(row.mirror)); reason != "" {
			t.Errorf("%s in %s %s", row.root, row.file, reason)
		}
	}

	const differs = "has the shape"
	for _, control := range []struct {
		name string
		edit func(types map[string]ast.Expr)
		want string
	}{
		{"credential_source a struct", func(types map[string]ast.Expr) {
			field := fieldTagged(t, types["ExternalAccountFile"], "credential_source")
			field.Type = field.Type.(*ast.StarExpr).X
		}, differs},
		{"headers a string", func(types map[string]ast.Expr) {
			fieldTagged(t, types["CredentialSource"], "headers").Type = ast.NewIdent("string")
		}, differs},
		{"type an any", func(types map[string]ast.Expr) {
			fieldTagged(t, types["ExternalAccountFile"], "type").Type = ast.NewIdent("any")
		}, differs},
		{"headers a map of any", func(types map[string]ast.Expr) {
			fieldTagged(t, types["CredentialSource"], "headers").Type = &ast.MapType{Key: ast.NewIdent("string"), Value: ast.NewIdent("any")}
		}, differs},
		{"type a named type", func(types map[string]ast.Expr) {
			fieldTagged(t, types["ExternalAccountFile"], "type").Type = ast.NewIdent("credentialKind")
		}, "names a type this check does not know: credentialKind"},
	} {
		types := structTypes(t, filepath.Join(dir, "internal/credsfile/filetype.go"))
		control.edit(types)
		if got := mirrorFailure(astShape(types["ExternalAccountFile"], types), reflectShape(credentialFieldsType)); !strings.Contains(got, control.want) {
			t.Errorf("control: with %s, the check says %q; want it to say %q", control.name, got, control.want)
		}
	}
}

// unknownType is how astShape writes a type it cannot name.
var unknownType = regexp.MustCompile(`unknown\(([^)]*)\)`)

// mirrorFailure is why library, the shape of a type of Google's library, is not mirror, the shape
// of its mirror, and "" where the two are one: a type of the library's that astShape cannot name,
// or another shape.
func mirrorFailure(library, mirror string) string {
	if unknown := unknownType.FindAllStringSubmatch(library, -1); unknown != nil {
		var names []string
		for _, match := range unknown {
			names = append(names, match[1])
		}
		return "names a type this check does not know: " + strings.Join(names, ", ")
	}
	if library != mirror {
		return "has the shape\n  " + library + "\nand the mirror\n  " + mirror
	}
	return ""
}

// structTypes is every struct type the Go file at path declares, by name.
func structTypes(t *testing.T, path string) map[string]ast.Expr {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]ast.Expr{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		if spec, ok := n.(*ast.TypeSpec); ok {
			if _, ok := spec.Type.(*ast.StructType); ok {
				types[spec.Name.Name] = spec.Type
			}
		}
		return true
	})
	return types
}

// fieldTagged is the field of the struct expr whose JSON name is name.
func fieldTagged(t *testing.T, expr ast.Expr, name string) *ast.Field {
	t.Helper()
	for _, field := range expr.(*ast.StructType).Fields.List {
		if jsonName(field) == name {
			return field
		}
	}
	t.Fatalf("no field %s", name)
	return nil
}

// jsonName is the JSON name a struct field's tag gives it, or its Go name.
func jsonName(field *ast.Field) string {
	if field.Tag != nil {
		tag, err := strconv.Unquote(field.Tag.Value)
		if err == nil {
			if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); name != "" {
				return name
			}
		}
	}
	return field.Names[0].Name
}

// leafKinds names, as reflect.Kind names it, the kind of each of Go's predeclared types a field can
// have: byte and rune are uint8 and int32, and any an interface.
var leafKinds = map[string]string{
	"bool": "bool", "string": "string", "int": "int", "int8": "int8", "int16": "int16", "int32": "int32",
	"int64": "int64", "uint": "uint", "uint8": "uint8", "uint16": "uint16", "uint32": "uint32",
	"uint64": "uint64", "uintptr": "uintptr", "float32": "float32", "float64": "float64",
	"complex64": "complex64", "complex128": "complex128", "byte": "uint8", "rune": "int32", "any": "interface",
}

// astShape is the shape of a type expression of a file whose struct types are types: a leaf's
// kind (leafKinds, and "interface" for an interface with no methods), "*" and the shape pointed
// at, "map[" key "]" value, "[]" element, or "{" each field's JSON name and shape "}", in name
// order. Any other type, such as a named type that is no struct of the file or a type of another
// package, is "unknown(" its name or Go type ")".
func astShape(expr ast.Expr, types map[string]ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		if named, ok := types[e.Name]; ok {
			return astShape(named, types)
		}
		if kind, ok := leafKinds[e.Name]; ok {
			return kind
		}
		return "unknown(" + e.Name + ")"
	case *ast.InterfaceType:
		if len(e.Methods.List) == 0 {
			return "interface"
		}
	case *ast.StarExpr:
		return "*" + astShape(e.X, types)
	case *ast.MapType:
		return "map[" + astShape(e.Key, types) + "]" + astShape(e.Value, types)
	case *ast.ArrayType:
		return "[]" + astShape(e.Elt, types)
	case *ast.StructType:
		var fields []string
		for _, field := range e.Fields.List {
			fields = append(fields, jsonName(field)+":"+astShape(field.Type, types))
		}
		slices.Sort(fields)
		return "{" + strings.Join(fields, ",") + "}"
	}
	return fmt.Sprintf("unknown(%T)", expr)
}

// reflectShape is the shape of a type, as astShape writes it, a leaf as its kind.
func reflectShape(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + reflectShape(t.Elem())
	case reflect.Map:
		return "map[" + reflectShape(t.Key()) + "]" + reflectShape(t.Elem())
	case reflect.Slice, reflect.Array:
		return "[]" + reflectShape(t.Elem())
	case reflect.Struct:
		var fields []string
		for i := range t.NumField() {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			fields = append(fields, name+":"+reflectShape(field.Type))
		}
		slices.Sort(fields)
		return "{" + strings.Join(fields, ",") + "}"
	}
	return t.Kind().String()
}

// routedClient is a client that sends a request for each host routes names to the stand-in at its
// base URL, and any other request as it stands.
func routedClient(t *testing.T, routes map[string]string) *http.Client {
	t.Helper()
	to := map[string]*url.URL{}
	for host, base := range routes {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		to[host] = u
	}
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if standIn, ok := to[r.URL.Host]; ok {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host, r.Host = standIn.Scheme, standIn.Host, standIn.Host
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
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
