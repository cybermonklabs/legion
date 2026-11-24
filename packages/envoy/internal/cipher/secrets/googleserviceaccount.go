package secrets

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleIAMCredentialsURL is the IAM Service Account Credentials API, which mints an access token
// for a service account the caller may impersonate: this minter's endpoint, and the group
// reader's for its reader account's token (groups.NewTokenSource).
const GoogleIAMCredentialsURL = "https://iamcredentials.googleapis.com"

// GoogleCloudPlatformScope is the scope the broker holds its own credential at
// (OwnGoogleCredential) and the one a secret that names no scopes mints with: the type exists so a
// person's own machine acts as a service account of their fleet, and that account's IAM roles, not
// a narrower scope, are what bound it. A secret whose scopes tag names scopes asks Google for those
// instead (MintRequest.GoogleScopes), which is a token narrower than the account's own roles,
// never a wider one.
const GoogleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// GoogleAccessTokenLifetime is every generateAccessToken request's lifetime, fixed at Google's
// own maximum for a default-configured project, one hour: MintRequest carries no lifetime (see
// GoogleServiceAccount), and the group reader's token is minted again as it ends.
const GoogleAccessTokenLifetime = "3600s"

// googleServiceAccountEmail is one service account of a project, the only value this type mints
// from: 6 to 30 of lowercase letters, digits and hyphens, then the project and the fixed
// .iam.gserviceaccount.com domain, the rule Google's own account ids follow. It is anchored and
// admits no comma, space or slash, so a person's address (aditya@example.com), a list of accounts
// and a value that would reach past the request's path all fail it.
var googleServiceAccountEmail = regexp.MustCompile(`^[a-z0-9-]{6,30}@[a-z0-9-]+\.iam\.gserviceaccount\.com$`)

// ValidServiceAccountEmail answers whether account is one service account email this minter will
// mint for, the googleServiceAccountEmail rule above. It is exported so the broker's
// configuration can hold every account in BROKER_SERVICE_ACCOUNT_SECRETS to the same rule the
// minter applies, and so that setting can never bind a secret to an address the minter would
// refuse: a binding the minter can never match would refuse every mint of that secret with
// ErrMintNotBound, at mint time, rather than when the deployment was configured.
func ValidServiceAccountEmail(account string) bool {
	return googleServiceAccountEmail.MatchString(account)
}

// serviceAgent is a Google-managed service agent: an account named service-<number>,
// service-org-<number> or service-folder-<number>, or any account of a gcp-sa- domain. Google's
// own services run as these accounts, never a VM and never a workload of the deployment's.
var serviceAgent = regexp.MustCompile(`^service-(org-|folder-)?[0-9]+@|@gcp-sa-`)

// IsServiceAgent answers whether account is a Google-managed service agent. It is exported so
// that every setting of the broker that names a Google account — the minter's bindings, the
// judge's account, the directory reader's and the identity providers' — refuses one by the same
// rule: the broker must not be able to mint or impersonate a token of Google's own services.
func IsServiceAgent(account string) bool {
	return serviceAgent.MatchString(account)
}

// GoogleServiceAccount mints a Google access token for the one service account the deployment
// binds to a google-service-account secret (Accounts), by impersonating that account with the
// broker's own Google credential. The secret's stored value names an account as well, and a
// mint happens only when the two are the same account: the broker holds
// roles/iam.serviceAccountTokenCreator on every account it is bound to, so a stored value alone
// choosing the account would let whoever may create a secret store someone else's account in it
// and mint that person's token. The token carries the scopes the secret's scopes tag names
// (MintRequest.GoogleScopes), and the cloud-platform scope alone when it names none. BaseURL and
// HTTP let tests replace the IAM Service Account Credentials endpoint and its transport, as
// GoogleLogin's TokenURL and HTTP do.
//
// The minted token lives one hour, whatever lifetime the grant that released it carries:
// MintRequest holds no lifetime, and Google's generateAccessToken bounds the token itself, so a
// shorter grant stops the broker releasing the token again rather than cutting short the token
// already released. The policy makes that bearable by serving this type only to the requesters
// the secret's tag names — its owner, and beside them the registered services whose pods a
// person approves one by one — for grants of an hour at most (policy.TagRules).
type GoogleServiceAccount struct {
	BaseURL string
	HTTP    *http.Client
	// Source is the broker's OWN Google credential, the identity that must hold
	// roles/iam.serviceAccountTokenCreator on the account it mints for. Nil means Google's
	// application default credentials, which in production are the pod's Workload Identity,
	// found on the first mint that needs them and kept for every mint after it (ownSource):
	// nothing is resolved when the broker is built, so a test that sets a fixed source runs on a
	// machine with no Google credential at all.
	Source oauth2.TokenSource

	// Accounts is the deployment's binding of each google-service-account secret to the one
	// account it may mint for, keyed by the stored secret's policy name (DEV_PERSON, GCP_ADMIN),
	// which MintRequest.Source carries, so a profile of a bound secret mints under its source's
	// binding whatever its own name; each value is that secret's one account email. The broker
	// fills it from BROKER_SERVICE_ACCOUNT_SECRETS. A nil or empty map binds nothing, so every
	// mint of this type is refused with ErrMintNotBound, which is what a deployment that has not
	// set the variable gets; that is deliberate, since the stored value alone may not choose the
	// account a broker with Token Creator on it impersonates.
	Accounts map[string]string

	// Retry is the policy the generateAccessToken call is retried under, and the process-wide
	// budget it draws from, as GitHubApp's is; nil makes one attempt. The broker's own
	// credential's token (Source) is not this minter's call and is not retried here.
	Retry *Retry

	// own is the application default credentials, found by the first mint that needs them and
	// kept for the rest (OwnGoogleCredential), when Source is nil.
	own OwnGoogleCredential
}

// OwnGoogleCredential is the broker's own Google credential: the application default
// credentials at the cloud-platform scope, in production the pod's Workload Identity. They are
// found on the first call that needs them and kept once found, so no later call re-reads them and
// takes a token round trip of its own, and a transient failure to find them never becomes
// permanent; nothing is resolved when the holder is built, so a broker that never mints looks for
// none. The google-service-account minter impersonates with it, and the group reader mints its
// reader account's token with it (groups.TokenOptions.Default). Its Token makes it an
// oauth2.TokenSource. A zero value is ready; it is used by pointer, since it keeps what it found.
type OwnGoogleCredential struct {
	mu     sync.Mutex
	source oauth2.TokenSource
}

// errNoOwnCredential is a broker that found no Google credentials of its own. Their own error
// names the file or metadata server they tried, so it is dropped.
var errNoOwnCredential = errors.New("the broker found no Google credentials of its own")

// Source is the credentials' token source, found on the first call. They are found with a
// context detached from ctx's cancellation (its values are kept): x/oauth2's refresher holds on
// to the context its source was built with, so a source built from one request's context would
// fail every refresh once that request ended.
func (c *OwnGoogleCredential) Source(ctx context.Context) (oauth2.TokenSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source != nil {
		return c.source, nil
	}
	source, err := google.DefaultTokenSource(context.WithoutCancel(ctx), GoogleCloudPlatformScope)
	if err != nil {
		return nil, errNoOwnCredential
	}
	c.source = source
	return source, nil
}

// Token is the credentials' token, for a caller that holds no context. A token source's error
// can quote the credential it read, so only its class is answered.
func (c *OwnGoogleCredential) Token() (*oauth2.Token, error) {
	source, err := c.Source(context.Background())
	if err != nil {
		return nil, err
	}
	token, err := source.Token()
	if err != nil {
		return nil, errors.New("the broker's own Google credentials gave no token")
	}
	return token, nil
}

// ownSource is the broker's own credential for one mint: Source whenever a caller set one, and
// otherwise the application default credentials (OwnGoogleCredential). Its refusal is
// ErrMintUpstream.
func (m *GoogleServiceAccount) ownSource(ctx context.Context) (oauth2.TokenSource, error) {
	if m.Source != nil {
		return m.Source, nil
	}
	source, err := m.own.Source(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMintUpstream, err)
	}
	return source, nil
}

// GenerateAccessTokenAnswer is IAM's generateAccessToken answer, which this minter and the group
// reader both read.
type GenerateAccessTokenAnswer struct {
	AccessToken string `json:"accessToken"`
	// ExpireTime is RFC 3339, as Google's generateAccessToken answers it.
	ExpireTime string `json:"expireTime"`
}

// GenerateAccessTokenRequest is one generateAccessToken body: the scopes the token carries and its
// lifetime. It is marshalled per call rather than concatenated into a string, because Scope can
// come from a secret's scopes tag, and a value that held a quote or a backslash would otherwise
// break the JSON instead of being refused by Google.
type GenerateAccessTokenRequest struct {
	Scope    []string `json:"scope"`
	Lifetime string   `json:"lifetime"`
}

// boundAccount is the one account the deployment binds (accounts, the deployment's
// BROKER_SERVICE_ACCOUNT_SECRETS) to the stored secret req's value was read from (req.Source), so
// a profile of a bound secret mints under its source's binding whatever its own name, when stored,
// the account the secret's stored value names, is that same account; otherwise it is
// ErrMintNotBound, naming the name asked for and, for a profile, the source whose binding it read.
// It is weighed before the broker's own credential is resolved and before any call is made:
// whoever may create a secret may store any account in it, and the broker may mint for every
// account it is bound to, so the binding, not the stored value, chooses the account. The bound
// account is safe to name in an error, since an operator reads it to fix the setting; the stored
// value is not. The google-service-account and anthropic-federation minters both hold their
// secrets to it.
func boundAccount(accounts map[string]string, req MintRequest, stored string) (string, error) {
	bound, ok := accounts[req.Source]
	if !ok {
		return "", fmt.Errorf("%w: secret %s: the deployment binds no Google service account to %s", ErrMintNotBound, req.named(), req.Source)
	}
	if bound != stored {
		return "", fmt.Errorf("%w: secret %s: the deployment binds %s to %s, which its stored value does not name", ErrMintNotBound, req.named(), req.Source, bound)
	}
	return bound, nil
}

// Mint returns the bare Google access token for the account the deployment binds to the stored
// secret req's value was read from (req.Source), as GoogleLogin.Mint returns a login's, so a
// wrapper that exports it as a gcloud access token reads it unchanged. A stored value that is not
// one service account email, and a secret whose binding is missing or names a different account
// than the stored value, are both refused before any credential is resolved and before any call is
// made; the refusal names the name asked for and, for a profile, the source whose binding it read.
// It never puts the stored value, the broker's own token, the minted token or Google's answer in
// an error, and every error wraps one of ErrMintStoredValue, ErrMintNotBound, ErrMintRefused or
// ErrMintUpstream. The Minted it answers carries the expiry Google's own answer names, so the
// airlock's proxy mints again five minutes before the token stops working rather than holding
// one that has; an answer naming no expiry, or one this cannot read, carries none, which the
// proxy reads as a value no clock ends.
//
// The receiver is a pointer because a mint that resolves the broker's own credentials keeps
// them on it (ownSource), which a copy would throw away.
func (m *GoogleServiceAccount) Mint(ctx context.Context, req MintRequest) (Minted, error) {
	const what = "mint Google service account token"
	parsed, err := ParseValue(typeGoogleServiceAccount, req.Value)
	if err != nil {
		return Minted{}, err
	}
	secret := parsed.GoogleServiceAccount
	if _, err := boundAccount(m.Accounts, req, secret.ServiceAccount); err != nil {
		return Minted{}, err
	}
	source, err := m.ownSource(ctx)
	if err != nil {
		return Minted{}, err
	}
	// A token source's error can quote the credential it read, so only its class is reported.
	own, err := source.Token()
	if err != nil {
		return Minted{}, fmt.Errorf("%w: the broker's own Google credentials gave no token", ErrMintUpstream)
	}
	// Only the email is escaped, and the verb is appended after: url.PathEscape would be free to
	// escape the colon of ":generateAccessToken", and Google reads a path whose verb arrived as
	// %3AgenerateAccessToken as a resource name of its own and answers 404, never a token. The
	// escaping is what keeps a stored value from reaching a different resource; the pattern above
	// already admits no slash, so the two agree.
	endpoint := cmp.Or(m.BaseURL, GoogleIAMCredentialsURL) +
		"/v1/projects/-/serviceAccounts/" + url.PathEscape(secret.ServiceAccount) + ":generateAccessToken"
	// A secret that names no scopes mints at cloud-platform, as every token of this type did
	// before the scopes tag existed: an empty scope list is not a token with no scope, it is a
	// request Google refuses. Marshalling a struct of a string slice and a string cannot fail,
	// so its error is dropped.
	scopes := req.GoogleScopes
	if len(scopes) == 0 {
		scopes = []string{GoogleCloudPlatformScope}
	}
	body, _ := json.Marshal(GenerateAccessTokenRequest{Scope: scopes, Lifetime: GoogleAccessTokenLifetime})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Minted{}, fmt.Errorf("%w: create Google service account token request: %w", ErrMintUpstream, err)
	}
	own.SetAuthHeader(request)
	request.Header.Set("Content-Type", "application/json")
	payload, err := mintCall(m.Retry.start(req.Type), m.HTTP, request, what, "Google", http.StatusOK, mintResponseLimit, true, nil)
	if err != nil {
		return Minted{}, err
	}
	var token GenerateAccessTokenAnswer
	if json.Unmarshal(payload, &token) != nil {
		return Minted{}, fmt.Errorf("%w: decode Google service account token response: not a JSON object of its fields", ErrMintUpstream)
	}
	if token.AccessToken == "" {
		return Minted{}, fmt.Errorf("%w: Google returned no access token", ErrMintUpstream)
	}
	minted := Minted{Value: token.AccessToken}
	// An expireTime this cannot read is no reason to refuse a token Google answered: the value
	// works, and a Minted with no expiry is one the proxy holds until a reread ends it.
	if expires, err := time.Parse(time.RFC3339, token.ExpireTime); err == nil {
		minted.Expires = expires
	}
	return minted, nil
}
