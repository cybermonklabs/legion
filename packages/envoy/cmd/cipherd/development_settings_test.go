package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
)

// TestDevelopmentSettings: the broker's development-only settings - a port-0 public URL, the
// edge's http URLs, and the nine test settings of the airlock and the judge - start with a
// development store, the memory store, and are each refused with every other store, naming the
// setting and that the store is no development store. A start that still sets
// BROKER_FAKE_SECRETS_FILE, which the memory store replaced, is refused naming what replaces it.
func TestDevelopmentSettings(t *testing.T) {
	var everyTestSetting config.Config
	for _, setting := range airlockTestSettings() {
		setting.cfg(&everyTestSetting)
	}
	stores := backends.All()
	if len(stores) == 0 {
		t.Fatal("UNKNOWN: the registry holds no store")
	}
	for _, b := range stores {
		t.Run(b.ID(), func(t *testing.T) {
			// The memory store is the one development store (its registry entry's Development);
			// every other store is a production one, whatever its entry claims.
			development := b.ID() == "memory"
			if b.Development() != development {
				t.Errorf("%s: Development() = %t, want %t: the memory store is the one development store", b.Name(), b.Development(), development)
			}
			refusals := map[string]error{
				"BROKER_PUBLIC_URL":             refusePortZeroPublicURLInProduction("http://127.0.0.1:0", b),
				"BROKER_EDGE_URL":               refuseHTTPEdgeURLsInProduction("http://edge.invalid", "", b),
				"BROKER_EDGE_MACHINE_LOGIN_URL": refuseHTTPEdgeURLsInProduction("", "http://dispatch.edge.invalid/m", b),
			}
			for _, setting := range airlockTestSettings() {
				var cfg config.Config
				setting.cfg(&cfg)
				refusals[setting.name] = refuseAirlockTestSettingsInProduction(cfg, b)
			}
			for name, err := range refusals {
				if development {
					if err != nil {
						t.Errorf("%s with the development store %s = %v, want it accepted", name, b.Name(), err)
					}
					continue
				}
				if want := "BROKER_SECRETS_STORE=" + b.Name() + " is no development store"; err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), want) {
					t.Errorf("%s with %s = %v, want a refusal naming it and %q", name, b.Name(), err, want)
				}
			}
			if development {
				if err := refuseAirlockTestSettingsInProduction(everyTestSetting, b); err != nil {
					t.Errorf("all nine test settings with %s = %v, want them accepted", b.Name(), err)
				}
			}
		})
	}
	t.Run("a start that sets BROKER_FAKE_SECRETS_FILE", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "memory-secrets.json")
		if err := os.WriteFile(file, []byte(`{"secrets": []}`), 0o600); err != nil {
			t.Fatal(err)
		}
		out := runRefusedBroker(t, buildBroker(t),
			"BROKER_DATABASE_URL=postgres://nonexistent-host-this-test-must-never-reach/db",
			"BROKER_PUBLIC_URL=https://secrets.internal.example",
			"BROKER_SECRETS_STORE=memory", "BROKER_MEMORY_SECRETS_FILE="+file,
			"BROKER_FAKE_SECRETS_FILE="+file,
		)
		for _, want := range []string{"BROKER_FAKE_SECRETS_FILE", "BROKER_SECRETS_STORE=memory", "BROKER_MEMORY_SECRETS_FILE"} {
			if !strings.Contains(out, want) {
				t.Errorf("the refused start's output does not name %s: %s", want, out)
			}
		}
	})
}
