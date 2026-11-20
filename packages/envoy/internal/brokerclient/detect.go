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
func externalAccountFor(audience string) (Source, string, error) {
	file := os.Getenv(credentialsFileVar)
	if file == "" {
		return nil, credentialsFileVar + " is unset", nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("read the credentials file %s names (%s): %w", credentialsFileVar, file, err)
	}
	var head struct {
		Type          string `json:"type"`
		Impersonation string `json:"service_account_impersonation_url"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, "", fmt.Errorf("read the credentials file %s names (%s): %w", credentialsFileVar, file, err)
	}
	if head.Type != string(google.ExternalAccount) || head.Impersonation == "" {
		return nil, fmt.Sprintf("the credentials file %s names (%s) is a %q credential, not an external_account with a service account to act as", credentialsFileVar, file, head.Type), nil
	}
	account := impersonatedAccount(head.Impersonation)
	if account == "" {
		return nil, "", fmt.Errorf("the credentials file %s names (%s) has a service_account_impersonation_url that names no service account", credentialsFileVar, file)
	}
	return &ExternalAccountIdentity{CredentialsJSON: data, ServiceAccount: account, Audience: audience, HTTP: HTTPClient()}, "", nil
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
// spends it on one generateIdToken. Every error names the step that failed and never a token. The
// exchange builds its requests' credentials itself (the subject token it read, the client's Basic
// pair, the access token it minted and spends on the impersonation call) and hands back only its
// error text, so its construction and each Token run under one record scope.
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	s := redact.Scope[*google.Credentials](ctx)
	creds, err := redact.OAuth2(s.Read(google.CredentialsFromJSONWithType(s.Context(), e.CredentialsJSON, google.ExternalAccount, poolScope)))
	if err != nil {
		return "", "", fmt.Errorf("read the external_account credential %s names: %w", credentialsFileVar, err)
	}
	access, err := redact.OAuth2(creds.TokenSource.Token())
	if err != nil {
		return "", "", fmt.Errorf("mint %s's access token with the external_account credential %s names: %w", e.ServiceAccount, credentialsFileVar, err)
	}
	token, err := generateIDToken(ctx, e.HTTP, e.IAMCredentialsURL, e.ServiceAccount, e.Audience, access.AccessToken)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}
