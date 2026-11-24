package secrets

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestGCPRunsTheStoreContract runs the store contract on the GCP store over the fake Secret
// Manager: a secret off the key is on Google's default one, an Unfilled one carries the create
// label and holds no version, and one scheduled for deletion has an expire_time.
func TestGCPRunsTheStoreContract(t *testing.T) {
	secretstoretest.StoreContract(t, "gcp", func(t *testing.T) secretstoretest.StoreRig {
		f := &fakeSecretManager{secrets: map[string]*fakeSecret{}}
		store := NewGCP(startFake(t, f), fakeKey)
		numbers := map[string]int{}
		return secretstoretest.StoreRig{
			Store: store, Prefix: fakePrefix, OffKey: true, Unfilled: true,
			Put: func(_ *testing.T, fixture secretstoretest.Fixture) {
				name := fakePrefix + fixture.Slug
				if fixture.Outside {
					name = fakeProject + "/secrets/x" + fakePrefix[len(fakeProject+"/secrets/"):] + fixture.Slug
				}
				s := &fakeSecret{
					annotations: maps.Clone(fixture.Tags), replicationKey: fakeKey, versionKey: fakeKey,
					state: secretmanagerpb.SecretVersion_ENABLED, value: []byte(fixture.Value),
					noVersion: fixture.Value == "",
				}
				if fixture.OffKey {
					s.replicationKey, s.versionKey = "", ""
				}
				if fixture.Unfilled {
					s.labels = map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}
				}
				if fixture.Deleted {
					s.expires = time.Now().Add(30 * 24 * time.Hour)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if held, ok := f.secrets[name]; !ok || string(held.value) != fixture.Value {
					numbers[name]++
				}
				s.number = strconv.Itoa(numbers[name])
				f.secrets[name] = s
			},
			Remove: func(_ *testing.T, slug string) {
				f.mu.Lock()
				defer f.mu.Unlock()
				delete(f.secrets, fakePrefix+slug)
			},
		}
	})
}
