package aws_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
)

// bodyRecorder is an HTTP client standing in for AWS: it answers every request 200 with an empty
// JSON object, and records, for each, whether the body net/http would have written had a WriteTo
// of its own (io.WriterTo), the method smithy-go's request body answers io.EOF from once it is
// closed.
type bodyRecorder struct {
	mu       sync.Mutex
	writeTos []bool
}

func (r *bodyRecorder) Do(request *http.Request) (*http.Response, error) {
	if request.Body != nil && request.Body != http.NoBody {
		_, writeTo := request.Body.(io.WriterTo)
		r.mu.Lock()
		r.writeTos = append(r.writeTos, writeTo)
		r.mu.Unlock()
		_, _ = io.Copy(io.Discard, request.Body)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/x-amz-json-1.1"}},
		Body: io.NopCloser(strings.NewReader("{}")), Request: request}, nil
}

// TestTheStoresClientsSendEachBodyThroughAWSBody: the Secrets Manager and KMS clients of the store
// the broker opens (storeOn) hand net/http each request body with only Read and Close, as
// awsbody.Client does, so an answered call over HTTP/1.1 is not failed by smithy-go closing the body
// early.
//
// The named revert: build either client without awsbody.Client, and its call's body has a WriteTo.
func TestTheStoresClientsSendEachBodyThroughAWSBody(t *testing.T) {
	recorder := &bodyRecorder{}
	store := awsstore.StoreOn(sdkaws.Config{
		Region:     "us-east-1",
		HTTPClient: recorder,
		Credentials: sdkaws.CredentialsProviderFunc(func(context.Context) (sdkaws.Credentials, error) {
			return sdkaws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
		}),
	}, awstest.KeyARN)
	ctx := context.Background()
	if _, err := store.Client.(*secretsmanager.Client).ListSecrets(ctx, &secretsmanager.ListSecretsInput{}); err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if _, err := store.KMS.(*kms.Client).ListAliases(ctx, &kms.ListAliasesInput{}); err != nil {
		t.Fatalf("ListAliases: %v", err)
	}
	if len(recorder.writeTos) != 2 {
		t.Fatalf("the stand-in saw %d request bodies, want one for each of the two calls", len(recorder.writeTos))
	}
	for i, call := range []string{"Secrets Manager's ListSecrets", "KMS's ListAliases"} {
		if recorder.writeTos[i] {
			t.Errorf("%s sent a body with a WriteTo of its own; want awsbody.Client's plain io.ReadCloser", call)
		}
	}
}

// TestTheWritersClientsSendEachBodyThroughAWSBody: the Secrets Manager and STS clients agent-secrets
// writes and signs in with (writerClientsOn) hand net/http each request body with only Read and
// Close, as the store's own clients do.
//
// The named revert: build either client without awsbody.Client, and its call's body has a WriteTo.
func TestTheWritersClientsSendEachBodyThroughAWSBody(t *testing.T) {
	recorder := &bodyRecorder{}
	writer, caller := awsstore.WriterClientsOn(sdkaws.Config{
		Region:     "us-east-1",
		HTTPClient: recorder,
		Credentials: sdkaws.CredentialsProviderFunc(func(context.Context) (sdkaws.Credentials, error) {
			return sdkaws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
		}),
	})
	ctx := context.Background()
	if _, err := writer.ListSecrets(ctx, &secretsmanager.ListSecretsInput{}); err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	// The stand-in answers JSON, which STS's query protocol does not parse: the call fails after the
	// body it sent is recorded, and the body is what this test reads.
	_, _ = caller.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if len(recorder.writeTos) != 2 {
		t.Fatalf("the stand-in saw %d request bodies, want one for each of the two calls", len(recorder.writeTos))
	}
	for i, call := range []string{"Secrets Manager's ListSecrets", "STS's GetCallerIdentity"} {
		if recorder.writeTos[i] {
			t.Errorf("%s sent a body with a WriteTo of its own; want awsbody.Client's plain io.ReadCloser", call)
		}
	}
}
