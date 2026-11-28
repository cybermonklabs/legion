package memory_test

import (
	"maps"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

const prefix = "example/agent-secrets/"

// TestMemoryRunsTheStoreContract runs the store contract on the memory store: a secret off the key
// is marked OffKey, and an Unfilled one carries the create label as a tag and holds no value.
func TestMemoryRunsTheStoreContract(t *testing.T) {
	secretstoretest.StoreContract(t, "memory", func(t *testing.T) secretstoretest.StoreRig {
		store := memory.New()
		return secretstoretest.StoreRig{
			Store: store, Prefix: prefix, OffKey: true, Unfilled: true,
			Put: func(_ *testing.T, f secretstoretest.Fixture) {
				s := memory.Secret{Name: prefix + f.Slug, Tags: maps.Clone(f.Tags), Value: f.Value, OffKey: f.OffKey}
				if f.Outside {
					s.Name = "x" + s.Name
				}
				if f.Unfilled {
					s.Tags[secretstore.CreateLabel] = "0123456789abcdef0123456789abcdef"
				}
				if f.Deleted {
					deleted := time.Now().UTC().Add(-time.Hour)
					s.DeletedAt = &deleted
				}
				store.Put(s)
			},
			Remove: func(_ *testing.T, slug string) { store.Delete(prefix + slug) },
		}
	})
}

// TestGetAnswersWhatPutHeld: Get answers the secret a Put holds at its name, with tags the caller
// may change without changing the store's, and nothing for a name the store does not hold.
func TestGetAnswersWhatPutHeld(t *testing.T) {
	store := memory.New(memory.Secret{Name: prefix + "deel-api-key", Tags: map[string]string{"owner": "shared", "tier": "agent"}, Value: "v"})
	got, ok := store.Get(prefix + "deel-api-key")
	if !ok || got.Value != "v" || got.Tags["owner"] != "shared" {
		t.Fatalf("Get = %+v, %t; want the secret Put held", got, ok)
	}
	got.Tags["owner"] = "changed"
	if again, _ := store.Get(prefix + "deel-api-key"); again.Tags["owner"] != "shared" {
		t.Fatalf("changing Get's tags changed the store's: %+v", again)
	}
	if _, ok := store.Get(prefix + "no-such-key"); ok {
		t.Fatal("Get of a name the store does not hold answered a secret")
	}
}
