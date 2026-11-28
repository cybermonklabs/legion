// packages/envoy/cmd/cipher/store_read_words_test.go
//
// Two failures of a form's read through a store's own Store, held to the words main printed for
// them before the forms read through it: a list whose read of one secret's current version fails,
// and a retag whose value read finds a value that is not text. Each want is main's stderr, as
// main's code printed it for the same rig (recorded on 8ce4195034e2).
package main

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
)

// binaryValue is a awstest.Local whose every GetSecretValue answers a value held as SecretBinary,
// which is no text.
type binaryValue struct{ *awstest.Local }

func (b binaryValue) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return &secretsmanager.GetSecretValueOutput{SecretBinary: []byte{0xff}}, nil
}

func TestStoreReadFailuresKeepMainsWords(t *testing.T) {
	const gcpUsing = "the well-known credentials file (application_default_credentials.json in the directory CLOUDSDK_CONFIG names): principal unknown"
	tags := map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}
	startGCP := func(t *testing.T) *gcptest.FakeSecretManager {
		t.Helper()
		fake := gcptest.NewFakeSecretManager()
		gcptest.UseFake(t, fake)
		gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		return fake
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T) (stderr string, code int)
		want string
	}{
		{
			name: "gcp: a list whose read of one secret's version fails",
			run: func(t *testing.T) (string, int) {
				fake := startGCP(t)
				gcpTypeStore{fake}.put(t, "HELD_KEY", tags, "v1")
				fake.Mu.Lock()
				fake.RefuseMethods = map[string]codes.Code{"GetSecretVersion": codes.PermissionDenied}
				fake.Mu.Unlock()
				_, stderr, code := runSecret("list")
				return stderr, code
			},
			want: "agent-secrets secret list: using " + gcpUsing + "\n" +
				"agent-secrets secret list: read projects/100000000001/secrets/agent-secrets-held-key: Secret Manager GetSecretVersion answered PermissionDenied (its message is not printed); the store refused " + gcpUsing + "; choose another with a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME, an account in CLOUDSDK_CORE_ACCOUNT, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS (gcloud configurations here: none); looked in GCP Secret Manager project 100000000001\n",
		},
		{
			name: "aws: a retag whose value read finds no text",
			run: func(t *testing.T) (string, int) {
				local := awstest.NewLocal(awstest.Secret("HELD_KEY", "ada@example.com", policy.TierAgent, "v1"))
				startSecretBroker(t, servedBy(local))
				awstest.Use(t, binaryValue{local}, testAccount, adaSignIn)
				_, stderr, code := runSecret("retag", "HELD_KEY", "--type", policy.TypeGoogleLogin)
				return stderr, code
			},
			want: "agent-secrets secret retag: using the AWS SDK's default credential chain: " + adaSignIn + "\n" +
				"agent-secrets secret retag: read HELD_KEY's value to check it as a google-login secret's, so nothing is written: its value is not a non-empty string; looked in AWS Secrets Manager account 111122223333 in us-east-1\n",
		},
		{
			name: "gcp: a retag whose value read finds no UTF-8",
			run: func(t *testing.T) (string, int) {
				fake := startGCP(t)
				gcpTypeStore{fake}.put(t, "HELD_KEY", tags, "\xff\xfe")
				_, stderr, code := runSecret("retag", "HELD_KEY", "--type", policy.TypeGoogleLogin)
				return stderr, code
			},
			want: "agent-secrets secret retag: using " + gcpUsing + "\n" +
				"agent-secrets secret retag: read HELD_KEY's value to check it as a google-login secret's, so nothing is written: its value is not a non-empty UTF-8 string; looked in GCP Secret Manager project 100000000001\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			stderr, code := c.run(t)
			if code != 1 || stderr != c.want {
				t.Fatalf("exit %d, stderr\n%q\nwant exit 1, stderr as main printed it\n%q", code, stderr, c.want)
			}
		})
	}
}
