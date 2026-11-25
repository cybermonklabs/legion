package awstest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
)

// STS answers GetCallerIdentity with Account and ARN, the sign-in a writer's RequireAccount reads.
type STS struct{ Account, ARN string }

func (f STS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(f.Account), Arn: aws.String(f.ARN)}, nil
}

// signInVariables are the variables the AWS SDK chooses a sign-in by, which Use empties so the
// machine running the test chooses none.
var signInVariables = []string{
	"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
	"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
}

// Use makes every AWS writer opened for the rest of t write sm, signed in as arn in account, with
// no credential source named, and empties the variables that would choose a sign-in.
func Use(t *testing.T, sm awsstore.WriterAPI, account, arn string) {
	t.Helper()
	for _, name := range signInVariables {
		t.Setenv(name, "")
	}
	UseClients(t, func(context.Context, string) (awsstore.WriterAPI, awsstore.STSAPI, []aws.CredentialSource, error) {
		return sm, STS{Account: account, ARN: arn}, nil, nil
	})
}

// UseClients makes clients the Clients hook every AWS writer opened for the rest of t loads with.
func UseClients(t *testing.T, clients func(ctx context.Context, region string) (awsstore.WriterAPI, awsstore.STSAPI, []aws.CredentialSource, error)) {
	t.Helper()
	t.Cleanup(awsstore.SwapHooks(awsstore.Hooks{Clients: clients}))
}

// UseSources makes the clients every AWS writer opened for the rest of t loads report sources as
// the credential sources the SDK resolved, over the Clients hook in place.
func UseSources(t *testing.T, sources []aws.CredentialSource) {
	t.Helper()
	inner := awsstore.CurrentHooks().Clients
	UseClients(t, func(ctx context.Context, region string) (awsstore.WriterAPI, awsstore.STSAPI, []aws.CredentialSource, error) {
		sm, st, _, err := inner(ctx, region)
		return sm, st, sources, err
	})
}

// SignInOnly leaves t with no AWS source but static keys and no address setting, and one attempt
// per call: each test that calls it sets the source and the address it tests.
func SignInOnly(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_STS",
		"AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA"+"EXAMPLEKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-"+"secret")
}
