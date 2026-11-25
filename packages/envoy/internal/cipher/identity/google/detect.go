// packages/envoy/internal/cipher/identity/google/detect.go
//
// The identity a machine can trade at the broker when nothing tells it which one it holds.
// MachineIdentity reads a deployment's own choice (IdentityVar); DetectMachineIdentity finds one
// with no setting, from the Google exchange audience the broker answers in GET /v1/settings, so a
// VM and a workload with an external_account credential each reach the broker as themselves with
// nothing configured. It looks in a fixed order and takes the first identity whose token can carry
// the broker's audience: an external_account credential file, and the Compute Engine metadata
// server. A pod's projected Kubernetes token is not among them: the exchange mints no launcher
// credential for a Kubernetes subject, so no session could be registered under it.

package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"cloud.google.com/go/auth/credentials"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
)

// credentialsFileVar is the variable Google's own libraries read a credentials file from.
const credentialsFileVar = "GOOGLE_APPLICATION_CREDENTIALS"

// DetectMachineIdentity is the first identity this machine holds for audience, the Google
// exchange audience the broker names, with every place it looked before that one and why it had
// nothing: (a) the external_account credential file GOOGLE_APPLICATION_CREDENTIALS names, taken
// when it names a service account to act as; (b) the Compute Engine metadata server, taken when
// one answers within googleidentity.ProbeTimeout. It answers nil and no error where neither holds
// one, which is a machine with no identity of its own rather than a fault. A place that is present
// but broken — a credentials file that does not parse, a metadata server that answers with a
// fault — is an error, and nothing after it is tried. Nothing here verifies a token: the broker
// does that.
func DetectMachineIdentity(ctx context.Context, audience string, client *http.Client) (identity.Source, []identity.Skipped, error) {
	var skipped []identity.Skipped
	skip := func(source, reason string) {
		skipped = append(skipped, identity.Skipped{Source: source, Reason: reason})
	}

	if found, reason, err := externalAccountFor(audience, client); err != nil {
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
func externalAccountFor(audience string, client *http.Client) (identity.Source, string, error) {
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
	if repeated, err := googleidentity.RepeatsKey(data, googleidentity.CredentialFieldsType); err != nil {
		return nil, "", fmt.Errorf("the credentials file %s names is not JSON", credentialsFileVar)
	} else if repeated {
		return nil, "", errCredentialRepeatsKey
	}
	account := impersonatedAccount(head.Impersonation)
	if account == "" {
		return nil, "", fmt.Errorf("the credentials file %s names has a service_account_impersonation_url that names no service account", credentialsFileVar)
	}
	return &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: account, Audience: audience, HTTP: client}, "", nil
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
//     (googleidentity.CertificateFailure), which no client read before: the exchange's federated
//     access token is spent on generateIdToken itself, and the impersonation URL only names the
//     account, as cloud.google.com/go/auth's own credentials/idtoken does for an external_account
//     credential (baseCredsForImpersonation). The federation's principal needs getOpenIdToken on
//     the account (roles/iam.workloadIdentityUser carries it), and the account needs no role on
//     itself.
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
	return googleidentity.ServiceAccountWords(e.ServiceAccount, credentialsFileVar)
}

// errCredentialUnread is an external_account credential file cloud.google.com/go/auth does not
// read.
var errCredentialUnread = errors.New("the external_account credential " + credentialsFileVar + " names is not one this client reads")

// errCredentialRepeatsKey is a credentials file that repeats a key (googleidentity.RepeatsKey),
// which this client and Google's library could read as two different credentials.
var errCredentialRepeatsKey = errors.New("the credentials file " + credentialsFileVar + " names repeats a key, which Google's library could read as another credential than this client does")

// Token mints the token generateIdToken spends, the account's access token or, for an X.509
// certificate, the federated one (ExternalAccountIdentity), and spends it on one generateIdToken.
// Every error names the step that failed and never a token, a part of the file or a library's
// text: cloud.google.com/go/auth's errors quote the paths the file names and the body of an
// answer (externalAccountFailure). The library logs nothing, whatever GOOGLE_SDK_GO_LOGGING_LEVEL
// says: its log of a request holds the subject token or the federated token, and of an answer the
// federated token or the account's.
//
// A credentials file that repeats a key is refused before anything reads it
// (googleidentity.RepeatsKey). An X.509 certificate's credential is given to the library less its
// impersonation URL, and its files are checked (googleidentity.CertificateFailure) as those bytes
// read, in the types the library decodes them into, before the library reads them. Its exchange
// goes out on the library's own client, which presents the certificate: the library takes a
// client it is given in place of its own wherever the credential names no certificate
// configuration (use_default_certificate_config), and that client presents none.
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	if repeated, err := googleidentity.RepeatsKey(e.CredentialsJSON, googleidentity.CredentialFieldsType); err != nil {
		return "", "", errCredentialUnread
	} else if repeated {
		return "", "", errCredentialRepeatsKey
	}
	credential := e.CredentialsJSON
	fields, err := googleidentity.ReadCredentialFields(credential)
	if err != nil {
		return "", "", errCredentialUnread
	}
	certified := fields.Certificate() != nil
	answered := &oauthrefusal.Recorder{}
	options := &credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)}
	if !certified {
		options.Client = oauthrefusal.Client(e.HTTP, answered)
	} else {
		if credential, err = withoutImpersonation(credential); err != nil {
			return "", "", errCredentialUnread
		}
		if fields, err = googleidentity.ReadCredentialFields(credential); err != nil || fields.Certificate() == nil {
			return "", "", errCredentialUnread
		}
		if reason := googleidentity.CertificateFailure(*fields.Certificate()); reason != "" {
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
	return token, SubjectTokenType, nil
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
func externalAccountFailure(fields googleidentity.CredentialFields, answered *oauthrefusal.Recorder, err error) string {
	if reason := credentialFileFailure(fields); reason != "" {
		return reason
	}
	token := fields.TokenURL
	if token == "" {
		token = defaultTokenURL
	}
	tokenURL, _ := url.Parse(token)
	status, body, host := answered.Status, answered.Answer(), answered.Host
	if status == 0 && fields.Certificate() != nil && tokenURL != nil {
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
// reason (redact.File), or an X.509 certificate's files, as googleidentity.CertificateFailure
// says. The library's error quotes the path (and, for the subject token file, does not wrap the
// file call's error), so each is read again here.
func credentialFileFailure(fields googleidentity.CredentialFields) string {
	if file := fields.File(); file != "" {
		if err := opens(file); err != nil {
			return "read the subject token file its credential_source names: " + redact.File(err).Error()
		}
	}
	if certificate := fields.Certificate(); certificate != nil {
		return googleidentity.CertificateFailure(*certificate)
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
