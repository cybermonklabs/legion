package onepassword

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
)

// BROKER_ONEPASSWORD_VAULT: with onepassword, the id of the 1Password vault whose items are the
// agent secrets (26 lowercase letters and digits, as `op vault list` prints it); required with
// that store and refused with any other. Each secret is an API Credential item there, titled
// BROKER_SECRETS_PREFIX and its name, its value the credential field, and its tags the fields of
// its section titled cipher.
const vaultVariable = "BROKER_ONEPASSWORD_VAULT"

// BROKER_ONEPASSWORD_TOKEN_FILE: with onepassword, the file holding the token of the 1Password
// service account the broker reads the vault with, read once at boot; required with that store and
// refused with any other. The broker only reads, so the account needs only read access to the
// vault.
const tokenFileVariable = "BROKER_ONEPASSWORD_TOKEN_FILE"

// BROKER_ONEPASSWORD_DAILY_REQUESTS: with onepassword, how many 1Password requests the whole
// deployment, every replica together, may make in a moving 24 hours; required with that store
// and refused with any other, with no default, since the account's type and the other service
// accounts that share its daily limit decide it (an Individual or Families account gives all its
// service accounts 1,000 together). Lists and rereads hold at four fifths of it, answering their
// last result; value reads spend the last fifth and are never held.
const dailyRequestsVariable = "BROKER_ONEPASSWORD_DAILY_REQUESTS"

// The range BROKER_ONEPASSWORD_DAILY_REQUESTS takes.
const (
	minDailyRequests = 10
	maxDailyRequests = 1000000
)

// vaultPattern is a 1Password vault id.
var vaultPattern = regexp.MustCompile(`^[a-z0-9]{26}$`)

// prefixPattern is a title prefix the store lists under: lowercase letters, digits and hyphens,
// ending in a hyphen, so the prefix and a secret's slug make one slug-shaped title.
var prefixPattern = regexp.MustCompile(`^[a-z0-9-]*-$`)

// maxPrefixLength is the longest BROKER_SECRETS_PREFIX the store takes, which leaves a slug most of
// a title's 255 bytes.
const maxPrefixLength = 64

// SettingsFields is the 1Password store's own fields of GET /v1/settings.
type SettingsFields struct {
	// With onepassword, the id of the vault whose items are the agent secrets, which agent-secrets
	// writes; absent otherwise.
	Vault string `json:"onepassword_vault,omitempty"`
}

// config is the 1Password store's configuration.
type config struct {
	prefix, vault, tokenFile string
	dailyRequests            int
}

func (c config) Store() string  { return Name }
func (c config) Prefix() string { return c.prefix }

// Settings is the vault's id.
func (c config) Settings() map[string]string {
	return settingsfields.Of(SettingsFields{Vault: c.vault})
}

// ParseConfig reads the store's three variables, each required, and holds the prefix to a title
// prefix of lowercase letters, digits and hyphens ending in -, at most 64 bytes.
func (Backend) ParseConfig(prefix string, getenv func(string) string) (secretstore.Config, error) {
	vault, tokenFile, daily := getenv(vaultVariable), getenv(tokenFileVariable), getenv(dailyRequestsVariable)
	for _, required := range []struct{ name, value string }{{vaultVariable, vault}, {tokenFileVariable, tokenFile}} {
		if required.value == "" {
			return nil, fmt.Errorf("%s is required with BROKER_SECRETS_STORE=%s", required.name, Name)
		}
	}
	if daily == "" {
		return nil, fmt.Errorf("%s is required with BROKER_SECRETS_STORE=%s: the account's type and its other service accounts decide it", dailyRequestsVariable, Name)
	}
	if !vaultPattern.MatchString(vault) {
		return nil, fmt.Errorf("%s must be a 1Password vault id, 26 lowercase letters and digits, got %q", vaultVariable, vault)
	}
	requests, err := strconv.Atoi(daily)
	if err != nil || requests < minDailyRequests || requests > maxDailyRequests {
		return nil, fmt.Errorf("%s must be a whole number from %d to %d, got %q", dailyRequestsVariable, minDailyRequests, maxDailyRequests, daily)
	}
	if !prefixPattern.MatchString(prefix) || len(prefix) > maxPrefixLength {
		return nil, fmt.Errorf("BROKER_SECRETS_PREFIX must be a 1Password title prefix of lowercase letters, digits and hyphens ending in -, at most %d bytes, such as cipher-, got %q", maxPrefixLength, prefix)
	}
	return config{prefix: prefix, vault: vault, tokenFile: tokenFile, dailyRequests: requests}, nil
}
