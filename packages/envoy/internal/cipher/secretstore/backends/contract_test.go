package backends_test

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// storeRigs are the rigs the store contract runs on, by Backend.ID(): each store over its fake. A
// store the registry holds with no row fails TestEachRegisteredStoreRunsTheContract.
var storeRigs = map[string]func(t *testing.T) secretstoretest.StoreRig{
	"aws":    awsRig,
	"gcp":    gcpRig,
	"memory": memoryRig,
}

// TestEachRegisteredStoreRunsTheContract runs the store contract on every store the registry holds,
// on its row of storeRigs, and fails for a registered store with no row, so no store joins the
// registry without running the contract against its fake.
//
// The named revert: take a store's row out, and the test fails naming the store.
func TestEachRegisteredStoreRunsTheContract(t *testing.T) {
	stores := backends.All()
	if len(stores) == 0 {
		t.Fatal("UNKNOWN: the registry holds no store, so no contract ran")
	}
	for _, b := range stores {
		rig, ok := storeRigs[b.ID()]
		if !ok {
			t.Errorf("the %s store has no row in storeRigs: give it the rig the store contract runs it on", b.ID())
			continue
		}
		secretstoretest.StoreContract(t, b.ID(), rig)
	}
}

// awsRig is the AWS store over awstest.Local, the stand-in for the Secrets Manager and KMS calls it
// reads with: a secret off the key names another key, and an Unfilled one carries the create label
// as a tag and holds no value.
func awsRig(t *testing.T) secretstoretest.StoreRig {
	const (
		prefix   = "example/agent-secrets/"
		otherKey = "arn:aws:kms:us-east-1:111122223333:key/abcd1234-12ab-34cd-56ef-1234567890ab"
	)
	local := awstest.NewLocal()
	name := func(slug string) string { return prefix + slug }
	return secretstoretest.StoreRig{
		Store: aws.Store{Client: local, KMS: local, KeyARN: awstest.KeyARN}, Prefix: prefix, OffKey: true, Unfilled: true,
		Put: func(_ *testing.T, f secretstoretest.Fixture) {
			s := awstest.LocalSecret{Name: name(f.Slug), KmsKeyID: awstest.KeyARN, Tags: maps.Clone(f.Tags), Value: f.Value}
			if f.Outside {
				s.Name = "x" + name(f.Slug)
			}
			if f.OffKey {
				s.KmsKeyID = otherKey
			}
			if f.Unfilled {
				s.Tags[secretstore.CreateLabel] = "0123456789abcdef0123456789abcdef"
			}
			if f.Deleted {
				deleted := time.Now().UTC().Add(-time.Hour)
				s.DeletedAt = &deleted
			}
			local.Put(s)
		},
		Remove: func(_ *testing.T, slug string) { local.Delete(name(slug)) },
	}
}

// gcpRig is the GCP store over the fake Secret Manager: a secret off the key is on Google's default
// one, an Unfilled one carries the create label and holds no version, and one scheduled for
// deletion has an expire_time.
func gcpRig(t *testing.T) secretstoretest.StoreRig {
	const (
		project = gcptest.Project
		prefix  = project + "/secrets/agent-secrets-"
		key     = "projects/example-project/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	)
	f := gcptest.NewFake(map[string]gcptest.Stored{})
	numbers := map[string]int{}
	return secretstoretest.StoreRig{
		Store: gcp.New(gcptest.Start(t, f), key), Prefix: prefix, OffKey: true, Unfilled: true,
		Put: func(_ *testing.T, fixture secretstoretest.Fixture) {
			name := prefix + fixture.Slug
			if fixture.Outside {
				name = project + "/secrets/x" + prefix[len(project+"/secrets/"):] + fixture.Slug
			}
			s := gcptest.Stored{
				Annotations: maps.Clone(fixture.Tags), ReplicationKey: key, VersionKey: key,
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
			if held, ok := f.Secrets[name]; !ok || string(held.Value()) != fixture.Value {
				numbers[name]++
			}
			s.Number = strconv.Itoa(numbers[name])
			f.Secrets[name] = s.Secret(name)
		},
		Remove: func(_ *testing.T, slug string) {
			f.Mu.Lock()
			defer f.Mu.Unlock()
			delete(f.Secrets, prefix+slug)
		},
	}
}

// memoryRig is the memory store: a secret off the key is marked OffKey, and an Unfilled one carries
// the create label as a tag and holds no value.
func memoryRig(*testing.T) secretstoretest.StoreRig {
	const prefix = "example/agent-secrets/"
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
}
