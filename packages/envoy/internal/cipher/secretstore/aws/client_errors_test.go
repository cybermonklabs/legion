package aws_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// failingCalls is a Local whose list, describe and value reads fail with err, as a client that
// words its own failures (the agent-secrets CLI's) returns them.
type failingCalls struct {
	*awstest.Local
	err error
}

func (f failingCalls) ListSecrets(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	return nil, f.err
}

func (f failingCalls) DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	return nil, f.err
}

func (f failingCalls) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return nil, f.err
}

// TestClientErrorsAnswerTheClientsFailure: a store with ClientErrors answers each failure of its
// client as the client worded it, with no prefix of its own, and a not-found one still matches
// ErrNotFound; the broker's store, without it, keeps its own words (the control).
func TestClientErrorsAnswerTheClientsFailure(t *testing.T) {
	ctx := context.Background()
	const id = "example/agent-secrets/deel-api-key"
	worded := errors.New("DescribeSecret: the store refused this sign-in")
	missing := fmt.Errorf("DescribeSecret: the store holds no such secret: %w", &types.ResourceNotFoundException{})
	calls := func(s aws.Store) map[string]error {
		_, listErr := s.List(ctx, "example/agent-secrets/", secretstore.ReadOptions{})
		_, describeErr := s.Describe(ctx, id, secretstore.ReadOptions{})
		_, readErr := s.Read(ctx, id)
		return map[string]error{"List": listErr, "Describe": describeErr, "Read": readErr}
	}
	client := aws.Store{Client: failingCalls{awstest.NewLocal(), worded}, KeyARN: awstest.KeyARN, ClientErrors: true}
	for call, err := range calls(client) {
		if err == nil || err.Error() != worded.Error() {
			t.Errorf("%s with ClientErrors = %v, want the client's %q", call, err, worded)
		}
	}
	missingClient := aws.Store{Client: failingCalls{awstest.NewLocal(), missing}, KeyARN: awstest.KeyARN, ClientErrors: true}
	for call, err := range calls(missingClient) {
		if call == "List" {
			continue
		}
		if !errors.Is(err, secretstore.ErrNotFound) || err.Error() != missing.Error() {
			t.Errorf("%s of a missing secret with ClientErrors = %v, want ErrNotFound printing %q", call, err, missing)
		}
	}
	broker := aws.Store{Client: failingCalls{awstest.NewLocal(), worded}, KeyARN: awstest.KeyARN}
	for call, err := range calls(broker) {
		if err == nil || err.Error() == worded.Error() {
			t.Errorf("%s without ClientErrors = %v, want the store's own prefix before the client's failure", call, err)
		}
	}
	if _, err := (aws.Store{Client: failingCalls{awstest.NewLocal(), missing}, KeyARN: awstest.KeyARN}).Describe(ctx, id, secretstore.ReadOptions{}); err != secretstore.ErrNotFound {
		t.Errorf("Describe of a missing secret without ClientErrors = %v, want ErrNotFound itself", err)
	}
}

// TestClientErrorsWordAValueThatIsNotText: with ClientErrors, a current value that is no text reads
// as ErrValueNotText in the CLI's words alone; without it, the broker's store names the secret
// (the control).
func TestClientErrorsWordAValueThatIsNotText(t *testing.T) {
	const source = "arn:aws:secretsmanager:us-east-1:1:secret:binary"
	binary := binaryValue{awstest.NewLocal()}
	_, err := (aws.Store{Client: binary, KeyARN: awstest.KeyARN, ClientErrors: true}).Read(context.Background(), source)
	if !errors.Is(err, secretstore.ErrValueNotText) || err.Error() != "its value is not a non-empty string" {
		t.Errorf("Read of a binary value with ClientErrors = %v, want ErrValueNotText in the CLI's words", err)
	}
	_, err = (aws.Store{Client: binary, KeyARN: awstest.KeyARN}).Read(context.Background(), source)
	if !errors.Is(err, secretstore.ErrValueNotText) || !strings.Contains(err.Error(), source) {
		t.Errorf("Read of a binary value without ClientErrors = %v, want ErrValueNotText naming the secret", err)
	}
}

// binaryValue is a awstest.Local whose every GetSecretValue answers a value held as SecretBinary.
type binaryValue struct{ *awstest.Local }

func (b binaryValue) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return &secretsmanager.GetSecretValueOutput{SecretBinary: []byte{0xff}}, nil
}
