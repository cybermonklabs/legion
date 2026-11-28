// packages/envoy/internal/cipherclient/repeatprobe/repeatprobe.go
//
// Package repeatprobe writes, for the tests of a reader of external_account credentials files, the
// X.509 credentials files that repeat a key at each level such a reader decodes: the credentials
// file's credential_source, the source's certificate, the certificate's two ways to name its
// certificate configuration, and the configuration's cert_configs, workload, cert_path and
// key_path. Each repeat is spelt as the key is, in upper case, and in a Unicode case fold where
// the key has a letter with one (ſ for s, the Kelvin sign for k), with and without a null between
// the two, so every way Go's JSON decoder reads two keys as one is tried.
//
// Beside them it writes an enterprise certificate proxy's certificate configuration, whose signer
// only creates a file to mark that it ran. A test points GOOGLE_API_CERTIFICATE_CONFIG at it, so
// that a default certificate configuration is the proxy's too: a reader that hands one of these
// files to Google's library, where the library then reads the proxy's configuration, starts the
// signer.
package repeatprobe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/testexec"
)

// Surface is the files every input names.
type Surface struct {
	// Workload is a certificate configuration that names Cert and Key; Proxy is an enterprise
	// certificate proxy's, whose signer creates Ran when it runs.
	Workload, Proxy, Ran string
	// Cert and Key are a self-signed certificate and its private key, in PEM.
	Cert, Key   string
	dir, signer string
}

// Input is one credentials file that repeats a key, and what it repeats.
type Input struct {
	Name       string
	Credential []byte
}

// pkcs11 is the cert_configs section that makes a configuration with no workload an enterprise
// certificate proxy's for Google's library.
const pkcs11 = `{"module":"/nowhere","slot":1,"label":"key"}`

// New writes a Surface in dir.
func New(dir string) (Surface, error) {
	s := Surface{
		Workload: filepath.Join(dir, "workload_config.json"), Proxy: filepath.Join(dir, "proxy_config.json"),
		Ran: filepath.Join(dir, "ran"), Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem"),
		dir: dir, signer: filepath.Join(dir, "signer"),
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return s, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "repeatprobe"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return s, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return s, err
	}
	for _, file := range []struct {
		path, content string
		mode          os.FileMode
	}{
		{s.Cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), 0o600},
		{s.Key, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), 0o600},
		{s.signer, "#!/bin/sh\ntouch " + strconv.Quote(s.Ran) + "\n", 0o700},
		{s.Workload, s.config(`"workload":` + s.workload()), 0o600},
		{s.Proxy, s.config(`"pkcs11":` + pkcs11), 0o600},
	} {
		// The signer is a program Google's library may run while the test's process forks, so each
		// file is written under the fork lock.
		if err := testexec.WriteFile(file.path, []byte(file.content), file.mode); err != nil {
			return s, err
		}
	}
	return s, nil
}

// workload is the workload section that names the surface's certificate and key.
func (s Surface) workload() string {
	return `{"cert_path":` + strconv.Quote(s.Cert) + `,"key_path":` + strconv.Quote(s.Key) + `}`
}

// config is a certificate configuration whose cert_configs hold sections and whose signer, should
// Google's library read it as an enterprise certificate proxy's, is the surface's.
func (s Surface) config(sections string) string {
	return `{"cert_configs":{` + sections + `},"libs":{"ecp":` + strconv.Quote(s.signer) + `}}`
}

// choice is a value a repeated key takes, by what it is.
type choice struct{ what, value string }

// Inputs writes every input, each a credentials file whose security token service is tokenURL and
// whose impersonation URL is impersonationURL, and the certificate configurations they name.
func (s Surface) Inputs(tokenURL, impersonationURL string) ([]Input, error) {
	q := strconv.Quote
	top := `"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/x509",` +
		`"subject_token_type":"urn:ietf:params:oauth:token-type:mtls","token_url":` + q(tokenURL) + `,"service_account_impersonation_url":` + q(impersonationURL)
	file := func(members string) []byte { return []byte("{" + top + "," + members + "}") }
	location := func(path string) string { return `{"certificate_config_location":` + q(path) + `}` }
	certificates := []choice{
		{"the workload configuration", location(s.Workload)}, {"the proxy's configuration", location(s.Proxy)},
		{"the default configuration", `{"use_default_certificate_config":true}`}, {"null", "null"},
	}
	sources := make([]choice, len(certificates))
	for i, c := range certificates {
		sources[i] = choice{c.what, `{"certificate":` + c.value + `}`}
		if c.value == "null" {
			sources[i].value = "null"
		}
	}
	var inputs []Input
	add := func(level string, members []choice, wrap func(members string) ([]byte, error)) error {
		for _, m := range members {
			data, err := wrap(m.value)
			if err != nil {
				return err
			}
			inputs = append(inputs, Input{Name: level + ": " + m.what, Credential: data})
		}
		return nil
	}
	inFile := func(members string) ([]byte, error) { return file(members), nil }
	inSource := func(members string) ([]byte, error) { return file(`"credential_source":{` + members + `}`), nil }
	inCertificate := func(members string) ([]byte, error) {
		return file(`"credential_source":{"certificate":{` + members + `}}`), nil
	}
	configured := 0
	inConfig := func(sections func(members string) string) func(string) ([]byte, error) {
		return func(members string) ([]byte, error) {
			configured++
			path := filepath.Join(s.dir, fmt.Sprintf("config-%d.json", configured))
			if err := os.WriteFile(path, []byte(sections(members)), 0o600); err != nil {
				return nil, err
			}
			return file(`"credential_source":{"certificate":` + location(path) + `}`), nil
		}
	}
	workload := s.workload()
	for _, level := range []struct {
		name    string
		members []choice
		wrap    func(string) ([]byte, error)
	}{
		{"the credentials file's credential_source", repeats("credential_source", sources), inFile},
		{"the credential_source's certificate", repeats("certificate", certificates), inSource},
		{"the certificate's certificate_config_location", repeats("certificate_config_location",
			[]choice{{"the workload configuration", q(s.Workload)}, {"the proxy's configuration", q(s.Proxy)}, {"none", `""`}}), inCertificate},
		{"the certificate's use_default_certificate_config", repeats("use_default_certificate_config",
			[]choice{{"true", "true"}, {"false", "false"}}), inCertificate},
		{"the configuration's cert_configs", repeats("cert_configs", []choice{
			{"a workload", `{"workload":` + workload + `}`}, {"the proxy's", `{"pkcs11":` + pkcs11 + `}`},
			{"both", `{"workload":` + workload + `,"pkcs11":` + pkcs11 + `}`}, {"null", "null"},
		}), inConfig(func(members string) string { return `{` + members + `,"libs":{"ecp":` + q(s.signer) + `}}` })},
		{"the configuration's workload", repeats("workload", []choice{{"a workload", workload}, {"null", "null"}}),
			inConfig(func(members string) string { return s.config(`"pkcs11":` + pkcs11 + `,` + members) })},
		{"the workload's cert_path", repeats("cert_path", []choice{{"the certificate", q(s.Cert)}, {"none", `""`}}),
			inConfig(func(members string) string {
				return s.config(`"pkcs11":` + pkcs11 + `,"workload":{"key_path":` + q(s.Key) + `,` + members + `}`)
			})},
		{"the workload's key_path", repeats("key_path", []choice{{"the key", q(s.Key)}, {"none", `""`}}),
			inConfig(func(members string) string {
				return s.config(`"pkcs11":` + pkcs11 + `,"workload":{"cert_path":` + q(s.Cert) + `,` + members + `}`)
			})},
	} {
		if err := add(level.name, level.members, level.wrap); err != nil {
			return nil, err
		}
	}
	return inputs, nil
}

// repeats is every way to write key twice with values: each value, then each value again under
// each spelling of key (spellings), with and without a null under key between the two.
func repeats(key string, values []choice) []choice {
	q := strconv.Quote
	var out []choice
	for _, first := range values {
		for _, spelling := range spellings(key) {
			for _, second := range values {
				for _, between := range []bool{false, true} {
					members, what := q(key)+":"+first.value, first.what
					if between {
						members, what = members+","+q(key)+":null", what+", then null"
					}
					out = append(out, choice{
						what:  fmt.Sprintf("%s, then %s as %s", what, second.what, spelling),
						value: members + "," + q(spelling) + ":" + second.value,
					})
				}
			}
		}
	}
	return out
}

// spellings are the ways Go's JSON decoder reads as key: key itself, in upper case, and with its
// first s or k as the letter whose case folds to it (ſ, U+017F, or the Kelvin sign, U+212A).
func spellings(key string) []string {
	out := []string{key, strings.ToUpper(key)}
	switch {
	case strings.Contains(key, "s"):
		out = append(out, strings.Replace(key, "s", "ſ", 1))
	case strings.Contains(key, "k"):
		out = append(out, strings.Replace(key, "k", "\u212a", 1))
	}
	return out
}
