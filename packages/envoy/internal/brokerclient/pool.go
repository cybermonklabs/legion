// packages/envoy/internal/brokerclient/pool.go

package brokerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/redact"
)

const (
	// DefaultSecurityTokenServiceURL is Google's own security token service, where an external
	// credential is traded for a federated access token (RFC 8693 again, Google's spelling of
	// it: a JSON body with camelCase members rather than a form).
	DefaultSecurityTokenServiceURL = "https://sts.googleapis.com/v1/token"
	// DefaultIAMCredentialsURL is Google's own IAM Credentials service, whose generateIdToken
	// mints the identity token the broker reads.
	DefaultIAMCredentialsURL = "https://iamcredentials.googleapis.com"
	// poolRequestedTokenType is what the security token service answers: a federated access
	// token, spendable at IAM Credentials and nowhere else this client asks.
	poolRequestedTokenType = oauthwire.SubjectTokenTypeAccessToken
	// poolScope is the one scope a federated access token is asked for; IAM Credentials takes
	// no narrower one.
	poolScope = "https://www.googleapis.com/auth/cloud-platform"
	// poolTimeout bounds each of the two calls a pool mint makes.
	poolTimeout = 20 * time.Second
)

// PoolIdentity is the identity of a machine with no Google service account of its own: an
// external credential — the subject this deployment already holds, a projected Kubernetes token
// among them — traded at Google's security token service for a federated access token, which
// IAM Credentials' generateIdToken then spends to mint the Google identity token the broker
// reads. includeEmail is set on that mint, because the broker looks a machine up by the token's
// `email` claim and a token minted without it names no account at all.
//
// A token minted this way carries no google.compute_engine claims: it was minted off an
// instance. An exchange entry that pins a project or an instance name refuses it, and one that
// pins neither takes it, which is what the broker's own settings say.
type PoolIdentity struct {
	// Subject is the external credential the pool federates: the identity this machine already
	// holds, such as a projected Kubernetes service-account token minted for Audience.
	Subject Source
	// Audience is the workload identity pool provider the external credential is presented to,
	// the full resource name Google spells
	// //iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>.
	Audience string
	// ServiceAccount is the managed identity generateIdToken is called on: the service account
	// the pool lets this credential act as, by its email address or its unique id.
	ServiceAccount string
	// TokenAudience is the `aud` of the identity token generateIdToken mints — the client id
	// the broker checks against BROKER_GOOGLE_OIDC_AUDIENCE.
	TokenAudience string
	// SecurityTokenServiceURL and IAMCredentialsURL are Google's own services unless a dev
	// stack or a test names its own stand-ins. IAMCredentialsURL is an origin; the resource
	// path is this client's to build, as Google's own libraries build it.
	SecurityTokenServiceURL string
	IAMCredentialsURL       string
	// HTTP is the client both calls go through; nil is http.DefaultClient.
	HTTP *http.Client
}

// Describe names the identity in every trade log line and error, by what its settings are where
// they are plainly that, and by the variable that names them otherwise: the token audience as
// internal/redact's allow-list prints a URL, beside MachineAudienceVar; the pool audience whole
// where it is a workload identity pool provider's resource name (poolProvider), else by
// PoolAudienceVar alone, since it is no http URL the allow-list could print; and the account where
// it is a service account's email or unique id (serviceAccountWords), else by PoolAccountVar. A
// setting is where a person who pasted a credential in the wrong place puts it.
func (p *PoolIdentity) Describe() string {
	pool := "that " + PoolAudienceVar + " names"
	if poolProvider.MatchString(p.Audience) {
		pool = p.Audience + " (" + PoolAudienceVar + " names it)"
	}
	return "a Google identity token for " + redact.URL(p.TokenAudience) + " (" + MachineAudienceVar + " names the audience) minted through the workload identity pool " + pool + " as " + serviceAccountWords(p.ServiceAccount, PoolAccountVar)
}

// poolProvider is a workload identity pool provider's full resource name, as Google spells it:
// the one shape of AGENT_SECRETS_POOL_AUDIENCE Describe prints.
var poolProvider = regexp.MustCompile(`^//iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/[a-z0-9-]+/providers/[a-z0-9-]+$`)

// serviceAccountEmail and serviceAccountID are the two ways Google names a service account, its
// email and its numeric unique id: the shapes of an account serviceAccountWords prints.
var (
	serviceAccountEmail = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z][a-z0-9-]+\.iam\.gserviceaccount\.com$`)
	serviceAccountID    = regexp.MustCompile(`^[0-9]{1,32}$`)
)

// serviceAccountWords is account as a message names it: whole where it is a service account's
// email or unique id, else as the service account setting names, which is not printed.
func serviceAccountWords(account, setting string) string {
	if serviceAccountEmail.MatchString(account) || serviceAccountID.MatchString(account) {
		return account
	}
	return "the service account " + setting + " names (not printed: it is no service account's email or unique id)"
}

// Token trades the external credential for a federated access token and spends it on one
// generateIdToken, answering the identity token that mint produced. Every error names the call
// that failed and never a token.
func (p *PoolIdentity) Token(ctx context.Context) (string, string, error) {
	subject, subjectType, err := p.Subject.Token(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read the credential the workload identity pool federates: %w", err)
	}
	federated, err := p.federate(ctx, subject, subjectType)
	if err != nil {
		return "", "", err
	}
	token, err := generateIDToken(ctx, p.HTTP, p.IAMCredentialsURL, p.ServiceAccount, serviceAccountWords(p.ServiceAccount, PoolAccountVar), p.TokenAudience, federated)
	if err != nil {
		return "", "", err
	}
	return token, oauthwire.SubjectTokenTypeGoogle, nil
}

// federate is the security token service's half: the external credential for a federated access
// token that acts as the pool's own principal.
func (p *PoolIdentity) federate(ctx context.Context, subject, subjectType string) (string, error) {
	endpoint := p.SecurityTokenServiceURL
	if endpoint == "" {
		endpoint = DefaultSecurityTokenServiceURL
	}
	body, err := json.Marshal(map[string]string{
		"audience":           p.Audience,
		"grantType":          GrantTypeTokenExchange,
		"requestedTokenType": poolRequestedTokenType,
		"scope":              poolScope,
		"subjectTokenType":   poolSubjectTokenType(subjectType),
		"subjectToken":       subject,
	})
	if err != nil {
		return "", fmt.Errorf("build the security token service request: %w", err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
	}
	if err := postGoogle(ctx, p.HTTP, endpoint, "", body, &answer); err != nil {
		return "", fmt.Errorf("trade this machine's credential at the security token service %s: %w", redact.URL(endpoint), err)
	}
	if answer.AccessToken == "" {
		return "", fmt.Errorf("the security token service %s answered no access_token", redact.URL(endpoint))
	}
	return answer.AccessToken, nil
}

// generateIDToken is IAM Credentials' half of a mint: an access token that may act as
// serviceAccount (bearer) spent on one generateIdToken for audience, with includeEmail set, at
// iamURL, or Google's own service where it is empty. Every error names the account as named
// says. A workload identity pool spends its federated token here, and an external_account
// credential the access token it mints.
func generateIDToken(ctx context.Context, client *http.Client, iamURL, serviceAccount, named, audience, bearer string) (string, error) {
	origin := iamURL
	if origin == "" {
		origin = DefaultIAMCredentialsURL
	}
	endpoint := strings.TrimSuffix(origin, "/") + "/v1/projects/-/serviceAccounts/" +
		url.PathEscape(serviceAccount) + ":generateIdToken"
	body, err := json.Marshal(map[string]any{"audience": audience, "includeEmail": true})
	if err != nil {
		return "", fmt.Errorf("build the generateIdToken request: %w", err)
	}
	var answer struct {
		Token string `json:"token"`
	}
	// The endpoint's path holds the account, whose `@` internal/redact's allow-list never prints,
	// so a message names the service by its origin and the account beside it, as named says.
	at := "IAM Credentials' generateIdToken at " + redact.URL(origin) + " for " + named
	if err := postGoogle(ctx, client, endpoint, bearer, body, &answer); err != nil {
		return "", fmt.Errorf("mint this machine's identity token with %s: %w", at, err)
	}
	if answer.Token == "" {
		return "", fmt.Errorf("%s answered no token", at)
	}
	return answer.Token, nil
}

// postGoogle posts body to endpoint through client (nil is http.DefaultClient), with bearer on
// the Authorization header when it is not empty, and decodes the answer into out. None of its
// errors holds a library's text or a body: a request that failed is its class (redact.Class), a
// non-2xx answer its status and registered error code (redact.Answered) - a Google error body's
// description and message can be anything, an echo of the token the request carried among them -
// and a success that does not decode fixed text, since the answer holds a token.
func postGoogle(ctx context.Context, client *http.Client, endpoint, bearer string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, poolTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return redact.Class(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return redact.Class(err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return redact.Class(err)
	}
	if resp.StatusCode/100 != 2 {
		return errors.New(redact.Answered(resp.StatusCode, answer))
	}
	if json.Unmarshal(answer, out) != nil {
		return errors.New("the answer is not the JSON this call reads")
	}
	return nil
}

// poolSubjectTokenType is the subjectTokenType the security token service reads an external
// credential under. A Kubernetes projected token is a JWT there, whatever name the broker's own
// exchange knows it by, since the pool provider that verifies it is an OIDC provider and not
// this broker.
func poolSubjectTokenType(brokerType string) string {
	if brokerType == oauthwire.SubjectTokenTypeKubernetes {
		return "urn:ietf:params:oauth:token-type:jwt"
	}
	return brokerType
}
