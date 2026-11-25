// packages/envoy/cmd/cipher/writer_contract_test.go
//
// The writer contract run on the CLI's GCP store writer, over this package's fake Secret Manager.
// The AWS writer's run is its own package's (TestAWSRunsTheWriterContract).
package main

import (
	"context"
	"fmt"
	"maps"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestTheBackendsHoldTheWriterContract runs the writer contract on the GCP writer
// openSecretBackend opens over the fake Secret Manager.
func TestTheBackendsHoldTheWriterContract(t *testing.T) {
	secretstoretest.WriterContract(t, "gcp", func(t *testing.T) secretstoretest.WriterRig {
		fake := newFakeSecretManager()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.UnaryInterceptor(fake.recordAuthorization))
		secretmanagerpb.RegisterSecretManagerServiceServer(server, fake)
		go server.Serve(lis)
		t.Cleanup(server.Stop)
		useGCPDialTo(t, lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		b, err := openSecretBackend(context.Background(), gcpTestSettings())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		methods := map[string]string{"create": "CreateSecret", "set": "AddSecretVersion", "tags": "UpdateSecret", "delete": "UpdateSecret", "restore": "UpdateSecret"}
		var once sync.Mutex
		fake.beforeCall = func(method string) {
			once.Lock()
			defer once.Unlock()
			fake.mu.Lock()
			delete(fake.answerAfter, method)
			delete(fake.refuseMethods, method)
			fake.mu.Unlock()
		}
		return secretstoretest.WriterRig{
			Writer: b, Prefix: testGCPPrefix,
			LoseNextAnswer: func(call string) {
				fake.mu.Lock()
				if fake.answerAfter == nil {
					fake.answerAfter = map[string]codes.Code{}
				}
				fake.answerAfter[methods[call]] = codes.Unavailable
				fake.mu.Unlock()
			},
			DenyNext: func(call, message string) {
				fake.mu.Lock()
				if fake.refuseMethods == nil {
					fake.refuseMethods = map[string]codes.Code{}
				}
				fake.refuseMethods[methods[call]] = codes.PermissionDenied
				fake.refuseText = ": " + message
				fake.mu.Unlock()
			},
			Retag: func(t *testing.T, id string, tags map[string]string) {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				entry := fake.secrets[id]
				annotations := maps.Clone(entry.secret.GetAnnotations())
				maps.Copy(annotations, tags)
				entry.secret.Annotations = annotations
				entry.etagCounter++
				entry.secret.Etag = fmt.Sprint(entry.etagCounter)
			},
		}
	})
}
