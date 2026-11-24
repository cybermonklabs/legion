package secrets

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/githubappjwt"
)

// The types ParseValue reads, as policy.Types names them. This package cannot import policy, which
// imports it, so it names them again here; policy's own test holds every entry of policy.Types to
// a value ParseValue decodes, so the two lists cannot drift apart.
const (
	typeGitHubApp            = "github-app"
	typeGitHubInstallations  = "github-installations"
	typeGoogleLogin          = "google-login"
	typeGoogleServiceAccount = "google-service-account"
	typeAzureLogin           = "azure-login"
	typeAnthropicFederation  = "anthropic-federation"
)

// Value is a type secret's stored value as ParseValue decodes it: the field of the type it was
// decoded as holds its fields, and every other field is zero.
type Value struct {
	GitHubApp            GitHubAppValue
	GitHubInstallations  GitHubInstallationsValue
	GoogleLogin          GoogleLoginValue
	GoogleServiceAccount GoogleServiceAccountValue
	AzureLogin           AzureLoginValue
	AnthropicFederation  AnthropicFederationValue
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

// AnthropicFederationValue is an anthropic-federation secret's value: the Anthropic
// organization's UUID, the federation rule (fdrl_...) the exchange asks Anthropic to evaluate, the
// rule's target Anthropic service account (svac_...), the Google service account whose identity
// token the exchange presents (an address ValidServiceAccountEmail accepts), and the workspace
// (wrkspc_...) to scope the token to, empty where the rule is enabled for one workspace alone, in
// which case the exchange names none (Anthropic's WIF reference, "Token exchange request").
type AnthropicFederationValue struct {
	OrganizationID   string
	FederationRuleID string
	ServiceAccountID string
	ServiceAccount   string
	WorkspaceID      string
}

// githubAppSecret, googleLoginSecret, googleServiceAccountSecret, azureLoginSecret and
// anthropicFederationSecret are the JSON objects each type stores, as ParseValue decodes them.
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
	anthropicFederationSecret struct {
		OrganizationID   string `json:"organization_id"`
		FederationRuleID string `json:"federation_rule_id"`
		ServiceAccountID string `json:"service_account_id"`
		ServiceAccount   string `json:"service_account"`
		WorkspaceID      string `json:"workspace_id"`
	}
)

// ParseValue is the one reader of a type secret's stored value: it decodes value as type stores
// it and refuses every value that type's minter cannot mint from. The six minters call it
// before anything else they do with the value, and agent-secrets calls it before it writes one,
// so a value one refuses the other refuses too, in the same words.
//
//   - github-app: a JSON object whose app_id is a run of ASCII digits and whose private_key
//     githubappjwt.ParseKey reads and githubappjwt.Sign signs a JWT with.
//   - github-installations: a JSON object of installations, each an installation token a minter
//     stored under its account's GitHub login (parseGitHubInstallations).
//   - google-login: a JSON object of type authorized_user, naming its client_id, client_secret
//     and refresh_token.
//   - google-service-account: a JSON object whose service_account is one account
//     ValidServiceAccountEmail accepts.
//   - azure-login: a JSON object naming a tenant_id and a client_id that are not white space alone.
//   - anthropic-federation: a JSON object whose organization_id is a UUID, whose
//     federation_rule_id is an fdrl_ id and service_account_id an svac_ id, whose service_account
//     is one account ValidServiceAccountEmail accepts, and whose workspace_id, when it names one,
//     is a wrkspc_ id.
//
// Any other type is refused. Every error wraps ErrMintStoredValue and never holds a byte of the
// value: encoding/json's syntax errors quote the byte they stopped at, so no decode's own error
// is wrapped.
func ParseValue(secretType, value string) (Value, error) {
	switch secretType {
	case typeGitHubApp:
		parsed, err := parseGitHubApp(value)
		return Value{GitHubApp: parsed}, err
	case typeGitHubInstallations:
		parsed, err := parseGitHubInstallations(value)
		return Value{GitHubInstallations: parsed}, err
	case typeGoogleLogin:
		parsed, err := parseGoogleLogin(value)
		return Value{GoogleLogin: parsed}, err
	case typeGoogleServiceAccount:
		parsed, err := parseGoogleServiceAccount(value)
		return Value{GoogleServiceAccount: parsed}, err
	case typeAzureLogin:
		parsed, err := parseAzureLogin(value)
		return Value{AzureLogin: parsed}, err
	case typeAnthropicFederation:
		parsed, err := parseAnthropicFederation(value)
		return Value{AnthropicFederation: parsed}, err
	default:
		return Value{}, fmt.Errorf("%w: %q is no type a minter reads", ErrMintStoredValue, secretType)
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

// The shapes of an anthropic-federation value's Anthropic ids: the organization's UUID, and the
// tagged ids of a federation rule, a service account and a workspace, each its prefix and then
// ASCII letters and digits. A workspace_id of "default", which Anthropic still takes for the
// Default Workspace, is refused: Anthropic's reference names it deprecated and asks for the
// workspace's own wrkspc_ id.
var (
	anthropicOrganizationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	anthropicRuleID         = regexp.MustCompile(`^fdrl_[A-Za-z0-9]+$`)
	anthropicServiceAccount = regexp.MustCompile(`^svac_[A-Za-z0-9]+$`)
	anthropicWorkspaceID    = regexp.MustCompile(`^wrkspc_[A-Za-z0-9]+$`)
)

// parseAnthropicFederation is ParseValue for an anthropic-federation value.
func parseAnthropicFederation(value string) (AnthropicFederationValue, error) {
	var secret anthropicFederationSecret
	if json.Unmarshal([]byte(value), &secret) != nil {
		return AnthropicFederationValue{}, fmt.Errorf("%w: decode Anthropic federation secret: not a JSON object of its fields", ErrMintStoredValue)
	}
	switch {
	case !anthropicOrganizationID.MatchString(secret.OrganizationID):
		return AnthropicFederationValue{}, fmt.Errorf("%w: Anthropic federation secret must name its organization's UUID in organization_id", ErrMintStoredValue)
	case !anthropicRuleID.MatchString(secret.FederationRuleID):
		return AnthropicFederationValue{}, fmt.Errorf("%w: Anthropic federation secret must name one fdrl_ rule in federation_rule_id", ErrMintStoredValue)
	case !anthropicServiceAccount.MatchString(secret.ServiceAccountID):
		return AnthropicFederationValue{}, fmt.Errorf("%w: Anthropic federation secret must name one svac_ service account in service_account_id", ErrMintStoredValue)
	case !ValidServiceAccountEmail(secret.ServiceAccount):
		return AnthropicFederationValue{}, fmt.Errorf("%w: Anthropic federation secret must name one <id>@<project>.iam.gserviceaccount.com account in service_account", ErrMintStoredValue)
	case secret.WorkspaceID != "" && !anthropicWorkspaceID.MatchString(secret.WorkspaceID):
		return AnthropicFederationValue{}, fmt.Errorf("%w: Anthropic federation secret's workspace_id, when it names one, is one wrkspc_ workspace", ErrMintStoredValue)
	}
	return AnthropicFederationValue{
		OrganizationID: secret.OrganizationID, FederationRuleID: secret.FederationRuleID,
		ServiceAccountID: secret.ServiceAccountID, ServiceAccount: secret.ServiceAccount, WorkspaceID: secret.WorkspaceID,
	}, nil
}
