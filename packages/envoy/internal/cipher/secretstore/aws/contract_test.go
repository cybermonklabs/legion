package aws

import (
	"maps"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestAWSRunsTheStoreContract runs the store contract on the AWS store over awstest.Local, the
// stand-in for the Secrets Manager and KMS calls it reads with: a secret off the key names another
// key, and an Unfilled one carries the create label as a tag and holds no value.
func TestAWSRunsTheStoreContract(t *testing.T) {
	const (
		prefix   = "example/agent-secrets/"
		otherKey = "arn:aws:kms:us-east-1:111122223333:key/abcd1234-12ab-34cd-56ef-1234567890ab"
	)
	secretstoretest.StoreContract(t, "aws", func(t *testing.T) secretstoretest.StoreRig {
		local := awstest.NewLocal()
		name := func(slug string) string { return prefix + slug }
		return secretstoretest.StoreRig{
			Store: Store{Client: local, KMS: local, KeyARN: awstest.KeyARN}, Prefix: prefix, OffKey: true, Unfilled: true,
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
	})
}
