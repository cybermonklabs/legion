package gcp_test

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestGCPRunsTheStoreContract runs the store contract on the GCP store over the fake Secret
// Manager: a secret off the key is on Google's default one, an Unfilled one carries the create
// label and holds no version, and one scheduled for deletion has an expire_time.
func TestGCPRunsTheStoreContract(t *testing.T) {
	secretstoretest.StoreContract(t, "gcp", func(t *testing.T) secretstoretest.StoreRig {
		f := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{}}
		store := gcp.New(gcptest.Start(t, f), fakeKey)
		numbers := map[string]int{}
		return secretstoretest.StoreRig{
			Store: store, Prefix: fakePrefix, OffKey: true, Unfilled: true,
			Put: func(_ *testing.T, fixture secretstoretest.Fixture) {
				name := fakePrefix + fixture.Slug
				if fixture.Outside {
					name = fakeProject + "/secrets/x" + fakePrefix[len(fakeProject+"/secrets/"):] + fixture.Slug
				}
				s := &gcptest.FakeSecret{
					Annotations: maps.Clone(fixture.Tags), ReplicationKey: fakeKey, VersionKey: fakeKey,
					State: secretmanagerpb.SecretVersion_ENABLED, Value: []byte(fixture.Value),
					NoVersion: fixture.Value == "",
				}
				if fixture.OffKey {
					s.ReplicationKey, s.VersionKey = "", ""
				}
				if fixture.Unfilled {
					s.Labels = map[string]string{secretstore.CreateLabel: "0123456789abcdef0123456789abcdef"}
				}
				if fixture.Deleted {
					s.Expires = time.Now().Add(30 * 24 * time.Hour)
				}
				f.Mu.Lock()
				defer f.Mu.Unlock()
				if held, ok := f.Secrets[name]; !ok || string(held.Value) != fixture.Value {
					numbers[name]++
				}
				s.Number = strconv.Itoa(numbers[name])
				f.Secrets[name] = s
			},
			Remove: func(_ *testing.T, slug string) {
				f.Mu.Lock()
				defer f.Mu.Unlock()
				delete(f.Secrets, fakePrefix+slug)
			},
		}
	})
}
