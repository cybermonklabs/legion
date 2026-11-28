package memory_test

import (
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

const prefix = "example/agent-secrets/"

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
