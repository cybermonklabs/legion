package aws

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
)

// BROKER_SECRETS_KMS_KEY_ARN: with aws-secrets-manager, the ARN of the AWS KMS key every agent
// secret is encrypted with, arn:aws:kms:<region>:<account>:key/<key id>; required with that store
// and refused with any other. A secret under the prefix encrypted with any other key, the AWS
// managed one included, is refused.
const keyVariable = "BROKER_SECRETS_KMS_KEY_ARN"

// SettingsFields is the AWS store's own fields of GET /v1/settings.
type SettingsFields struct {
	// With aws-secrets-manager, the ARN of the agent-secrets KMS key, which every agent secret
	// must be encrypted with; absent otherwise.
	KMSKeyARN string `json:"kms_key_arn,omitempty"`
	// With aws-secrets-manager, the AWS account the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSAccountID string `json:"aws_account_id,omitempty"`
	// With aws-secrets-manager, the AWS region the agent-secrets key, and so every agent secret,
	// is in; absent otherwise.
	AWSRegion string `json:"aws_region,omitempty"`
}

// config is the AWS store's configuration: the prefix, and the agent-secrets key's ARN.
type config struct{ prefix, keyARN string }

func (c config) Store() string  { return Name }
func (c config) Prefix() string { return c.prefix }

// SecondPrefix is none: ParseConfig refuses BROKER_SECRETS_SECOND_PREFIX with this store.
func (c config) SecondPrefix() string { return "" }

// SourcePrefixes are none: a Secrets Manager source is an ARN whose region, account and closing
// suffix no prefix fixes, and the store refuses a second prefix, so the grant re-checks compare its
// sources whole.
func (c config) SourcePrefixes() (prefix, second string) { return "", "" }

// Settings are the key's ARN, with the account and region it names.
func (c config) Settings() map[string]string {
	region, account := KeyARNParts(c.keyARN)
	return settingsfields.Of(SettingsFields{KMSKeyARN: c.keyARN, AWSAccountID: account, AWSRegion: region})
}

// ParseConfig reads BROKER_SECRETS_KMS_KEY_ARN and holds the prefix to a Secrets Manager name
// prefix ending in "/", and the key to a KMS key's ARN. It refuses any
// BROKER_SECRETS_SECOND_PREFIX (secretstore.SecondPrefixVariable): a Secrets Manager source is an
// ARN whose random suffix a move to another name always changes, so no second prefix carries a
// grant across one.
func (Backend) ParseConfig(prefix string, getenv func(string) string) (secretstore.Config, error) {
	keyARN := getenv(keyVariable)
	if strings.TrimSpace(keyARN) == "" {
		return nil, fmt.Errorf("%s is required", keyVariable)
	}
	if !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.ContainsFunc(prefix, unicode.IsSpace) {
		return nil, fmt.Errorf("BROKER_SECRETS_PREFIX must be a Secrets Manager name prefix ending in /, such as production/agent-secrets/, got %q", prefix)
	}
	if !ValidKeyARN(keyARN) {
		return nil, fmt.Errorf("%s must be a KMS key ARN, arn:aws:kms:<region>:<account>:key/<key id>, got %q", keyVariable, keyARN)
	}
	if getenv(secretstore.SecondPrefixVariable) != "" {
		return nil, fmt.Errorf("%s is read only with BROKER_SECRETS_STORE=gcp-secret-manager, or the development store memory, and this broker's store is %s: an AWS Secrets Manager source is an ARN whose random suffix a move to another name always changes, so no second prefix carries a grant across one; unset it", secretstore.SecondPrefixVariable, Name)
	}
	return config{prefix: prefix, keyARN: keyARN}, nil
}
