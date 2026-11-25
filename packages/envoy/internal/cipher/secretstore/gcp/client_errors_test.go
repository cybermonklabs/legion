package gcp_test

import (
	"context"
	"errors"
	"testing"

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
	fake := &gcptest.FakeSecretManager{Secrets: map[string]*gcptest.FakeSecret{}, FailList: status.Error(codes.PermissionDenied, "the fake refuses ListSecrets")}
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
