// packages/envoy/internal/brokerclient/detect.go
//
// The identity a machine can trade at the broker when nothing tells it which one it holds.
// MachineIdentity reads a deployment's own choice (IdentityVar); DetectMachineIdentity finds one
// with no setting, from the Google exchange audience the broker answers in GET /v1/settings, so a
// VM and a workload with an external_account credential each reach the broker as themselves with
// nothing configured. It looks in a fixed order and takes the first identity whose token can carry
// the broker's audience: an external_account credential file, and the Compute Engine metadata
// server. A pod's projected Kubernetes token is not among them: the exchange mints no launcher
// credential for a Kubernetes subject, so no session could be registered under it.

package brokerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/redact"
)

// credentialsFileVar is the variable Google's own libraries read a credentials file from.
const credentialsFileVar = "GOOGLE_APPLICATION_CREDENTIALS"

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
	return NewMetadataIdentity(audience), skipped, nil
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
	if head.Type != string(google.ExternalAccount) || head.Impersonation == "" {
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

// credentialKinds are the credential types Google's libraries read (golang.org/x/oauth2/google's
// CredentialsType), the only `type` a message about a credentials file names.
var credentialKinds = map[string]bool{
	string(google.ServiceAccount): true, string(google.AuthorizedUser): true, string(google.ExternalAccount): true,
	string(google.ExternalAccountAuthorizedUser): true, string(google.ImpersonatedServiceAccount): true,
	string(google.GDCHServiceAccount): true,
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
// exchange mints that account's access token, which IAM Credentials' generateIdToken then spends
// on the identity token the broker reads, for Audience and with includeEmail set.
type ExternalAccountIdentity struct {
	// CredentialsJSON is the credentials file as it was read.
	CredentialsJSON []byte
	// ServiceAccount is the account the file acts as, and the one generateIdToken is called on.
	ServiceAccount string
	// Audience is the `aud` of the identity token minted, the broker's Google exchange audience.
	Audience string
	// HTTP is the client every call goes through; nil is http.DefaultClient.
	HTTP *http.Client
	// IAMCredentialsURL is Google's own IAM Credentials service unless a test names a stand-in.
	IAMCredentialsURL string
}

func (e *ExternalAccountIdentity) Describe() string {
	return "a Google identity token for " + e.Audience + " minted as " + e.ServiceAccount + " through the external_account credential " + credentialsFileVar + " names"
}

// Token mints the service account's access token with the credentials file's own exchange and
// spends it on one generateIdToken. Every error names the step that failed and never a token, a
// part of the file or a library's text: x/oauth2's errors quote the file's fields and the body of
// the security token service's answer, so a refusal prints as the status that answer came with
// (redact.Answered) and any other failure as its class (redact.Class).
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	if e.HTTP != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, e.HTTP)
	}
	answered := &statusOf{}
	ctx = withStatusOf(ctx, answered)
	creds, err := google.CredentialsFromJSONWithType(ctx, e.CredentialsJSON, google.ExternalAccount, poolScope)
	if err != nil {
		return "", "", fmt.Errorf("the external_account credential %s names is not one this client reads", credentialsFileVar)
	}
	access, err := creds.TokenSource.Token()
	if err != nil {
		cause := redact.Class(err).Error()
		if answered.status/100 > 2 {
			cause = redact.Answered(answered.status, nil)
		}
		return "", "", fmt.Errorf("mint %s's access token with the external_account credential %s names: %s", e.ServiceAccount, credentialsFileVar, cause)
	}
	token, err := generateIDToken(ctx, e.HTTP, e.IAMCredentialsURL, e.ServiceAccount, e.Audience, access.AccessToken)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}
