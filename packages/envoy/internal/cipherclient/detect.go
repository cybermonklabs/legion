// packages/envoy/internal/cipherclient/detect.go
//
// The identity a machine can trade at the broker when nothing tells it which one it holds.
// MachineIdentity reads a deployment's own choice (IdentityVar); DetectMachineIdentity finds one
// with no setting, from the Google exchange audience the broker answers in GET /v1/settings, so a
// VM and a workload with an external_account credential each reach the broker as themselves with
// nothing configured. It looks in a fixed order and takes the first identity whose token can carry
// the broker's audience: an external_account credential file, and the Compute Engine metadata
// server. A pod's projected Kubernetes token is not among them: the exchange mints no launcher
// credential for a Kubernetes subject, so no session could be registered under it.

package cipherclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"cloud.google.com/go/auth/credentials"
	"github.com/googleapis/enterprise-certificate-proxy/client/util"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// credentialsFileVar is the variable Google's own libraries read a credentials file from.
	credentialsFileVar = "GOOGLE_APPLICATION_CREDENTIALS"
	// certificateConfigVar is the variable that names the default certificate configuration of an
	// X.509 credential with use_default_certificate_config, before gcloud's own file
	// (enterprise-certificate-proxy's util.GetConfigFilePathFromEnv reads it).
	certificateConfigVar = "GOOGLE_API_CERTIFICATE_CONFIG"
)

// Skipped is one place DetectMachineIdentity looked and found nothing it could trade, and why, in
// words a refusal prints as they stand.
type Skipped struct {
	Source, Reason string
}

// DetectMachineIdentity is the first identity this machine holds for audience, the Google
// exchange audience the broker names, with every place it looked before that one and why it had
// nothing: (a) the external_account credential file GOOGLE_APPLICATION_CREDENTIALS names, taken
// when it names a service account to act as; (b) the Compute Engine metadata server, taken when
// one answers within googleidentity.ProbeTimeout. It answers nil and no error where neither holds
// one, which is a machine with no identity of its own rather than a fault. A place that is present
// but broken — a credentials file that does not parse, a metadata server that answers with a
// fault — is an error, and nothing after it is tried. Nothing here verifies a token: the broker
// does that.
func DetectMachineIdentity(ctx context.Context, audience string) (Source, []Skipped, error) {
	var skipped []Skipped
	skip := func(source, reason string) { skipped = append(skipped, Skipped{Source: source, Reason: reason}) }

	if found, reason, err := externalAccountFor(audience); err != nil {
		return nil, skipped, err
	} else if found != nil {
		return found, skipped, nil
	} else {
		skip("credentials file", reason)
	}

	if _, err := googleidentity.Probe(ctx); errors.Is(err, googleidentity.ErrNoMetadataServer) {
		skip("metadata server", "no Google metadata server")
		return nil, skipped, nil
	} else if err != nil {
		return nil, skipped, err
	}
	return &MetadataIdentity{Audience: audience, FromBroker: true}, skipped, nil
}

// externalAccountFor is the external_account credential GOOGLE_APPLICATION_CREDENTIALS names,
// acting as the service account its service_account_impersonation_url names, for audience; or nil
// and why not. A file the variable names that cannot be read, or that is not JSON, is an error.
//
// The file holds a credential, so nothing here prints its path - for a person who pasted the
// credential itself into the variable, the path is the credential - or any of its content: a
// read error prints the system's reason alone (redact.File), a file that does not parse fixed
// text, and a credential of another kind its kind only where it is one Google's libraries name.
func externalAccountFor(audience string) (Source, string, error) {
	file := os.Getenv(credentialsFileVar)
	if file == "" {
		return nil, credentialsFileVar + " is unset", nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("read the credentials file %s names: %w", credentialsFileVar, redact.File(err))
	}
	if repeated, err := repeatsKey(data); err != nil {
		return nil, "", fmt.Errorf("the credentials file %s names is not JSON", credentialsFileVar)
	} else if repeated {
		return nil, "", errCredentialRepeatsKey
	}
	var head struct {
		Type          string `json:"type"`
		Impersonation string `json:"service_account_impersonation_url"`
	}
	if json.Unmarshal(data, &head) != nil {
		return nil, "", fmt.Errorf("the credentials file %s names is not JSON", credentialsFileVar)
	}
	if head.Type != string(credentials.ExternalAccount) || head.Impersonation == "" {
		kind := "another kind of"
		if credentialKinds[head.Type] {
			kind = fmt.Sprintf("a %q", head.Type)
		}
		return nil, fmt.Sprintf("the credentials file %s names is %s credential, not an external_account with a service account to act as", credentialsFileVar, kind), nil
	}
	account := impersonatedAccount(head.Impersonation)
	if account == "" {
		return nil, "", fmt.Errorf("the credentials file %s names has a service_account_impersonation_url that names no service account", credentialsFileVar)
	}
	return &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: account, Audience: audience, HTTP: HTTPClient()}, "", nil
}

// credentialKinds are the credential types Google's libraries read (cloud.google.com/go/auth's
// credentials.CredType), the only `type` a message about a credentials file names.
var credentialKinds = map[string]bool{
	string(credentials.ServiceAccount): true, string(credentials.AuthorizedUser): true, string(credentials.ExternalAccount): true,
	string(credentials.ExternalAccountAuthorizedUser): true, string(credentials.ImpersonatedServiceAccount): true,
	string(credentials.GDCHServiceAccount): true,
}

// impersonatedAccount is the service account a service_account_impersonation_url acts as: its
// last path element, before the colon that names generateAccessToken.
func impersonatedAccount(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	account, _, _ := strings.Cut(path.Base(u.Path), ":")
	if account == "." || account == "/" {
		return ""
	}
	return account
}

// ExternalAccountIdentity is the identity of a workload that holds an external_account credential
// file (workload identity federation) naming a service account to act as. The file may take its
// subject token from any credential_source cloud.google.com/go/auth reads, and which one decides
// the token that IAM Credentials' generateIdToken spends on the identity token the broker reads,
// for Audience and with includeEmail set, and so the IAM grants the account needs:
//
//   - A file, a URL, an executable or AWS, as golang.org/x/oauth2 read them before: the file's own
//     exchange at the security token service and its service_account_impersonation_url (IAM
//     Credentials' generateAccessToken) mint the account's access token, which generateIdToken
//     spends. The federation's principal needs getAccessToken on the account
//     (roles/iam.workloadIdentityUser carries it), and the account needs getOpenIdToken on itself
//     (roles/iam.serviceAccountOpenIdTokenCreator). An executable is told the account
//     (GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL).
//   - An X.509 certificate whose configuration names a workload certificate and key
//     (certificateFailure), which no client read before: the exchange's federated access token is
//     spent on generateIdToken itself, and the impersonation URL only names the account, as
//     cloud.google.com/go/auth's own credentials/idtoken does for an external_account credential
//     (baseCredsForImpersonation). The federation's principal needs getOpenIdToken on the account
//     (roles/iam.workloadIdentityUser carries it), and the account needs no role on itself.
type ExternalAccountIdentity struct {
	// CredentialsJSON is the credentials file as it was read.
	CredentialsJSON []byte
	// ServiceAccount is the account the file acts as, and the one generateIdToken is called on.
	ServiceAccount string
	// Audience is the `aud` of the identity token minted, the broker's Google exchange audience.
	Audience string
	// HTTP is the client every call goes through (nil is http.DefaultClient), except an X.509
	// certificate's exchange, which cloud.google.com/go/auth makes on a client of its own that
	// presents the certificate.
	HTTP *http.Client
	// IAMCredentialsURL is Google's own IAM Credentials service unless a test names a stand-in.
	IAMCredentialsURL string
}

// Describe names the identity in every trade's log line and error: the audience, the broker's
// Google exchange audience, as internal/redact's allow-list prints a URL, and by what it is where
// that withholds it, since it is text a broker's settings answer wrote; and the account where it
// is a service account's email or unique id (ServiceAccountWords), else by the credentials file
// that names it, since the file is a credential's place.
func (e *ExternalAccountIdentity) Describe() string {
	audience := "the Google exchange audience the broker names"
	if redact.Clean(e.Audience) {
		audience = e.Audience
	}
	return "a Google identity token for " + audience + " minted as " + e.account() + " through the external_account credential " + credentialsFileVar + " names"
}

// account is the service account the identity acts as, as a message names it.
func (e *ExternalAccountIdentity) account() string {
	return ServiceAccountWords(e.ServiceAccount, credentialsFileVar)
}

// errCredentialUnread is an external_account credential file cloud.google.com/go/auth does not
// read.
var errCredentialUnread = errors.New("the external_account credential " + credentialsFileVar + " names is not one this client reads")

// errCredentialRepeatsKey is a credentials file that repeats a key (repeatsKey), which this client
// and Google's library could read as two different credentials.
var errCredentialRepeatsKey = errors.New("the credentials file " + credentialsFileVar + " names repeats a key, which Google's library could read as another credential than this client does")

// Token mints the token generateIdToken spends, the account's access token or, for an X.509
// certificate, the federated one (ExternalAccountIdentity), and spends it on one generateIdToken.
// Every error names the step that failed and never a token, a part of the file or a library's
// text: cloud.google.com/go/auth's errors quote the paths the file names and the body of an
// answer (externalAccountFailure). The library logs nothing, whatever GOOGLE_SDK_GO_LOGGING_LEVEL
// says: its log of a request holds the subject token or the federated token, and of an answer the
// federated token or the account's.
//
// A credentials file that repeats a key is refused before anything reads it (repeatsKey). An
// X.509 certificate's credential is given to the library less its impersonation URL, and its
// files are checked (certificateFailure) as those bytes read, in the types the library decodes
// them into, before the library reads them. Its exchange goes out on the library's own client,
// which presents the certificate: the library takes a client it is given in place of its own
// wherever the credential names no certificate configuration (use_default_certificate_config), and
// that client presents none.
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	if repeated, err := repeatsKey(e.CredentialsJSON); err != nil {
		return "", "", errCredentialUnread
	} else if repeated {
		return "", "", errCredentialRepeatsKey
	}
	credential := e.CredentialsJSON
	fields, err := readCredentialFields(credential)
	if err != nil {
		return "", "", errCredentialUnread
	}
	certified := fields.certificate() != nil
	answered := &oauthrefusal.Recorder{}
	options := &credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)}
	if !certified {
		options.Client = oauthrefusal.Client(e.HTTP, answered)
	} else {
		if credential, err = withoutImpersonation(credential); err != nil {
			return "", "", errCredentialUnread
		}
		if fields, err = readCredentialFields(credential); err != nil || fields.certificate() == nil {
			return "", "", errCredentialUnread
		}
		if reason := certificateFailure(*fields.certificate()); reason != "" {
			return "", "", e.mintFailure(certified, reason)
		}
	}
	creds, err := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, credential, options)
	if err != nil {
		if reason := credentialFileFailure(fields); reason != "" {
			return "", "", e.mintFailure(certified, reason)
		}
		return "", "", errCredentialUnread
	}
	access, err := creds.Token(ctx)
	if err != nil {
		return "", "", e.mintFailure(certified, externalAccountFailure(fields, answered, err))
	}
	if access.Value == "" {
		return "", "", e.mintFailure(certified, "the exchange answered no access token")
	}
	token, err := generateIDToken(ctx, e.HTTP, e.IAMCredentialsURL, e.ServiceAccount, e.account(), e.Audience, access.Value)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

// mintFailure is the error of a mint that failed for reason, which names the account and never
// the file or a library's text: of the account's access token, or for an X.509 certificate
// (certified) of the federated one.
func (e *ExternalAccountIdentity) mintFailure(certified bool, reason string) error {
	if certified {
		return fmt.Errorf("mint a federated access token with the external_account credential %s names, to act as %s: %s", credentialsFileVar, e.account(), reason)
	}
	return fmt.Errorf("mint an access token as %s with the external_account credential %s names: %s", e.account(), credentialsFileVar, reason)
}

// withoutImpersonation is the credentials file less its service_account_impersonation_url, in any
// case, as Go's JSON decoder and so Google's library match it: the credential's own exchange, whose
// answer is the federated access token and not the account's.
func withoutImpersonation(credential []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(credential, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if strings.EqualFold(key, "service_account_impersonation_url") {
			delete(fields, key)
		}
	}
	return json.Marshal(fields)
}

// credentialFields are the fields of an external_account credential file this client reads for
// its own messages and checks: the token service, and the files its credential_source names. Each
// has the type cloud.google.com/go/auth decodes it into (internal/credsfile's
// ExternalAccountFile), a pointer where the library's is one, so that a null reads as the library
// reads it, and a file whose fields do not parse here is one the library refuses too.
type credentialFields struct {
	TokenURL string            `json:"token_url"`
	Source   *credentialSource `json:"credential_source"`
}

// credentialSource is a credential's credential_source, as far as this client reads it (the
// library's CredentialSource).
type credentialSource struct {
	File        string             `json:"file"`
	Certificate *certificateSource `json:"certificate"`
}

// readCredentialFields is the credentialFields of credential.
func readCredentialFields(credential []byte) (credentialFields, error) {
	var fields credentialFields
	err := json.Unmarshal(credential, &fields)
	return fields, err
}

// certificate is the X.509 certificate source the credential names, or nil.
func (f credentialFields) certificate() *certificateSource {
	if f.Source == nil {
		return nil
	}
	return f.Source.Certificate
}

// file is the subject token file the credential names, or "".
func (f credentialFields) file() string {
	if f.Source == nil {
		return ""
	}
	return f.Source.File
}

// certificateSource is an X.509 credential's credential_source: the certificate configuration it
// names, or the default one, and the trust chain beside it.
type certificateSource struct {
	Config     string `json:"certificate_config_location"`
	UseDefault bool   `json:"use_default_certificate_config"`
	TrustChain string `json:"trust_chain_path"`
}

// externalAccountFailure is why an external_account credential's mint failed, err, in words of
// this client's own. A file its credential_source names that cannot be read prints as
// credentialFileFailure says; a refusal names the service that answered - the security token
// service, IAM Credentials (at the impersonation URL), the token service the credential names
// (token_url, else the library's default, defaultTokenURL), or else the subject token source its
// credential_source names (a url, or AWS's metadata) - and the status and registered code its
// answer carried (redact.Answered); any other failure prints as its class (redact.Class). An X.509
// certificate's exchange goes out on the library's own client, which answered does not see, so the
// status and body of the security token service's answer are read from the library's error instead
// (libraryAnswer), and only its registered code printed.
func externalAccountFailure(fields credentialFields, answered *oauthrefusal.Recorder, err error) string {
	if reason := credentialFileFailure(fields); reason != "" {
		return reason
	}
	token := fields.TokenURL
	if token == "" {
		token = defaultTokenURL
	}
	tokenURL, _ := url.Parse(token)
	status, body, host := answered.Status, answered.Answer(), answered.Host
	if status == 0 && fields.certificate() != nil && tokenURL != nil {
		if quoted := libraryAnswer.FindStringSubmatch(err.Error()); quoted != nil {
			status, _ = strconv.Atoi(quoted[1])
			body, host = []byte(quoted[2]), tokenURL.Host
		}
	}
	if status/100 <= 2 {
		return redact.Class(err).Error()
	}
	service := "the subject token source its credential_source names"
	switch {
	case host == "sts.googleapis.com" || host == "sts.mtls.googleapis.com":
		service = "Google's security token service"
	case host == "iamcredentials.googleapis.com":
		service = "IAM Credentials"
	case tokenURL != nil && host == tokenURL.Host:
		service = "the token service the credential names"
	}
	return service + ": " + redact.Answered(status, body)
}

// libraryAnswer is the whole of the error cloud.google.com/go/auth answers for a security token
// service that refused an exchange, from its first word: `credentials: status code <status>:
// <body>` (credentials/internal/stsexchange). Another error that quotes those words further in is
// no answer. TestAnExternalAccountMintsFailureSaysWhatFailed and
// TestAnX509CredentialPresentsItsCertificateToTheTokenService fail where a release of the library
// words it otherwise.
var libraryAnswer = regexp.MustCompile(`(?s)\Acredentials: status code ([0-9]{3}): (.*)\z`)

// defaultTokenURL is where cloud.google.com/go/auth exchanges a credential that names no token_url
// (credentials/internal/externalaccount's defaultTokenURL, in the default universe: the library
// does not pass it a credential's universe_domain).
const defaultTokenURL = "https://sts.googleapis.com/v1/token"

// credentialFileFailure is why a file the credential's credential_source names cannot be used,
// and "" where each can be: the subject token file, whose read error prints as the file call's
// reason (redact.File), or an X.509 certificate's files, as certificateFailure says. The library's
// error quotes the path (and, for the subject token file, does not wrap the file call's error), so
// each is read again here.
func credentialFileFailure(fields credentialFields) string {
	if file := fields.file(); file != "" {
		if err := opens(file); err != nil {
			return "read the subject token file its credential_source names: " + redact.File(err).Error()
		}
	}
	if certificate := fields.certificate(); certificate != nil {
		return certificateFailure(*certificate)
	}
	return ""
}

// certificateFailure is why an X.509 credential's files cannot be used, and "" where each can be:
// its trust chain; its certificate configuration, the one it names or else the default one
// (GOOGLE_API_CERTIFICATE_CONFIG, else gcloud's), which Google's library finds the same way; and
// the certificate and private key that configuration names. A file that cannot be read prints as
// the file call's reason (redact.File), and one that does not parse as Google's library parses it
// as a fixed reason: the library fails on either before it sends the exchange, or in its TLS
// handshake, with no answer to name. A configuration that repeats a key (repeatsKey), or names no
// workload certificate and key, is refused: the one other kind Google's library reads, an
// enterprise certificate proxy's, makes it start the signer program the configuration names, with
// no setting that allows it (as GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES allows an executable
// credential_source) and no time limit. So is a source that names a configuration and asks for
// the default one too, which the library refuses, rather than read one of the two.
func certificateFailure(source certificateSource) string {
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
		config, what = util.GetConfigFilePathFromEnv(), "the default certificate configuration "+certificateConfigVar+" names"
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
	if repeated, err := repeatsKey(data); err != nil {
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
// it into (internal/transport/cert's certificateConfig), a pointer where the library's is one, so
// that a null reads as the library reads it. The library reads a nil Workload as an enterprise
// certificate proxy's configuration.
type certificateConfig struct {
	CertConfigs struct {
		Workload *struct {
			CertPath string `json:"cert_path"`
			KeyPath  string `json:"key_path"`
		} `json:"workload"`
	} `json:"cert_configs"`
}

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

// opens is nil where path opens for reading and is no directory, and the file call's error
// otherwise. It reads none of the file: the subject token file, which holds the credential the
// exchange is sent.
func opens(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	return nil
}

// repeatsKey is whether data, one JSON value, holds an object with two keys Go's JSON decoder
// reads as one (foldKey), at any depth, and an error where data is not one JSON value. Such a key
// is read differently by a decoder into a struct, into a pointer and into a map (the last of the
// keys, a merge of them, or each spelling), so where this client checks a file that Google's
// library then reads, a repeated key can make the two read different files: the file is refused
// before either reads it. A file gcloud writes repeats none.
func repeatsKey(data []byte) (bool, error) {
	// objects holds, for each object or array the walk is in, innermost last, the folded keys
	// read so far (nil for an array), and whether the next token is a key.
	type open struct {
		keys map[string]bool
		key  bool
	}
	var objects []open
	dec := json.NewDecoder(bytes.NewReader(data))
	read := false
	for {
		token, err := dec.Token()
		if err == io.EOF && read {
			return false, nil
		}
		if err == io.EOF {
			return false, io.ErrUnexpectedEOF
		}
		if err != nil {
			return false, err
		}
		if read {
			return false, errors.New("more than one JSON value")
		}
		inner := len(objects) - 1
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{':
				objects = append(objects, open{keys: map[string]bool{}, key: true})
				continue
			case '[':
				objects = append(objects, open{})
				continue
			}
			objects = objects[:inner]
		} else if inner >= 0 && objects[inner].key {
			folded := foldKey(token.(string))
			if objects[inner].keys[folded] {
				return true, nil
			}
			objects[inner].keys[folded], objects[inner].key = true, false
			continue
		}
		// A value ended: the next token of the object around it is a key.
		if outer := len(objects) - 1; outer < 0 {
			read = true
		} else if objects[outer].keys != nil {
			objects[outer].key = true
		}
	}
}

// foldKey is key as Go's JSON decoder matches it with a field's name (encoding/json's foldName):
// each ASCII letter in upper case, and each other rune as the least of the runes whose case folds
// to it, so that two keys fold alike exactly where strings.EqualFold holds.
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
