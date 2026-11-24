package aws_test

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/awssdk"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// env is a getenv over vars.
func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// TestParseConfigKeepsMainsChecks: the AWS store's entry refuses what config.Load refused for it on
// main, with main's messages - no key, a prefix that is no Secrets Manager name prefix ending in
// "/", a key named any way but by its ARN - and answers the key's ARN, account and region as its
// settings fields.
func TestParseConfigKeepsMainsChecks(t *testing.T) {
	const prefix = "example/agent-secrets/"
	if _, err := (aws.Backend{}).ParseConfig(prefix, env(nil)); err == nil || err.Error() != "BROKER_SECRETS_KMS_KEY_ARN is required" {
		t.Fatalf("ParseConfig with no key = %v, want BROKER_SECRETS_KMS_KEY_ARN is required", err)
	}
	for _, bad := range []string{"example/agent-secrets", "/example/agent-secrets/", "example/agent secrets/"} {
		_, err := aws.Backend{}.ParseConfig(bad, env(map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": awstest.KeyARN}))
		if want := "BROKER_SECRETS_PREFIX must be a Secrets Manager name prefix ending in /, such as production/agent-secrets/, got"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ParseConfig(prefix %q) = %v, want %q…", bad, err, want)
		}
	}
	for _, key := range []string{"alias/agent-secrets", "1234abcd-12ab-34cd-56ef-1234567890ab", "arn:aws:kms:us-east-1:111122223333:alias/agent-secrets"} {
		_, err := aws.Backend{}.ParseConfig(prefix, env(map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": key}))
		if want := "BROKER_SECRETS_KMS_KEY_ARN must be a KMS key ARN, arn:aws:kms:<region>:<account>:key/<key id>, got"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ParseConfig(key %q) = %v, want %q…", key, err, want)
		}
	}
	cfg, err := aws.Backend{}.ParseConfig(prefix, env(map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": awstest.KeyARN}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kms_key_arn": awstest.KeyARN, "aws_account_id": "111122223333", "aws_region": "us-east-1"}
	if cfg.Store() != aws.Name || cfg.Prefix() != prefix || !maps.Equal(cfg.Settings(), want) {
		t.Fatalf("Config = %s %s %v; want %s, the prefix, and %v", cfg.Store(), cfg.Prefix(), cfg.Settings(), aws.Name, want)
	}
}

// TestOpenStoreLogsThroughTheBrokersLogger: the store the AWS entry opens reads under the AWS
// configuration whose logger is the broker's own (awssdk.Log), and names its credentials aws-sdk.
func TestOpenStoreLogsThroughTheBrokersLogger(t *testing.T) {
	for _, variable := range os.Environ() {
		if name, _, _ := strings.Cut(variable, "="); strings.HasPrefix(name, "AWS_") {
			t.Setenv(name, "")
		}
	}
	none := filepath.Join(t.TempDir(), "none")
	t.Setenv("AWS_CONFIG_FILE", none)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", none)
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	cfg, err := aws.Backend{}.ParseConfig("example/agent-secrets/", env(map[string]string{"BROKER_SECRETS_KMS_KEY_ARN": awstest.KeyARN}))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := aws.Backend{}.OpenStore(t.Context(), cfg, secretstore.OpenEnv{})
	if err != nil {
		t.Fatal(err)
	}
	store, ok := opened.Store.(aws.Store)
	if !ok || opened.Credentials != "aws-sdk" || store.KeyARN != awstest.KeyARN {
		t.Fatalf("OpenStore = %T %+v, credentials %q; want an aws.Store on the key, credentials aws-sdk", opened.Store, opened.Store, opened.Credentials)
	}
	client, ok := store.Client.(*secretsmanager.Client)
	if !ok {
		t.Fatalf("the store reads through %T, want Secrets Manager's own client", store.Client)
	}
	if logger := client.Options().Logger; logger == nil {
		t.Fatal("the store's Secrets Manager client has no logger")
	} else if _, ok := logger.(awssdk.Log); !ok {
		t.Fatalf("the store's Secrets Manager client logs through %T, want awssdk.Log", logger)
	}
}
