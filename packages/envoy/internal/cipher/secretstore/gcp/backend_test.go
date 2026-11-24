package gcp_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
)

// TestParseConfigKeepsMainsChecks: the GCP store's entry refuses what config.Load refused for it on
// main, with main's messages - no key name, a prefix that names the project by its id, an AWS
// prefix or one with no trailing dash, a regional key - and answers the key's name and the
// project the prefix names as its settings fields.
func TestParseConfigKeepsMainsChecks(t *testing.T) {
	const prefix = "projects/000000000000/secrets/agent-secrets-"
	key := map[string]string{"BROKER_SECRETS_KMS_KEY_NAME": fakeKey}
	getenv := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	if _, err := (gcp.Backend{}).ParseConfig(prefix, getenv(nil)); err == nil || err.Error() != "BROKER_SECRETS_KMS_KEY_NAME is required" {
		t.Fatalf("ParseConfig with no key = %v, want BROKER_SECRETS_KMS_KEY_NAME is required", err)
	}
	for _, bad := range []string{"projects/example-project/secrets/agent-secrets-", "example/agent-secrets/", "projects/000000000000/secrets/agent-secrets"} {
		_, err := gcp.Backend{}.ParseConfig(bad, getenv(key))
		if want := "BROKER_SECRETS_PREFIX: a Google Secret Manager prefix is projects/<project number>/secrets/"; err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("ParseConfig(prefix %q) = %v, want %q…", bad, err, want)
		}
	}
	_, err := gcp.Backend{}.ParseConfig(prefix, getenv(map[string]string{"BROKER_SECRETS_KMS_KEY_NAME": "projects/example-project/locations/us-west1/keyRings/r/cryptoKeys/k"}))
	if want := "BROKER_SECRETS_KMS_KEY_NAME: a Cloud KMS key name is projects/"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("ParseConfig(a regional key) = %v, want %q…", err, want)
	}
	cfg, err := gcp.Backend{}.ParseConfig(prefix, getenv(key))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kms_key_name": fakeKey, "gcp_project": "000000000000"}
	if cfg.Store() != gcp.Name || cfg.Prefix() != prefix || !maps.Equal(cfg.Settings(), want) {
		t.Fatalf("Config = %s %s %v; want %s, the prefix, and %v", cfg.Store(), cfg.Prefix(), cfg.Settings(), gcp.Name, want)
	}
	if b := (gcp.Backend{}); b.RereadBudget() != gcp.RereadBudget || b.Development() || b.ID() != "gcp" {
		t.Fatalf("gcp.Backend = %s budget %+v development %t; want gcp, gcp.RereadBudget, no development store", b.ID(), b.RereadBudget(), b.Development())
	}
}
