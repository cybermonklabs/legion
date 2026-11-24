package memory_test

import (
	"context"
	"errors"
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

// TestFailNextFailsOneCall: FailNext refuses the next call of its kind alone, with the error it is
// given, and leaves the other kinds alone.
func TestFailNextFailsOneCall(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	store := memory.New(memory.Secret{Name: prefix + "deel-api-key", Tags: map[string]string{"owner": "shared", "tier": "agent"}, Value: "v"})
	for _, call := range []string{"list", "describe", "read"} {
		store.FailNext(call, boom)
	}
	do := map[string]func() error{
		"list": func() error { _, err := store.List(ctx, prefix, secretstore.ReadOptions{}); return err },
		"describe": func() error {
			_, err := store.Describe(ctx, prefix+"deel-api-key", secretstore.ReadOptions{})
			return err
		},
		"read": func() error { _, err := store.Read(ctx, memory.Source(prefix+"deel-api-key")); return err },
	}
	for call, f := range do {
		if err := f(); !errors.Is(err, boom) {
			t.Errorf("the first %s after FailNext = %v, want boom", call, err)
		}
		if err := f(); err != nil {
			t.Errorf("the second %s after FailNext = %v, want it answered", call, err)
		}
	}
}

// TestFailHoldsUntilCleared: Fail refuses every later call of its kind until Fail(call, nil), and
// Calls counts the refused calls with the answered ones.
func TestFailHoldsUntilCleared(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	source := memory.Source(prefix + "deel-api-key")
	store := memory.New(memory.Secret{Name: prefix + "deel-api-key", Tags: map[string]string{"owner": "shared", "tier": "agent"}, Value: "v"})
	store.Fail("read", boom)
	for range 3 {
		if _, err := store.Read(ctx, source); !errors.Is(err, boom) {
			t.Fatalf("Read under Fail = %v, want boom", err)
		}
	}
	store.Fail("read", nil)
	if value, err := store.Read(ctx, source); err != nil || value != "v" {
		t.Fatalf("Read after Fail(read, nil) = %q, %v; want the value", value, err)
	}
	if got := store.Calls("read"); got != 4 {
		t.Fatalf("Calls(read) = %d, want 4: three refused and one answered", got)
	}
	if got := store.Calls(""); got != 4 {
		t.Fatalf("Calls(\"\") = %d, want every call, 4", got)
	}
}
