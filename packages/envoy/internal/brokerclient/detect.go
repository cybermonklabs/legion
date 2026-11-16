// packages/envoy/internal/brokerclient/detect.go
//
// The identity a machine can trade at the broker when nothing tells it which one it holds.
// MachineIdentity reads a deployment's own choice (IdentityVar); DetectMachineIdentity finds one
// with no setting, from the exchange audiences the broker answers in GET /v1/settings, so a VM,
// a pod and a workload with an external_account credential each reach the broker as themselves
// with nothing configured. It looks in a fixed order and takes the first identity whose token
// can carry the broker's audience: a projected Kubernetes service-account token, an
// external_account credential file, and the Compute Engine metadata server.

package brokerclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/googleidentity"
)

// DefaultProjectedTokenFile is where a Legion pod's runtime mounts the projected service-account
// token minted for the broker's exchange audience, the file `agent-secrets enroll --pod` reads
// too. It is a variable so a test points it at a file of its own.
var DefaultProjectedTokenFile = "/var/run/legion/agent-secrets-token/token"

// credentialsFileVar is the variable Google's own libraries read a credentials file from.
const credentialsFileVar = "GOOGLE_APPLICATION_CREDENTIALS"

// ExchangeSettings is the two exchange audiences a broker answers in GET /v1/settings: the one a
// machine's Google identity token must carry (BROKER_GOOGLE_OIDC_AUDIENCE) and the one a pod's
// projected token must carry (BROKER_EXCHANGE_K8S_AUDIENCE). Either is empty where the broker
// accepts no such identity.
type ExchangeSettings struct {
	Google, Kubernetes string
}

// Skipped is one place DetectMachineIdentity looked and found nothing it could trade, and why, in
// words a refusal prints as they stand.
type Skipped struct {
	Source, Reason string
}

// DetectMachineIdentity is the first identity this machine holds for one of settings' audiences,
// with every place it looked before that one and why it had nothing: (a) the projected token at
// DefaultProjectedTokenFile, taken when its `aud` holds settings.Kubernetes; (b) the
// external_account credential file GOOGLE_APPLICATION_CREDENTIALS names, taken when it names a
// service account to act as; (c) the Compute Engine metadata server, taken when one answers
// within googleidentity.ProbeTimeout. (a) is skipped when settings.Kubernetes is empty, and (b)
// and (c) when settings.Google is. It answers nil and no error where none of them holds one,
// which is a machine with no identity of its own rather than a fault. A place that is present
// but broken — a token file it cannot read, a credentials file that does not parse, a metadata
// server that answers with a fault — is an error, and nothing after it is tried.
//
// Nothing here verifies a token: the broker does that. A projected token's payload is read only
// to see which audience it was minted for, since presenting one minted for another audience
// would be refused at the exchange anyway and would hide the identity after it.
func DetectMachineIdentity(ctx context.Context, settings ExchangeSettings) (Source, []Skipped, error) {
	var skipped []Skipped
	skip := func(source, reason string) { skipped = append(skipped, Skipped{Source: source, Reason: reason}) }

	if settings.Kubernetes == "" {
		skip("projected token", "the broker names no Kubernetes exchange audience")
	} else if found, reason, err := projectedTokenFor(settings.Kubernetes); err != nil {
		return nil, skipped, err
	} else if found != nil {
		return found, skipped, nil
	} else {
		skip("projected token", reason)
	}

	if settings.Google == "" {
		skip("credentials file", "the broker names no Google exchange audience")
		skip("metadata server", "the broker names no Google exchange audience")
		return nil, skipped, nil
	}
	if found, reason, err := externalAccountFor(settings.Google); err != nil {
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
	return NewMetadataIdentity(settings.Google), skipped, nil
}

// projectedTokenFor is the projected token at DefaultProjectedTokenFile when it was minted for
// audience, or nil and why not.
func projectedTokenFor(audience string) (Source, string, error) {
	file := DefaultProjectedTokenFile
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "no projected token at " + file, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the projected service-account token %s: %w", file, err)
	}
	minted, ok := jwtAudiences(strings.TrimSpace(string(data)))
	if !ok {
		return nil, "the projected token at " + file + " is not a JWT", nil
	}
	if !slices.Contains(minted, audience) {
		return nil, fmt.Sprintf("the projected token at %s was minted for %s, not the broker's exchange audience %s", file, quoteAll(minted), audience), nil
	}
	return NewProjectedToken(file), "", nil
}

// jwtAudiences reads the `aud` claim of a compact JWT without verifying it, as one string or a
// list of them; ok is false where token is not a JWT whose payload says.
func jwtAudiences(token string) (audiences []string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || len(claims.Aud) == 0 {
		return nil, false
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil {
		return []string{one}, true
	}
	if json.Unmarshal(claims.Aud, &audiences) == nil {
		return audiences, true
	}
	return nil, false
}

// quoteAll is audiences each quoted and joined, for a reason that names them.
func quoteAll(audiences []string) string {
	quoted := make([]string, len(audiences))
	for i, a := range audiences {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return strings.Join(quoted, ", ")
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
// spends it on one generateIdToken. Every error names the step that failed and never a token.
func (e *ExternalAccountIdentity) Token(ctx context.Context) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*poolTimeout)
	defer cancel()
	if e.HTTP != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, e.HTTP)
	}
	creds, err := google.CredentialsFromJSONWithType(ctx, e.CredentialsJSON, google.ExternalAccount, poolScope)
	if err != nil {
		return "", "", fmt.Errorf("read the external_account credential %s names: %w", credentialsFileVar, err)
	}
	access, err := creds.TokenSource.Token()
	if err != nil {
		return "", "", fmt.Errorf("mint %s's access token with the external_account credential %s names: %w", e.ServiceAccount, credentialsFileVar, err)
	}
	token, err := generateIDToken(ctx, e.HTTP, e.IAMCredentialsURL, e.ServiceAccount, e.Audience, access.AccessToken)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}
