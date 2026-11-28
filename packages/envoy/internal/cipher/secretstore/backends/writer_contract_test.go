package backends_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/backends"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// writerRigs are the rigs the writer contract runs on, by Backend.ID(): each store's writer as
// OpenWriter opens it, over its fake. A store the registry holds with no row fails
// TestEachRegisteredStoreRunsTheWriterContract.
var writerRigs = map[string]func(t *testing.T) secretstoretest.WriterRig{
	"aws":    awsWriterRig,
	"gcp":    gcpWriterRig,
	"memory": memoryWriterRig,
}

// TestEachRegisteredStoreRunsTheWriterContract runs the writer contract on every store the
// registry holds, on its row of writerRigs, and fails for a registered store with no row, so no
// store joins the registry without running the writer contract against its fake.
//
// The named revert: take a store's row out, and the test fails naming the store.
func TestEachRegisteredStoreRunsTheWriterContract(t *testing.T) {
	stores := backends.All()
	if len(stores) == 0 {
		t.Fatal("UNKNOWN: the registry holds no store, so no contract ran")
	}
	for _, b := range stores {
		rig, ok := writerRigs[b.ID()]
		if !ok {
			t.Errorf("the %s store has no row in writerRigs: give it the rig the writer contract runs it on", b.ID())
			continue
		}
		secretstoretest.WriterContract(t, b.ID(), rig)
	}
}

// awsWriterRig is the AWS writer over awstest.Local with one-shot losses and denials of its write
// calls (awsHooked), signed in to the broker's account.
func awsWriterRig(t *testing.T) secretstoretest.WriterRig {
	const (
		account = "111122223333"
		prefix  = "dev/agent-secrets/"
	)
	sm := &awsHooked{Local: awstest.NewLocal(), lose: map[string]bool{}, deny: map[string]string{}}
	awstest.Use(t, sm, account, "arn:aws:sts::"+account+":assumed-role/AWSReservedSSO_User_abc/ada@example.com")
	settings := secretstore.Settings{Store: aws.Name, Prefix: prefix, Fields: map[string]string{
		"kms_key_arn": awstest.KeyARN, "aws_account_id": account, "aws_region": "us-east-1",
	}}
	w, err := aws.Backend{}.OpenWriter(context.Background(), settings, secretstore.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.(secretstore.AccountGuard).RequireAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	return secretstoretest.WriterRig{
		Writer: w, Prefix: prefix,
		LoseNextAnswer: func(call string) { sm.mu.Lock(); sm.lose[call] = true; sm.mu.Unlock() },
		DenyNext:       func(call, message string) { sm.mu.Lock(); sm.deny[call] = message; sm.mu.Unlock() },
		Retag: func(t *testing.T, id string, tags map[string]string) {
			if _, err := sm.Local.TagResource(context.Background(), &secretsmanager.TagResourceInput{SecretId: sdkaws.String(id), Tags: aws.TagList(tags)}); err != nil {
				t.Fatal(err)
			}
		},
	}
}

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

// gcpWriterRig is the GCP writer over the fake Secret Manager, under a person's Application Default
// Credentials.
func gcpWriterRig(t *testing.T) secretstoretest.WriterRig {
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
}

// memoryWriterRig is the memory store's writer over an empty secrets file of its own; another
// writer's retag is a second writer over the same file.
func memoryWriterRig(t *testing.T) secretstoretest.WriterRig {
	const prefix = "example/agent-secrets/"
	path := filepath.Join(t.TempDir(), "memory-secrets.json")
	if err := os.WriteFile(path, []byte(`{"secrets": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() secretstore.Writer {
		w, err := memory.Backend{}.OpenWriter(context.Background(), secretstore.Settings{
			Store: memory.Name, Prefix: prefix, Fields: map[string]string{"memory_secrets_file": path},
		}, secretstore.WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	}
	w, other := open().(*memory.Writer), open()
	return secretstoretest.WriterRig{
		Writer: w, Prefix: prefix,
		LoseNextAnswer: w.LoseNext,
		DenyNext:       w.DenyNext,
		Retag: func(t *testing.T, id string, tags map[string]string) {
			t.Helper()
			held, err := other.CurrentTags(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err := other.SetTags(context.Background(), id, held, tags, nil); err != nil {
				t.Fatalf("another writer's retag: %v", err)
			}
		},
	}
}
