package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
)

func TestSecretTypeWritesAndDisplaysBothStores(t *testing.T) {
	onEachStore(t, func(t *testing.T, store typeStore, _ *secretBroker) {
		useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
		if _, stderr, code := runSecret("create", "TYPE_NOT_A_SECRET", "--owner", "me", "--tier", "agent", "--type", "google-login"); code != 0 {
			t.Fatalf("create --type: exit %d stderr %q", code, stderr)
		}
		tags := store.tags(t, "TYPE_NOT_A_SECRET")
		if tags["type"] != "google-login" || tags["kind"] != "" {
			t.Fatalf("stored tags = %v; want type google-login and no old classifier", tags)
		}
		for _, form := range []string{"show", "list"} {
			args := []string{form}
			if form == "show" {
				args = append(args, "TYPE_NOT_A_SECRET")
			}
			stdout, stderr, code := runSecret(args...)
			if code != 0 || !strings.Contains(stdout, "google-login") || (form == "show" && !strings.Contains(stdout, "type: google-login")) || (form == "list" && !strings.Contains(stdout, "TYPE")) {
				t.Fatalf("%s: exit %d stdout %q stderr %q", form, code, stdout, stderr)
			}
			stdout, stderr, code = runSecret(append(args, "--json")...)
			var rows []map[string]any
			if form == "show" {
				var row map[string]any
				if err := json.Unmarshal([]byte(stdout), &row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			} else if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
				t.Fatal(err)
			}
			if code != 0 || len(rows) != 1 || rows[0]["type"] != "google-login" {
				t.Fatalf("%s --json: exit %d rows %v stderr %q", form, code, rows, stderr)
			}
			if _, old := rows[0]["kind"]; old {
				t.Fatalf("%s --json retains the old classifier: %v", form, rows)
			}
		}
		store.put(t, "TYPE_NOT_A_SECRET", map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent, "kind": "azure-login"}, policytest.TypeValue(policy.TypeGoogleLogin))
		if _, stderr, code := runSecret("retag", "TYPE_NOT_A_SECRET", "--type", "google-login"); code != 0 {
			t.Fatalf("retag --type over an old tag: exit %d stderr %q", code, stderr)
		}
		if got := store.tags(t, "TYPE_NOT_A_SECRET")["type"]; got != "google-login" {
			t.Fatalf("retagged type = %q, want google-login", got)
		}
		if _, stderr, code := runSecret("retag", "TYPE_NOT_A_SECRET", "--type", "none"); code != 0 {
			t.Fatalf("retag --type none: exit %d stderr %q", code, stderr)
		}
		tags = store.tags(t, "TYPE_NOT_A_SECRET")
		if tags["type"] != "" || tags["kind"] != "" {
			t.Fatalf("removal leaves classifier tags: %v", tags)
		}
	})
}

func TestSecretOldClassifierFlagIsUnknown(t *testing.T) {
	for _, form := range []string{"create", "retag", "set", "show", "list", "delete", "restore"} {
		for _, old := range [][]string{{"--kind", "google-login"}, {"--kind=google-login"}} {
			args := append([]string{form, "TYPE_NOT_A_SECRET"}, old...)
			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "flag provided but not defined") || !strings.Contains(stderr, "--type") {
				t.Errorf("%v: exit %d stderr %q; want an unknown flag naming --type", args, code, stderr)
			}
		}
	}
}
