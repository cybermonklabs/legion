package policy_test

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/groups/groupstest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// groupCurrent is the live policy over store with fake as its loader's group directory.
func groupCurrent(t *testing.T, store *secrets.Local, fake *groupstest.Directory) *policy.Current {
	t.Helper()
	loader := policytest.Loader(store)
	loader.Groups = fake
	cur, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatalf("NewCurrent: %v", err)
	}
	return cur
}

// TestEachPublishedSetCarriesTheLoadersDirectory: the set the first load makes live, and the set
// a reread that changes a secret makes live, each carry the loader's group directory, so every
// decision on a set Get answers asks the same reader; a set no Current published carries none.
func TestEachPublishedSetCarriesTheLoadersDirectory(t *testing.T) {
	store := secrets.NewLocal(policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierAgent, "v1"))
	fake := groupstest.New()
	cur := groupCurrent(t, store, fake)
	if got := cur.Get().Groups(); got != record.Groups(fake) {
		t.Fatalf("the first load's set carries groups %v, want the loader's directory", got)
	}
	store.Put(policytest.Secret("DEEL_API_KEY", "ada@example.com", policy.TierHuman, "v1"))
	if lk, err := cur.RefreshOne(t.Context(), "DEEL_API_KEY"); err != nil || lk.Secret.Tier != policy.TierHuman {
		t.Fatalf("RefreshOne(DEEL_API_KEY) = %+v, %v; want it served at human tier", lk, err)
	}
	if got := cur.Get().Groups(); got != record.Groups(fake) {
		t.Fatalf("the reread's set carries groups %v, want the loader's directory", got)
	}
	if got := policy.NewSet(map[string]policy.Secret{}).Groups(); got != nil {
		t.Fatalf("NewSet(...).Groups() = %v, want nil", got)
	}
	if got := policy.NewSetWithGroups(map[string]policy.Secret{}, fake).Groups(); got != record.Groups(fake) {
		t.Fatalf("NewSetWithGroups(..., fake).Groups() = %v, want fake", got)
	}
}
