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
	"reflect"
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
	if repeated, err := repeatsKey(data, credentialFieldsType); err != nil {
		return nil, "", fmt.Errorf("the credentials file %s names is not JSON", credentialsFileVar)
	} else if repeated {
		return nil, "", errCredentialRepeatsKey
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
	// HTTP is the client every call goes through, as a copy that follows no redirect (clientOf,
	// postGoogle); nil is http.DefaultClient. An X.509 certificate's exchange is the exception,
	// which cloud.google.com/go/auth makes on a client of its own that presents the certificate.
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
// that client presents none. Every other credential's exchange goes out on the identity's client
// as a copy that follows no redirect (clientOf HTTP), under a RefusalClient (oauthrefusal.Client).
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	if repeated, err := repeatsKey(e.CredentialsJSON, credentialFieldsType); err != nil {
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
		options.Client = oauthrefusal.Client(clientOf(ctx, e.HTTP), answered)
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

// credentialFields is an external_account credentials file in the types cloud.google.com/go/auth
// decodes it into (internal/credsfile's ExternalAccountFile and the types it holds): every field,
// with the library's JSON name, and a struct, a pointer, a map or a leaf of the kind the library
// has there, so that this client reads the bytes it checks as the library reads them, a null and a
// repeated key included (repeatsKey reads which objects are structs and which are maps from these
// types). TestTheMirroredTypesAreGooglesLibrarys holds each field's name, shape and leaf kind to
// the library's own source. This client reads the token service and the files its
// credential_source names; the other fields are here for repeatsKey.
type credentialFields struct {
	Type                     string                 `json:"type"`
	ClientID                 string                 `json:"client_id"`
	ClientSecret             string                 `json:"client_secret"`
	Audience                 string                 `json:"audience"`
	SubjectTokenType         string                 `json:"subject_token_type"`
	Impersonation            string                 `json:"service_account_impersonation_url"`
	TokenURL                 string                 `json:"token_url"`
	Source                   *credentialSource      `json:"credential_source,omitempty"`
	TokenInfoURL             string                 `json:"token_info_url"`
	ImpersonationLifetime    *impersonationLifetime `json:"service_account_impersonation,omitempty"`
	QuotaProjectID           string                 `json:"quota_project_id"`
	WorkforcePoolUserProject string                 `json:"workforce_pool_user_project"`
	UniverseDomain           string                 `json:"universe_domain"`
}

// credentialFieldsType is credentialFields' type, the schema repeatsKey reads a credentials file
// with.
var credentialFieldsType = reflect.TypeFor[credentialFields]()

// credentialSource is a credential's credential_source (the library's CredentialSource).
type credentialSource struct {
	File                        string             `json:"file"`
	URL                         string             `json:"url"`
	Headers                     map[string]string  `json:"headers"`
	Executable                  *executableSource  `json:"executable,omitempty"`
	Certificate                 *certificateSource `json:"certificate"`
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

// CredentialsFileFailure is why a credentials file must not be handed to Google's library, in
// words that print no path and no part of it, and "" where it may be. A file that is not JSON
// (json.Valid) answers "", for its reader's own refusal, and so does a file of another type than
// external_account, of which this client reads only its type, as every reader decodes it. An
// external_account file is refused where it repeats a key (repeatsKey), or where it does not
// decode into the types the library decodes it into, so that the library refuses it too (a field
// the library reads that holds another JSON type, such as a token_url that is a number), or where
// it is an X.509 certificate's whose files cannot be used (certificateFailure). Any reader that
// hands a credentials file to the library calls it first, on the bytes it hands over: the secret
// forms' sign-in (cmd/cipher), as the machine source does in Token.
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
	if repeated, err := repeatsKey(credential, credentialFieldsType); err != nil {
		return unread
	} else if repeated {
		return "it repeats a key, which Google's library could read as another credential than this client does"
	}
	fields, err := readCredentialFields(credential)
	if err != nil {
		return unread
	}
	if fields.certificate() == nil {
		return ""
	}
	return certificateFailure(*fields.certificate())
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
	if repeated, err := repeatsKey(data, certificateConfigType); err != nil {
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
// library's is one, so that a null reads as the library reads it, and the schema repeatsKey reads
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

// repeatsKey is whether data, one JSON value that Google's library decodes into a value of type
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
func repeatsKey(data []byte, schema reflect.Type) (bool, error) {
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
// schema (nil for one it skips), and answers whether an object in it repeats a key as repeatsKey
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
