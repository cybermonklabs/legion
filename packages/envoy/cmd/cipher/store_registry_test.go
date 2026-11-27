// packages/envoy/cmd/cipher/store_registry_test.go
//
// agent-secrets reads the store registry and names no store: a settings body keeps each store's
// own fields, whatever store they belong to, and a store this build does not register is refused
// before any store is called.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSettingsKeepEachStoresFields: each GET /v1/settings body main answered (the records of
// internal/cipher/api/testdata, S1's) decodes into Settings with every member that is no fixed
// field of Settings kept in Fields, and encodes back to the record, member for member.
func TestSettingsKeepEachStoresFields(t *testing.T) {
	for _, store := range []string{"aws", "gcp"} {
		t.Run(store, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "cipher", "api", "testdata", "settings-recorded-on-main-"+store+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			var settings Settings
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatal(err)
			}
			fixed := map[string]bool{"store": true, "secrets_prefix": true, "google_exchange_audience": true, "secret_type_tag": true, "judge_keys": true, "approver_fields": true}
			kept := 0
			for name, value := range record {
				if fixed[name] {
					continue
				}
				kept++
				if got, ok := settings.Fields[name]; !ok || got != value {
					t.Errorf("Fields[%q] = %q (held %v), want %v", name, got, ok, value)
				}
			}
			if kept == 0 {
				t.Fatal("the record holds no per-store field, so this test checked nothing")
			}
			again, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			var encoded map[string]any
			if err := json.Unmarshal(again, &encoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(encoded, record) {
				t.Errorf("Settings encode as\n%s\nwant the record\n%s", again, raw)
			}
		})
	}
}

// TestAnUnknownStoreIsRefusedBeforeAnyCall: settings that name a store this build does not
// register (store "example-store") make each secret form exit 1, naming that store, before it
// opens any store's client: no AWS client is loaded, no Secret Manager client dialled, and no
// memory store opened.
func TestAnUnknownStoreIsRefusedBeforeAnyCall(t *testing.T) {
	opened := storeOpens(t)
	// The settings carry every field the AWS store reads, so a form that fell back to a store it
	// registers would get as far as opening that store's client, which storeOpens counts.
	settings := awsTestSettings()
	settings.Store = "example-store"
	startSecretBrokerWith(t, settings, alwaysServed)
	refuseValueReads(t)
	for _, form := range [][]string{{"list"}, {"show", "HELD_KEY"}, {"create", "NEW_KEY", "--owner", "shared", "--tier", "agent"}, {"set", "HELD_KEY"}, {"retag", "HELD_KEY", "--tier", "human"}, {"delete", "HELD_KEY"}, {"restore", "HELD_KEY"}} {
		_, stderr, code := runSecret(form...)
		if code != 1 || !strings.Contains(stderr, `unknown secret store "example-store"`) {
			t.Errorf("secret %s: exit %d, stderr %q; want exit 1 naming unknown secret store \"example-store\"", strings.Join(form, " "), code, stderr)
		}
	}
	if n := opened(); n != 0 {
		t.Errorf("the forms opened %d store clients for a store this build does not register, want none", n)
	}
}
