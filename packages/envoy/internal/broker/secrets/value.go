package secrets

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/githubappjwt"
)

// The kinds ParseValue reads, as policy.Kinds names them. This package cannot import policy, which
// imports it, so it names them again here; policy's own test holds every entry of policy.Kinds to
// a value ParseValue decodes, so the two lists cannot drift apart.
const (
	kindGitHubApp            = "github-app"
	kindGoogleLogin          = "google-login"
	kindGoogleServiceAccount = "google-service-account"
	kindAzureLogin           = "azure-login"
)

// Value is a kind secret's stored value as ParseValue decodes it: the field of the kind it was
// decoded as holds its fields, and every other field is zero.
type Value struct {
	GitHubApp            GitHubAppValue
	GoogleLogin          GoogleLoginValue
	GoogleServiceAccount GoogleServiceAccountValue
	AzureLogin           AzureLoginValue
}

// GitHubAppValue is a github-app secret's value: the App's id, a run of ASCII digits, and its
// private key, parsed, which has signed one JWT.
type GitHubAppValue struct {
	AppID string
	Key   *rsa.PrivateKey
}

// GoogleLoginValue is a google-login secret's value: an authorized_user credential's three fields.
type GoogleLoginValue struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// GoogleServiceAccountValue is a google-service-account secret's value: the one service account
// it names, an address ValidServiceAccountEmail accepts.
type GoogleServiceAccountValue struct {
	ServiceAccount string
}

// AzureLoginValue is an azure-login secret's value: the Entra tenant and client ids, neither
// empty nor white space alone. It certifies those fields and never that a mint succeeds, since
// the mint also needs the broker's projected token, which the value cannot carry.
type AzureLoginValue struct {
	TenantID string
	ClientID string
}

// githubAppSecret, googleLoginSecret, googleServiceAccountSecret and azureLoginSecret are the
// JSON objects each kind stores, as ParseValue decodes them.
type (
	githubAppSecret struct {
		AppID      string `json:"app_id"`
		PrivateKey string `json:"private_key"`
	}
	googleLoginSecret struct {
		Type         string `json:"type"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		RefreshToken string `json:"refresh_token"`
	}
	googleServiceAccountSecret struct {
		ServiceAccount string `json:"service_account"`
	}
	azureLoginSecret struct {
		TenantID string `json:"tenant_id"`
		ClientID string `json:"client_id"`
	}
)

// ParseValue is the one reader of a kind secret's stored value: it decodes value as kind stores
// it and refuses every value that kind's minter cannot mint from. The four minters call it
// before anything else they do with the value, and agent-secrets calls it before it writes one,
// so a value one refuses the other refuses too, in the same words.
//
//   - github-app: a JSON object whose app_id is a run of ASCII digits and whose private_key
//     githubappjwt.ParseKey reads and githubappjwt.Sign signs a JWT with.
//   - google-login: a JSON object of type authorized_user, naming its client_id, client_secret
//     and refresh_token.
//   - google-service-account: a JSON object whose service_account is one account
//     ValidServiceAccountEmail accepts.
//   - azure-login: a JSON object naming a tenant_id and a client_id that are not white space alone.
//
// Any other kind is refused. Every error wraps ErrMintStoredValue and never holds a byte of the
// value: encoding/json's syntax errors quote the byte they stopped at, so no decode's own error
// is wrapped.
func ParseValue(kind, value string) (Value, error) {
	switch kind {
	case kindGitHubApp:
		parsed, err := parseGitHubApp(value)
		return Value{GitHubApp: parsed}, err
	case kindGoogleLogin:
		parsed, err := parseGoogleLogin(value)
		return Value{GoogleLogin: parsed}, err
	case kindGoogleServiceAccount:
		parsed, err := parseGoogleServiceAccount(value)
		return Value{GoogleServiceAccount: parsed}, err
	case kindAzureLogin:
		parsed, err := parseAzureLogin(value)
		return Value{AzureLogin: parsed}, err
	default:
		return Value{}, fmt.Errorf("%w: %q is no kind a minter reads", ErrMintStoredValue, kind)
	}
}

// parseGitHubApp is ParseValue for a github-app value. The JWT it signs proves the key signs
// and is thrown away: Mint signs its own, at its own clock's now.
func parseGitHubApp(value string) (GitHubAppValue, error) {
	var secret githubAppSecret
	if json.Unmarshal([]byte(value), &secret) != nil {
		return GitHubAppValue{}, fmt.Errorf("%w: decode GitHub App secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if !validGitHubAppID(secret.AppID) || secret.PrivateKey == "" {
		return GitHubAppValue{}, fmt.Errorf("%w: GitHub App secret needs an app_id and private_key", ErrMintStoredValue)
	}
	key, err := githubappjwt.ParseKey(secret.PrivateKey)
	if err != nil {
		return GitHubAppValue{}, fmt.Errorf("%w: parse GitHub App private key: %w", ErrMintStoredValue, err)
	}
	if _, err := githubappjwt.Sign(secret.AppID, key, time.Now()); err != nil {
		return GitHubAppValue{}, fmt.Errorf("%w: sign GitHub App JWT: %w", ErrMintStoredValue, err)
	}
	return GitHubAppValue{AppID: secret.AppID, Key: key}, nil
}

// validGitHubAppID reports whether id is a non-empty run of ASCII digits.
func validGitHubAppID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool { return r < '0' || r > '9' })
}

// parseGoogleLogin is ParseValue for a google-login value.
func parseGoogleLogin(value string) (GoogleLoginValue, error) {
	var secret googleLoginSecret
	if json.Unmarshal([]byte(value), &secret) != nil {
		return GoogleLoginValue{}, fmt.Errorf("%w: decode Google login secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if secret.Type != "authorized_user" || secret.ClientID == "" || secret.ClientSecret == "" || secret.RefreshToken == "" {
		return GoogleLoginValue{}, fmt.Errorf("%w: Google login secret must be an authorized_user credential", ErrMintStoredValue)
	}
	return GoogleLoginValue{ClientID: secret.ClientID, ClientSecret: secret.ClientSecret, RefreshToken: secret.RefreshToken}, nil
}

// parseGoogleServiceAccount is ParseValue for a google-service-account value.
func parseGoogleServiceAccount(value string) (GoogleServiceAccountValue, error) {
	var secret googleServiceAccountSecret
	if json.Unmarshal([]byte(value), &secret) != nil {
		return GoogleServiceAccountValue{}, fmt.Errorf("%w: decode Google service account secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	if !ValidServiceAccountEmail(secret.ServiceAccount) {
		return GoogleServiceAccountValue{}, fmt.Errorf("%w: Google service account secret must name one <id>@<project>.iam.gserviceaccount.com account in service_account", ErrMintStoredValue)
	}
	return GoogleServiceAccountValue{ServiceAccount: secret.ServiceAccount}, nil
}

// parseAzureLogin is ParseValue for an azure-login value.
func parseAzureLogin(value string) (AzureLoginValue, error) {
	var secret azureLoginSecret
	if json.Unmarshal([]byte(value), &secret) != nil || strings.TrimSpace(secret.TenantID) == "" || strings.TrimSpace(secret.ClientID) == "" {
		return AzureLoginValue{}, fmt.Errorf("%w: Azure login secret must name tenant_id and client_id", ErrMintStoredValue)
	}
	return AzureLoginValue{TenantID: secret.TenantID, ClientID: secret.ClientID}, nil
}
