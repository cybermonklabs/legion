package memory_test

import (
	"context"
	"maps"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// TestParseConfigNeedsTheFile: the memory store reads its secrets from the file
// BROKER_MEMORY_SECRETS_FILE names, so a start with none is refused naming it.
func TestParseConfigNeedsTheFile(t *testing.T) {
	_, err := memory.Backend{}.ParseConfig(prefix, func(string) string { return "" })
	if want := "BROKER_MEMORY_SECRETS_FILE is required with BROKER_SECRETS_STORE=memory"; err == nil || err.Error() != want {
		t.Fatalf("ParseConfig with no file = %v, want %q", err, want)
	}
}

// TestTheBackendOpensItsFile: the memory store's entry parses the file's path into its one
// settings field, opens the store over that file, and is the one development store.
func TestTheBackendOpensItsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	writeFile(t, path, `{"secrets": [{"name": "example/agent-secrets/deel-api-key", "tags": {"owner": "shared", "tier": "agent"}, "value": "v"}]}`)
	b := memory.Backend{}
	if b.Name() != memory.Name || b.ID() != "memory" || !b.Development() || len(b.ServiceSDKs()) != 0 {
		t.Fatalf("memory.Backend = %s/%s development %t SDKs %v; want memory/memory, the development store, no SDK", b.Name(), b.ID(), b.Development(), b.ServiceSDKs())
	}
	if b.RereadBudget() != secretstore.DefaultRereadBudget {
		t.Fatalf("RereadBudget = %+v, want the default", b.RereadBudget())
	}
	env := map[string]string{"BROKER_MEMORY_SECRETS_FILE": path}
	cfg, err := b.ParseConfig(prefix, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store() != memory.Name || cfg.Prefix() != prefix || !maps.Equal(cfg.Settings(), map[string]string{"memory_secrets_file": path}) {
		t.Fatalf("Config = %s %s %v; want memory, the prefix and the file's path", cfg.Store(), cfg.Prefix(), cfg.Settings())
	}
	opened, err := b.OpenStore(context.Background(), cfg, secretstore.OpenEnv{})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Credentials != "memory secrets file" {
		t.Fatalf("Credentials = %q, want memory secrets file", opened.Credentials)
	}
	if value, err := opened.Store.Read(context.Background(), memory.Source(prefix+"deel-api-key")); err != nil || value != "v" {
		t.Fatalf("the opened store reads %d bytes, %v; want the file's value", len(value), err)
	}
}
