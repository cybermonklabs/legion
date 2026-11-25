// packages/envoy/internal/googleidentity/credentialsfile.go
//
// Whether an external_account credentials file may be handed to Google's library. It lives here,
// beside ServiceAccountWords, so both readers that hand one over — the Google identity provider's
// machine source and agent-secrets' GCP store — run the same check without naming a provider
// package, and a build without the Google provider keeps it.

package googleidentity

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"cloud.google.com/go/auth/credentials"
	"github.com/googleapis/enterprise-certificate-proxy/client/util"

	"github.com/sjawhar/envoy/internal/redact"
)

// CertificateConfigVar is the variable that names the default certificate configuration of an
// X.509 credential with use_default_certificate_config, before gcloud's own file
// (enterprise-certificate-proxy's util.GetConfigFilePathFromEnv reads it).
const CertificateConfigVar = "GOOGLE_API_CERTIFICATE_CONFIG"

// CredentialFields is an external_account credentials file in the types cloud.google.com/go/auth
// decodes it into (internal/credsfile's ExternalAccountFile and the types it holds): every field,
// with the library's JSON name, and a struct, a pointer, a map or a leaf of the kind the library
// has there, so that this client reads the bytes it checks as the library reads them, a null and a
// repeated key included (RepeatsKey reads which objects are structs and which are maps from these
// types). TestTheMirroredTypesAreGooglesLibrarys holds each field's name, shape and leaf kind to
// the library's own source. This client reads the token service and the files its
// credential_source names; the other fields are here for RepeatsKey.
type CredentialFields struct {
	Type                     string                 `json:"type"`
	ClientID                 string                 `json:"client_id"`
	ClientSecret             string                 `json:"client_secret"`
	Audience                 string                 `json:"audience"`
	SubjectTokenType         string                 `json:"subject_token_type"`
	Impersonation            string                 `json:"service_account_impersonation_url"`
	TokenURL                 string                 `json:"token_url"`
	Source                   *CredentialSource      `json:"credential_source,omitempty"`
	TokenInfoURL             string                 `json:"token_info_url"`
	ImpersonationLifetime    *impersonationLifetime `json:"service_account_impersonation,omitempty"`
	QuotaProjectID           string                 `json:"quota_project_id"`
	WorkforcePoolUserProject string                 `json:"workforce_pool_user_project"`
	UniverseDomain           string                 `json:"universe_domain"`
}

// CredentialFieldsType is CredentialFields' type, the schema RepeatsKey reads a credentials file
// with.
var CredentialFieldsType = reflect.TypeFor[CredentialFields]()

// CredentialSource is a credential's credential_source (the library's CredentialSource).
type CredentialSource struct {
	File                        string             `json:"file"`
	URL                         string             `json:"url"`
	Headers                     map[string]string  `json:"headers"`
	Executable                  *executableSource  `json:"executable,omitempty"`
	Certificate                 *CertificateSource `json:"certificate"`
	EnvironmentID               string             `json:"environment_id"`
	RegionURL                   string             `json:"region_url"`
	RegionalCredVerificationURL string             `json:"regional_cred_verification_url"`
	CredVerificationURL         string             `json:"cred_verification_url"`
	IMDSv2SessionTokenURL       string             `json:"imdsv2_session_token_url"`
	Format                      *subjectFormat     `json:"format,omitempty"`
}

// executableSource is an executable credential_source (the library's ExecutableConfig).
type executableSource struct {
	Command       string `json:"command"`
	TimeoutMillis int    `json:"timeout_millis"`
	OutputFile    string `json:"output_file"`
}

// subjectFormat is how a file or URL credential_source holds its subject token (the library's
// Format).
type subjectFormat struct {
	Type                  string `json:"type"`
	SubjectTokenFieldName string `json:"subject_token_field_name"`
}

// impersonationLifetime is a credential's service_account_impersonation (the library's
// ServiceAccountImpersonationInfo).
type impersonationLifetime struct {
	TokenLifetimeSeconds int `json:"token_lifetime_seconds"`
}

// ReadCredentialFields is the CredentialFields of credential.
func ReadCredentialFields(credential []byte) (CredentialFields, error) {
	var fields CredentialFields
	err := json.Unmarshal(credential, &fields)
	return fields, err
}

// Certificate is the X.509 certificate source the credential names, or nil.
func (f CredentialFields) Certificate() *CertificateSource {
	if f.Source == nil {
		return nil
	}
	return f.Source.Certificate
}

// File is the subject token file the credential names, or "".
func (f CredentialFields) File() string {
	if f.Source == nil {
		return ""
	}
	return f.Source.File
}

// CertificateSource is an X.509 credential's credential_source: the certificate configuration it
// names, or the default one, and the trust chain beside it.
type CertificateSource struct {
	Config     string `json:"certificate_config_location"`
	UseDefault bool   `json:"use_default_certificate_config"`
	TrustChain string `json:"trust_chain_path"`
}

// CredentialsFileFailure is why a credentials file must not be handed to Google's library, in
// words that print no path and no part of it, and "" where it may be. A file that is not JSON
// (json.Valid) answers "", for its reader's own refusal, and so does a file of another type than
// external_account, of which this client reads only its type, as every reader decodes it. An
// external_account file is refused where it repeats a key (RepeatsKey), or where it does not
// decode into the types the library decodes it into, so that the library refuses it too (a field
// the library reads that holds another JSON type, such as a token_url that is a number), or where
// it is an X.509 certificate's whose files cannot be used (CertificateFailure). Any reader that
// hands a credentials file to the library calls it first, on the bytes it hands over: the secret
// forms' sign-in (cmd/cipher), as the machine source does in Token (identity/google's
// ExternalAccountIdentity).
func CredentialsFileFailure(credential []byte) string {
	const unread = "it is not an external_account credential that Google's library reads"
	if !json.Valid(credential) {
		return ""
	}
	var kind struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(credential, &kind) != nil || kind.Type != string(credentials.ExternalAccount) {
		return ""
	}
	if repeated, err := RepeatsKey(credential, CredentialFieldsType); err != nil {
		return unread
	} else if repeated {
		return "it repeats a key, which Google's library could read as another credential than this client does"
	}
	fields, err := ReadCredentialFields(credential)
	if err != nil {
		return unread
	}
	if fields.Certificate() == nil {
		return ""
	}
	return CertificateFailure(*fields.Certificate())
}

// CertificateFailure is why an X.509 credential's files cannot be used, and "" where each can be:
// its trust chain; its certificate configuration, the one it names or else the default one
// (GOOGLE_API_CERTIFICATE_CONFIG, else gcloud's), which Google's library finds the same way; and
// the certificate and private key that configuration names. A file that cannot be read prints as
// the file call's reason (redact.File), and one that does not parse as Google's library parses it
// as a fixed reason: the library fails on either before it sends the exchange, or in its TLS
// handshake, with no answer to name. A configuration that repeats a key (RepeatsKey), or names no
// workload certificate and key, is refused: the one other kind Google's library reads, an
// enterprise certificate proxy's, makes it start the signer program the configuration names, with
// no setting that allows it (as GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES allows an executable
// credential_source) and no time limit. So is a source that names a configuration and asks for
// the default one too, which the library refuses, rather than read one of the two.
func CertificateFailure(source CertificateSource) string {
	var chain []byte
	if source.TrustChain != "" {
		var err error
		if chain, err = os.ReadFile(source.TrustChain); err != nil {
			return "read the trust chain its credential_source names: " + redact.File(err).Error()
		}
	}
	if source.Config != "" && source.UseDefault {
		return "its credential_source names a certificate configuration and asks for the default one too, which Google's library refuses"
	}
	config, what := source.Config, "the certificate configuration its credential_source names"
	if config == "" && source.UseDefault {
		config, what = util.GetConfigFilePathFromEnv(), "the default certificate configuration "+CertificateConfigVar+" names"
		if config == "" {
			config, what = util.GetDefaultConfigFilePath(), "gcloud's default certificate configuration"
		}
	}
	if config == "" {
		// Neither field: the library refuses the credential itself.
		return ""
	}
	data, err := os.ReadFile(config)
	if err != nil {
		return "read " + what + ": " + redact.File(err).Error()
	}
	if repeated, err := RepeatsKey(data, certificateConfigType); err != nil {
		return what + " is not JSON"
	} else if repeated {
		return what + " repeats a key"
	}
	var parsed certificateConfig
	if json.Unmarshal(data, &parsed) != nil {
		return what + " is not JSON"
	}
	workload := parsed.CertConfigs.Workload
	if workload == nil || workload.CertPath == "" || workload.KeyPath == "" {
		return what + " holds no workload certificate and private key"
	}
	cert, err := os.ReadFile(workload.CertPath)
	if err != nil {
		return "read the certificate its certificate configuration names: " + redact.File(err).Error()
	}
	key, err := os.ReadFile(workload.KeyPath)
	if err != nil {
		return "read the private key its certificate configuration names: " + redact.File(err).Error()
	}
	leaf := pemCertificate(cert)
	if leaf == nil {
		return "the certificate its certificate configuration names is not a PEM certificate"
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return "the private key its certificate configuration names is not the certificate's PEM private key"
	}
	return trustChainFailure(chain, leaf)
}

// certificateConfig is a certificate configuration decoded into the types Google's library decodes
// it into (internal/transport/cert's certificateConfig and the types it holds), a pointer where the
// library's is one, so that a null reads as the library reads it, and the schema RepeatsKey reads
// the configuration with. The library reads a nil Workload as an enterprise certificate proxy's
// configuration. TestTheMirroredTypesAreGooglesLibrarys holds each field's name, shape and leaf
// kind to the library's own source.
type certificateConfig struct {
	CertConfigs struct {
		Workload *struct {
			CertPath string `json:"cert_path"`
			KeyPath  string `json:"key_path"`
		} `json:"workload"`
	} `json:"cert_configs"`
}

// certificateConfigType is certificateConfig's type.
var certificateConfigType = reflect.TypeFor[certificateConfig]()

// pemCertificate is the certificate whose PEM block leads data, or nil where data leads with none,
// as Google's library reads a certificate file (credentials/internal/externalaccount's
// parseCertificate).
func pemCertificate(data []byte) *x509.Certificate {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	return certificate
}

// pemCertificateHeader opens each certificate of a trust chain file, which Google's library splits
// on it.
const pemCertificateHeader = "-----BEGIN CERTIFICATE-----"

// trustChainFailure is why chain, the content of a trust chain file, is not one Google's library
// reads beside leaf, and "" where it is (x509Provider's readTrustChain and subjectToken): PEM
// certificates, of which leaf may be only the first.
func trustChainFailure(chain []byte, leaf *x509.Certificate) string {
	read := 0
	for _, block := range strings.Split(string(chain), pemCertificateHeader) {
		if block = strings.TrimSpace(block); block == "" {
			continue
		}
		certificate := pemCertificate([]byte(pemCertificateHeader + "\n" + block))
		if certificate == nil {
			return "the trust chain its credential_source names is not PEM certificates"
		}
		if read > 0 && bytes.Equal(certificate.Raw, leaf.Raw) {
			return "the trust chain its credential_source names holds the certificate below its top"
		}
		read++
	}
	return ""
}

// RepeatsKey is whether data, one JSON value that Google's library decodes into a value of type
// schema, holds an object with two keys the decoder (encoding/json) puts in one place (placeOf): in
// an object the library decodes into a struct, two keys that name one of its fields, which the
// decoder matches without case (foldKey); in one it decodes into a map, such as a credential_source's
// headers, two equal keys, since a map keeps keys that differ in case apart. A key that names no
// field of its struct is one the decoder skips, value and all, so neither it nor anything under it
// is compared. A repeated key is read differently by a decoder into a struct, into a pointer and
// into a map (the last of the keys, a merge of them, or each spelling), so where this client checks
// a file that Google's library then reads, it can make the two read different files: such a file is
// refused before either reads it. Numbers are read as text (UseNumber), as the decoder skips one
// whatever its size. Where data is not one JSON value it answers an error: the decoder's for data
// that is truncated, has a delimiter, a colon or a comma out of place, or nests deeper than the
// decoder reads, and its own ("more than one JSON value") for data after the value. The walk stops
// at the first repeat, so data that repeats a key before such a defect answers true and no error,
// and its callers refuse it as a file that repeats a key.
func RepeatsKey(data []byte, schema reflect.Type) (bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if repeated, err := repeatsIn(dec, schema); err != nil || repeated {
		return repeated, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return false, errors.New("more than one JSON value")
	}
	return false, nil
}

// repeatsIn reads the next JSON value from dec, which the library decodes into a value of type
// schema (nil for one it skips), and answers whether an object in it repeats a key as RepeatsKey
// says.
func repeatsIn(dec *json.Decoder, schema reflect.Type) (bool, error) {
	token, err := dec.Token()
	if err == io.EOF {
		return false, io.ErrUnexpectedEOF
	}
	if err != nil {
		return false, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return false, nil
	}
	for schema != nil && schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	seen := map[string]bool{}
	for dec.More() {
		value := schema
		if delim == '{' {
			token, err := dec.Token()
			if err != nil {
				return false, err
			}
			key, _ := token.(string)
			place, in, decoded := placeOf(schema, key)
			if decoded {
				if seen[place] {
					return true, nil
				}
				seen[place] = true
			}
			value = in
		} else if schema != nil && (schema.Kind() == reflect.Slice || schema.Kind() == reflect.Array) {
			value = schema.Elem()
		} else if schema == nil || schema.Kind() != reflect.Interface {
			value = nil
		}
		if repeated, err := repeatsIn(dec, value); err != nil || repeated {
			return repeated, err
		}
	}
	_, err = dec.Token()
	return false, err
}

// placeOf is where the decoder puts key's value in an object it decodes into a value of type
// schema, and the type of the value there: the field key names in a struct (decoded is false where
// it names none), and key itself in a map, or in an any, which the decoder fills with a map. In an
// object where the library decodes a scalar, which it refuses, key goes nowhere.
func placeOf(schema reflect.Type, key string) (place string, value reflect.Type, decoded bool) {
	if schema == nil {
		return "", nil, false
	}
	switch schema.Kind() {
	case reflect.Struct:
		folded := foldKey(key)
		for i := range schema.NumField() {
			field := schema.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			if foldKey(name) == folded {
				return name, field.Type, true
			}
		}
		return "", nil, false
	case reflect.Map:
		return key, schema.Elem(), true
	case reflect.Interface:
		return key, schema, true
	}
	return "", nil, false
}

// foldKey is key as Go's JSON decoder matches it with a struct field's name: in its v1 defaults
// that match is strings.EqualFold (Go 1.27 builds encoding/json on encoding/json/v2, whose own
// case-insensitive match also drops _ and -, which v1's MatchCaseSensitiveDelimiter turns off).
// Each ASCII letter is put in upper case, and each other rune as the least of the runes whose case
// folds to it, so that two keys fold alike exactly where strings.EqualFold holds.
func foldKey(key string) string {
	var folded strings.Builder
	for _, r := range key {
		if r < utf8.RuneSelf {
			if 'a' <= r && r <= 'z' {
				r -= 'a' - 'A'
			}
			folded.WriteRune(r)
			continue
		}
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				r = next
				break
			}
			r = next
		}
		folded.WriteRune(r)
	}
	return folded.String()
}
