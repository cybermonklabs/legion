package api_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
)

// gcpKeyName is the Cloud KMS key the GCP store of these tests is on, and awsKeyARN the AWS KMS
// key the AWS store's settings name (an AWS documentation account's).
const (
	gcpKeyName = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	awsKeyARN  = "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
)

// storeEnv is one store's configuration in these tests, by its registry ID: its prefix, the
// variables its registry entry parses, and the settings field that names its key or file.
type storeEnv struct {
	prefix        string
	env           map[string]string
	keyField, key string
}

var storeEnvs = map[string]storeEnv{
	"aws":    {policytest.Prefix, map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": awsKeyARN}, "kms_key_arn", awsKeyARN},
	"gcp":    {"projects/000000000000/secrets/agent-secrets-", map[string]string{"BROKER_SECRETS_KMS_KEY_NAME": gcpKeyName}, "kms_key_name", gcpKeyName},
	"memory": {policytest.Prefix, map[string]string{"BROKER_MEMORY_SECRETS_FILE": "memory-secrets.json"}, "memory_secrets_file", "memory-secrets.json"},
}

// storeDeps is the Deps fields of the store whose registry ID is id, configured as storeEnvs
// holds it, as cipherd fills them: SecretsStore, SecretsPrefix and SettingsFields. A store the
// registry holds and storeEnvs does not fails as UNKNOWN, so a new store gets its row.
func storeDeps(t *testing.T, id string) api.Deps {
	t.Helper()
	e, ok := storeEnvs[id]
	if !ok {
		t.Fatalf("UNKNOWN: no settings environment for the store %s; add one to storeEnvs", id)
	}
	for _, b := range backends.All() {
		if b.ID() != id {
			continue
		}
		cfg, err := b.ParseConfig(e.prefix, func(name string) string { return e.env[name] })
		if err != nil {
			t.Fatalf("%s: ParseConfig: %v", id, err)
		}
		return api.Deps{SecretsStore: b.Name(), SecretsPrefix: cfg.Prefix(), SettingsFields: cfg.Settings()}
	}
	t.Fatalf("the registry holds no store %s", id)
	return api.Deps{}
}

// awsSettings is the AWS store's settings fields on the test key (storeDeps).
func awsSettings(t *testing.T) map[string]string {
	t.Helper()
	return storeDeps(t, "aws").SettingsFields
}
