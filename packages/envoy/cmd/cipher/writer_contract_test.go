// packages/envoy/cmd/cipher/writer_contract_test.go
//
// The writer contract run on the CLI's two store writers, AWS's over awstest.Local with one-shot
// losses and denials of its write calls, and GCP's over this package's fake Secret Manager.
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"sync"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// awsHooked is awstest.Local with one-shot losses and denials of the writer's calls.
type awsHooked struct {
	*awstest.Local
	mu   sync.Mutex
	lose map[string]bool
	deny map[string]string
}

// awsCalls are the Secrets Manager operations each writer call kind makes.
var awsCalls = map[string][]string{
	"create": {"CreateSecret"}, "set": {"PutSecretValue"}, "tags": {"TagResource", "UntagResource"},
	"delete": {"DeleteSecret"}, "restore": {"RestoreSecret"},
}

func (a *awsHooked) kindOf(op string) string {
	for kind, ops := range awsCalls {
		for _, o := range ops {
			if o == op {
				return kind
			}
		}
	}
	return ""
}

// around runs apply for op, refusing it first where its kind is to be denied, and losing its
// answer after it where its kind is to be lost.
func around[T any](a *awsHooked, op string, apply func() (T, error)) (T, error) {
	var zero T
	kind := a.kindOf(op)
	a.mu.Lock()
	message, denied := a.deny[kind]
	delete(a.deny, kind)
	lost := a.lose[kind]
	delete(a.lose, kind)
	a.mu.Unlock()
	if denied {
		return zero, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: message}
	}
	out, err := apply()
	if err == nil && lost {
		return zero, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset after the call was applied")}
	}
	return out, err
}

func (a *awsHooked) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	return around(a, "CreateSecret", func() (*secretsmanager.CreateSecretOutput, error) { return a.Local.CreateSecret(ctx, in, o...) })
}
func (a *awsHooked) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	return around(a, "PutSecretValue", func() (*secretsmanager.PutSecretValueOutput, error) { return a.Local.PutSecretValue(ctx, in, o...) })
}
func (a *awsHooked) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	return around(a, "TagResource", func() (*secretsmanager.TagResourceOutput, error) { return a.Local.TagResource(ctx, in, o...) })
}
func (a *awsHooked) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	return around(a, "UntagResource", func() (*secretsmanager.UntagResourceOutput, error) { return a.Local.UntagResource(ctx, in, o...) })
}
func (a *awsHooked) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	return around(a, "DeleteSecret", func() (*secretsmanager.DeleteSecretOutput, error) { return a.Local.DeleteSecret(ctx, in, o...) })
}
func (a *awsHooked) RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	return around(a, "RestoreSecret", func() (*secretsmanager.RestoreSecretOutput, error) { return a.Local.RestoreSecret(ctx, in, o...) })
}

// TestTheBackendsHoldTheWriterContract runs the writer contract on the AWS and GCP writers
// openSecretBackend opens, each over its fake store.
func TestTheBackendsHoldTheWriterContract(t *testing.T) {
	secretstoretest.WriterContract(t, "aws", func(t *testing.T) secretstoretest.WriterRig {
		sm := &awsHooked{Local: awstest.NewLocal(), lose: map[string]bool{}, deny: map[string]string{}}
		useAWS(t, sm, testAccount, "arn:aws:sts::"+testAccount+":assumed-role/AWSReservedSSO_User_abc/ada@example.com")
		settings := Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: awstest.KeyARN, AWSAccountID: testAccount, AWSRegion: "us-east-1"}
		b, err := openSecretBackend(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.(secretstore.AccountGuard).RequireAccount(context.Background()); err != nil {
			t.Fatal(err)
		}
		return secretstoretest.WriterRig{
			Writer: b, Prefix: policytest.Prefix,
			LoseNextAnswer: func(call string) { sm.mu.Lock(); sm.lose[call] = true; sm.mu.Unlock() },
			DenyNext:       func(call, message string) { sm.mu.Lock(); sm.deny[call] = message; sm.mu.Unlock() },
			Retag: func(t *testing.T, id string, tags map[string]string) {
				var list []smtypes.Tag
				for k, v := range tags {
					list = append(list, smtypes.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
				}
				if _, err := sm.Local.TagResource(context.Background(), &secretsmanager.TagResourceInput{SecretId: awssdk.String(id), Tags: list}); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
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
