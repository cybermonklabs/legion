package gcp

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
)

// BROKER_SECRETS_KMS_KEY_NAME: with gcp-secret-manager, the resource name of the Cloud KMS key
// every agent secret is encrypted with,
// projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key> (global, since a secret
// with automatic replication takes a global key); required with that store and refused with any
// other. A secret under the prefix encrypted with any other key, Google's default one included,
// is refused.
const keyVariable = "BROKER_SECRETS_KMS_KEY_NAME"

// SettingsFields is the GCP store's own fields of GET /v1/settings.
type SettingsFields struct {
	// With gcp-secret-manager, the resource name of the agent-secrets Cloud KMS key, which every
	// agent secret must be encrypted with; absent otherwise.
	KMSKeyName string `json:"kms_key_name,omitempty"`
	// With gcp-secret-manager, the number of the project every agent secret is in, the one the
	// prefix names; absent otherwise.
	GCPProject string `json:"gcp_project,omitempty"`
}

// config is the GCP store's configuration: the prefix, the second prefix a move leaves or reaches
// ("" for none), and the agent-secrets key's resource name.
type config struct{ prefix, second, keyName string }

func (c config) Store() string        { return Name }
func (c config) Prefix() string       { return c.prefix }
func (c config) SecondPrefix() string { return c.second }

// Settings are the key's name and the project the prefix names.
func (c config) Settings() map[string]string {
	return settingsfields.Of(SettingsFields{KMSKeyName: c.keyName, GCPProject: Project(c.prefix)})
}

// ParseConfig reads BROKER_SECRETS_KMS_KEY_NAME and holds the prefix to
// projects/<project number>/secrets/<secret-id prefix>-, and the key to a global Cloud KMS key's
// resource name. A Secret Manager source is the secret's resource name, which a move to another
// prefix keeps but for the prefix, so the store reads BROKER_SECRETS_SECOND_PREFIX
// (secretstore.SecondPrefixVariable) and holds a set one to the prefix's form.
func (Backend) ParseConfig(prefix string, getenv func(string) string) (secretstore.Config, error) {
	keyName := getenv(keyVariable)
	if strings.TrimSpace(keyName) == "" {
		return nil, fmt.Errorf("%s is required", keyVariable)
	}
	if err := ValidPrefix(prefix); err != nil {
		return nil, fmt.Errorf("BROKER_SECRETS_PREFIX: %w", err)
	}
	if err := ValidKeyName(keyName); err != nil {
		return nil, fmt.Errorf("%s: %w", keyVariable, err)
	}
	second := getenv(secretstore.SecondPrefixVariable)
	if second != "" {
		if err := ValidPrefix(second); err != nil {
			return nil, fmt.Errorf("%s: %w", secretstore.SecondPrefixVariable, err)
		}
	}
	return config{prefix: prefix, second: second, keyName: keyName}, nil
}
