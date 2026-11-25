package gcp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
)

// TestClientErrorsAnswerTheClientsFailure: a store WithClientErrors answers each failure of its
// client as the client returned it, never reduced to the status code, and a not-found one still
// matches ErrNotFound printing as the client did; the broker's store keeps the code alone (the
// control).
func TestClientErrorsAnswerTheClientsFailure(t *testing.T) {
	ctx := context.Background()
	fake := gcptest.NewFake(nil)
	fake.FailList = status.Error(codes.PermissionDenied, "the fake refuses ListSecrets")
	client := gcptest.Start(t, fake)
	worded := gcp.New(client, fakeKey).WithClientErrors()
	broker := gcp.New(client, fakeKey)

	_, err := worded.List(ctx, fakePrefix, secretstore.ReadOptions{})
	if err == nil || status.Code(err) != codes.PermissionDenied || err.Error() == codes.PermissionDenied.String() {
		t.Errorf("List WithClientErrors = %v, want the client's PermissionDenied as it returned it", err)
	}
	if _, err := broker.List(ctx, fakePrefix, secretstore.ReadOptions{}); err == nil || err.Error() != "list secrets under "+fakePrefix+": "+codes.PermissionDenied.String() {
		t.Errorf("List without client errors = %v, want the store's prefix and the code alone", err)
	}

	missing := fakePrefix + "no-such-key"
	_, err = worded.Describe(ctx, missing, secretstore.ReadOptions{})
	if !errors.Is(err, secretstore.ErrNotFound) || status.Code(errors.Unwrap(err)) != codes.NotFound {
		t.Errorf("Describe of a missing secret WithClientErrors = %v, want ErrNotFound printing the client's NotFound", err)
	}
	if _, err := broker.Describe(ctx, missing, secretstore.ReadOptions{}); err != secretstore.ErrNotFound {
		t.Errorf("Describe of a missing secret without client errors = %v, want ErrNotFound itself", err)
	}
	if _, err := worded.Read(ctx, missing); !errors.Is(err, secretstore.ErrNotFound) || status.Code(errors.Unwrap(err)) != codes.NotFound {
		t.Errorf("Read of a missing secret WithClientErrors = %v, want ErrNotFound printing the client's NotFound", err)
	}
}

// TestClientErrorsReadAsTheCLIReads: WithClientErrors, a List whose read of one secret's version
// fails answers a secretstore.SecretFailure naming that secret and printing as the client
// returned it, and Read makes the CLI's one AccessSecretVersion: no GetSecret, so a secret
// scheduled for deletion reads, and a value that is not UTF-8 is refused in the CLI's words. The
// broker's store, without it, fails the List with no name and refuses the deleted secret (the
// controls).
func TestClientErrorsReadAsTheCLIReads(t *testing.T) {
	ctx := context.Background()
	held, deleted, binary := fakePrefix+"held-key", fakePrefix+"deleted-key", fakePrefix+"binary-key"
	enabled := secretmanagerpb.SecretVersion_ENABLED
	fake := gcptest.NewFake(map[string]gcptest.Stored{
		held:    {DenyVersion: true, State: enabled, Value: []byte("v1")},
		deleted: {Expires: time.Now().Add(time.Hour), State: enabled, Value: []byte("v1")},
		binary:  {State: enabled, Value: []byte{0xff, 0xfe}},
	})

	client := gcptest.Start(t, fake)
	worded := gcp.New(client, fakeKey).WithClientErrors()
	broker := gcp.New(client, fakeKey)

	var one secretstore.SecretFailure
	_, err := worded.List(ctx, fakePrefix+"held", secretstore.ReadOptions{})
	if !errors.As(err, &one) || one.ID != held || status.Code(err) != codes.PermissionDenied {
		t.Errorf("List WithClientErrors = %v, want a SecretFailure naming %s with the client's PermissionDenied", err, held)
	}
	if _, err := broker.List(ctx, fakePrefix+"held", secretstore.ReadOptions{}); errors.As(err, &one) {
		t.Errorf("List without client errors = %v, want no SecretFailure", err)
	}

	fake.Mu.Lock()
	before := fake.Calls["GetSecret"]
	fake.Mu.Unlock()
	if value, err := worded.Read(ctx, deleted); err != nil || value != "v1" {
		t.Errorf("Read of a secret scheduled for deletion WithClientErrors = %q, %v; want its value", value, err)
	}
	fake.Mu.Lock()
	after := fake.Calls["GetSecret"]
	fake.Mu.Unlock()
	if after != before {
		t.Errorf("Read WithClientErrors made %d GetSecret calls, want none", after-before)
	}
	if _, err := broker.Read(ctx, deleted); !errors.Is(err, secretstore.ErrNotFound) {
		t.Errorf("Read of a secret scheduled for deletion without client errors = %v, want ErrNotFound", err)
	}
	if _, err := worded.Read(ctx, binary); !errors.Is(err, secretstore.ErrValueNotText) || err.Error() != "its value is not a non-empty UTF-8 string" {
		t.Errorf("Read of a value that is not UTF-8 WithClientErrors = %v, want ErrValueNotText in the CLI's words", err)
	}
}
