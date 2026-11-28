package aws_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/secretstoretest"
)

// TestAWSRunsTheWriterContract runs the writer contract on the AWS writer OpenWriter opens, over
// awstest.Local with one-shot losses and denials of its write calls (awsHooked).
func TestAWSRunsTheWriterContract(t *testing.T) {
	const account = "111122223333"
	prefix := "dev/agent-secrets/"
	secretstoretest.WriterContract(t, "aws", func(t *testing.T) secretstoretest.WriterRig {
		sm := &awsHooked{Local: awstest.NewLocal(), lose: map[string]bool{}, deny: map[string]string{}}
		awstest.Use(t, sm, account, "arn:aws:sts::"+account+":assumed-role/AWSReservedSSO_User_abc/ada@example.com")
		settings := secretstore.Settings{Store: awsstore.Name, Prefix: prefix, Fields: map[string]string{
			"kms_key_arn": awstest.KeyARN, "aws_account_id": account, "aws_region": "us-east-1",
		}}
		w, err := awsstore.Backend{}.OpenWriter(context.Background(), settings, secretstore.WriterOptions{})
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
				if _, err := sm.Local.TagResource(context.Background(), &secretsmanager.TagResourceInput{SecretId: aws.String(id), Tags: awsstore.TagList(tags)}); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
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
