package memory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// writeFile writes content to a memory secrets file of t's own and answers its path.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// versions is every listed secret's Version under prefix, by ID.
func versions(t *testing.T, store *memory.Store) map[string]string {
	t.Helper()
	listings, err := store.List(context.Background(), prefix, secretstore.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]string{}
	for _, ls := range listings {
		byID[ls.ID] = ls.Version
	}
	return byID
}

// TestOpenFollowsTheFile: an Open store is its file. A rewrite shows in the next List; a rewrite
// that leaves a value's bytes as they were keeps its Version, and one that changes them moves it.
func TestOpenFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	const one = `{"secrets": [{"name": "example/agent-secrets/deel-api-key", "tags": {"owner": "shared", "tier": "agent"}, "value": "first"}]}`
	writeFile(t, path, one)
	store, err := memory.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before := versions(t, store)
	deel := prefix + "deel-api-key"
	if len(before) != 1 || before[deel] == "" {
		t.Fatalf("List = %v, want deel-api-key with a version", before)
	}
	writeFile(t, path, `{"secrets": [
		{"name": "example/agent-secrets/deel-api-key", "tags": {"owner": "shared", "tier": "agent"}, "value": "first"},
		{"name": "example/agent-secrets/other-key", "tags": {"owner": "shared", "tier": "agent"}, "value": "other"}
	]}`)
	added := versions(t, store)
	if len(added) != 2 || added[deel] != before[deel] {
		t.Fatalf("after a rewrite that adds a secret, List = %v; want both, deel-api-key at %s still", added, before[deel])
	}
	writeFile(t, path, strings.Replace(one, `"first"`, `"second"`, 1))
	changed := versions(t, store)
	if len(changed) != 1 || changed[deel] == before[deel] || changed[deel] == "" {
		t.Fatalf("after a rewrite that changes its value, List = %v; want deel-api-key alone at a new version than %s", changed, before[deel])
	}
	if value, err := store.Read(context.Background(), memory.Source(deel)); err != nil || value != "second" {
		t.Fatalf("Read after the rewrite = %d bytes, %v; want the new value", len(value), err)
	}
}

// TestOpenRefusesKMSKeyID: the file the memory store replaced named each secret's KMS key; a file
// that still does is refused naming the field and what replaces it, never quoting the file.
func TestOpenRefusesKMSKeyID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	writeFile(t, path, `{"secrets": [{"name": "example/agent-secrets/deel-api-key", "kms_key_id": "arn:aws:kms:us-east-1:111122223333:key/k", "tags": {}, "value": "hunter2"}]}`)
	_, err := memory.Open(path)
	want := path + ": kms_key_id is a field of the fake secrets file the memory store replaced; take it out (off_key: true marks a secret off the agent-secrets key)"
	if err == nil || err.Error() != want {
		t.Fatalf("Open = %v, want %q", err, want)
	}
}

// TestOpenNeverQuotesTheFile: a file that does not parse is refused naming the path and where it
// failed, never its content, which may be a secret's value.
func TestOpenNeverQuotesTheFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"not-json":      `{"secrets": [{"name": "a", "value": "hunter2"`,
		"wrong-type":    `{"secrets": [{"name": "a", "value": 42, "tags": {"note": "hunter2"}}]}`,
		"unknown-field": `{"secrets": [{"name": "a", "value": "hunter2", "colour": "x"}]}`,
		"no-name":       `{"secrets": [{"value": "hunter2"}]}`,
	} {
		path := filepath.Join(dir, name+".json")
		writeFile(t, path, content)
		_, err := memory.Open(path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: Open = %v, want an error naming %s", name, err, path)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: Open = %v, which quotes the file", name, err)
		}
	}
}
