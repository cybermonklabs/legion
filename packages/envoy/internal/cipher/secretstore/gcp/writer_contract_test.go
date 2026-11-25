package gcp_test

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestGCPRunsTheWriterContract runs the writer contract on the GCP writer OpenWriter opens, over
// the fake Secret Manager, under a person's Application Default Credentials.
func TestGCPRunsTheWriterContract(t *testing.T) {
	secretstoretest.WriterContract(t, "gcp", func(t *testing.T) secretstoretest.WriterRig {
		fake := gcptest.NewFakeSecretManager()
		gcptest.UseFake(t, fake)
		gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
		w, err := gcp.Backend{}.OpenWriter(context.Background(), gcptest.Settings(), secretstore.WriterOptions{Run: gcptest.Run})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		methods := map[string]string{"create": "CreateSecret", "set": "AddSecretVersion", "tags": "UpdateSecret", "delete": "UpdateSecret", "restore": "UpdateSecret"}
		var once sync.Mutex
		fake.BeforeCall = func(method string) {
			once.Lock()
			defer once.Unlock()
			fake.Mu.Lock()
			delete(fake.AnswerAfter, method)
			delete(fake.RefuseMethods, method)
			fake.Mu.Unlock()
		}
		return secretstoretest.WriterRig{
			Writer: w, Prefix: gcptest.SettingsPrefix,
			LoseNextAnswer: func(call string) {
				fake.Mu.Lock()
				if fake.AnswerAfter == nil {
					fake.AnswerAfter = map[string]codes.Code{}
				}
				fake.AnswerAfter[methods[call]] = codes.Unavailable
				fake.Mu.Unlock()
			},
			DenyNext: func(call, message string) {
				fake.Mu.Lock()
				if fake.RefuseMethods == nil {
					fake.RefuseMethods = map[string]codes.Code{}
				}
				fake.RefuseMethods[methods[call]] = codes.PermissionDenied
				fake.RefuseText = ": " + message
				fake.Mu.Unlock()
			},
			Retag: func(t *testing.T, id string, tags map[string]string) {
				fake.Mu.Lock()
				defer fake.Mu.Unlock()
				entry := fake.Secrets[id]
				annotations := maps.Clone(entry.Secret.GetAnnotations())
				maps.Copy(annotations, tags)
				entry.Secret.Annotations = annotations
				entry.EtagCounter++
				entry.Secret.Etag = fmt.Sprint(entry.EtagCounter)
			},
		}
	})
}
