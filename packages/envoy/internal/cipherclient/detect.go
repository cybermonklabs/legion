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
// file (workload identity federation) naming a service account to act as: the file's own
// exchange at the security token service mints a federated access token, which IAM Credentials'
// generateIdToken spends on the identity token the broker reads, for Audience and with
// includeEmail set, as a workload identity pool's federated token is spent (PoolIdentity). The
// federation's principal so needs getOpenIdToken on the account, which
// roles/iam.workloadIdentityUser carries, and the account needs no role on itself. The file may
// take its subject token from any credential_source cloud.google.com/go/auth reads: a file, a
// URL, an executable, AWS, or an X.509 certificate whose configuration names a workload
// certificate and key (certificateFailure).
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

// Token trades the credentials file at the security token service for a federated access token
// and spends it on one generateIdToken. The file's service_account_impersonation_url only names
// the account: it is never called. Every error names the step that failed and never a token, a
// part of the file or a library's text: cloud.google.com/go/auth's errors quote the paths the file
// names and the body of an answer (externalAccountFailure). The library logs nothing, whatever
// GOOGLE_SDK_GO_LOGGING_LEVEL says: its log of a request holds the subject token, and of an
// answer the federated token.
//
// An X.509 certificate's files are checked before the library reads them (certificateFailure),
// and its exchange goes out on the library's own client, which presents the certificate: the
// library takes a client it is given in place of its own wherever the credential names no
// certificate configuration (use_default_certificate_config), and that client presents none.
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	var fields credentialFields
	federation, err := withoutImpersonation(e.CredentialsJSON)
	if err != nil || json.Unmarshal(e.CredentialsJSON, &fields) != nil {
		return "", "", errCredentialUnread
	}
	answered := &oauthrefusal.Recorder{}
	options := &credentials.DetectOptions{Scopes: []string{poolScope}, Logger: slog.New(slog.DiscardHandler)}
	if certificate := fields.Source.Certificate; certificate == nil {
		options.Client = oauthrefusal.Client(e.HTTP, answered)
	} else if reason := certificateFailure(*certificate); reason != "" {
		return "", "", e.mintFailure(reason)
	}
	creds, err := credentials.NewCredentialsFromJSON(credentials.ExternalAccount, federation, options)
	if err != nil {
		if reason := credentialFileFailure(fields); reason != "" {
			return "", "", e.mintFailure(reason)
		}
		return "", "", errCredentialUnread
	}
	federated, err := creds.Token(ctx)
	if err != nil {
		return "", "", e.mintFailure(externalAccountFailure(fields, answered, err))
	}
	if federated.Value == "" {
		return "", "", e.mintFailure("the security token service answered no access_token")
	}
	token, err := generateIDToken(ctx, e.HTTP, e.IAMCredentialsURL, e.ServiceAccount, e.account(), e.Audience, federated.Value)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

// mintFailure is the error of a federated mint that failed for reason, which names the account
// and never the file or a library's text.
func (e *ExternalAccountIdentity) mintFailure(reason string) error {
	return fmt.Errorf("mint a federated access token with the external_account credential %s names, to act as %s: %s", credentialsFileVar, e.account(), reason)
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
// has the type cloud.google.com/go/auth reads it as, so a file whose fields do not parse here is
// one the library refuses too.
type credentialFields struct {
	TokenURL string `json:"token_url"`
	Source   struct {
		File        string             `json:"file"`
		Certificate *certificateSource `json:"certificate"`
	} `json:"credential_source"`
}

// certificateSource is an X.509 credential's credential_source: the certificate configuration it
// names, or the default one, and the trust chain beside it.
type certificateSource struct {
	Config     string `json:"certificate_config_location"`
	UseDefault bool   `json:"use_default_certificate_config"`
	TrustChain string `json:"trust_chain_path"`
}

// externalAccountFailure is why an external_account credential's exchange failed, err, in words
// of this client's own. A file its credential_source names that cannot be read prints as
// credentialFileFailure says; a refusal names the service that answered - the security token
// service, the token service the credential names (token_url), or else the subject token source
// its credential_source names (a url, or AWS's metadata) - and the status and registered code its
// answer carried (redact.Answered); any other failure prints as its class (redact.Class). An X.509
// certificate's exchange goes out on the library's own client, which answered does not see, so
// the status and body of the security token service's answer are read from the library's error
// text instead (libraryAnswer), and only its registered code printed.
func externalAccountFailure(fields credentialFields, answered *oauthrefusal.Recorder, err error) string {
	if reason := credentialFileFailure(fields); reason != "" {
		return reason
	}
	tokenURL, _ := url.Parse(fields.TokenURL)
	status, body, host := answered.Status, answered.Answer(), answered.Host
	if text := err.Error(); status == 0 && fields.Source.Certificate != nil && tokenURL != nil {
		if quoted := libraryAnswer.FindStringSubmatchIndex(text); quoted != nil {
			status, _ = strconv.Atoi(text[quoted[2]:quoted[3]])
			body, host = []byte(text[quoted[1]:]), tokenURL.Host
		}
	}
	if status/100 <= 2 {
		return redact.Class(err).Error()
	}
	service := "the subject token source its credential_source names"
	switch {
	case host == "sts.googleapis.com" || host == "sts.mtls.googleapis.com":
		service = "Google's security token service"
	case tokenURL != nil && host == tokenURL.Host:
		service = "the token service the credential names"
	}
	return service + ": " + redact.Answered(status, body)
}

// libraryAnswer is how cloud.google.com/go/auth quotes, in its error, the answer of a security
// token service that refused an exchange: `credentials: status code <status>: <body>`
// (credentials/internal/stsexchange). TestAnExternalAccountMintsFailureSaysWhatFailed fails where
// a release of the library words it otherwise.
var libraryAnswer = regexp.MustCompile(`credentials: status code ([0-9]{3}): `)

// credentialFileFailure is why a file the credential's credential_source names cannot be read,
// and "" where each can: the subject token file, or an X.509 certificate's files
// (certificateFailure). The library's error quotes the path (and, for the subject token file, does
// not wrap the file call's error), so each is opened again here and prints as the file call's
// reason (redact.File).
func credentialFileFailure(fields credentialFields) string {
	if file := fields.Source.File; file != "" {
		if err := opens(file); err != nil {
			return "read the subject token file its credential_source names: " + redact.File(err).Error()
		}
	}
	if fields.Source.Certificate != nil {
		return certificateFailure(*fields.Source.Certificate)
	}
	return ""
}

// certificateFailure is why an X.509 credential's files cannot be read, and "" where each can:
// its trust chain; its certificate configuration, the one it names or else the default one
// (GOOGLE_API_CERTIFICATE_CONFIG, else gcloud's), which Google's library finds the same way; and
// the certificate and private key that configuration names. A configuration that names no
// workload certificate and key is refused: the one other kind Google's library reads, an
// enterprise certificate proxy's, makes it start the signer program the configuration names, with
// no setting that allows it (as GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES allows an executable
// credential_source) and no time limit. Each file prints as the file call's reason (redact.File).
func certificateFailure(source certificateSource) string {
	if source.TrustChain != "" {
		if err := opens(source.TrustChain); err != nil {
			return "read the trust chain its credential_source names: " + redact.File(err).Error()
		}
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
	var workload struct {
		Configs struct {
			Workload struct {
				Cert string `json:"cert_path"`
				Key  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if json.Unmarshal(data, &workload) != nil {
		return what + " is not JSON"
	}
	cert, key := workload.Configs.Workload.Cert, workload.Configs.Workload.Key
	if cert == "" || key == "" {
		return what + " holds no workload certificate and private key"
	}
	for _, file := range []struct{ what, path string }{{"certificate", cert}, {"private key", key}} {
		if err := opens(file.path); err != nil {
			return "read the " + file.what + " its certificate configuration names: " + redact.File(err).Error()
		}
	}
	return ""
}

// opens is nil where path opens for reading and is no directory, and the file call's error
// otherwise. It reads none of the file, which can be a private key.
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
