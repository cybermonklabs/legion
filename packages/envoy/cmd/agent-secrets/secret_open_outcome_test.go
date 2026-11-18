package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestAStoreWriteWhoseAnswerIsLostSaysTheOutcomeIsUnknown: `secret set`, `secret delete` and
// `secret restore` each make one store write, and a write whose error is no definite refusal (an
// AWS service fault, a GCP UNAVAILABLE) may follow a write the store carried out. Each says the
// store's answer leaves open whether the write landed, names the `secret show` that reads what the
// store now holds, asks the broker to reread, and prints no success line. Every row's store applies
// the write and then answers the error, so the store holds the write's result.
func TestAStoreWriteWhoseAnswerIsLostSaysTheOutcomeIsUnknown(t *testing.T) {
	type stored struct {
		value   string
		deleted bool
	}
	for _, c := range []struct {
		form, lose, success string
		deleted             bool
		stdin               string
		want                stored
	}{
		{form: "set", lose: "put", success: "set a new value of API_KEY", stdin: "v2", want: stored{value: "v2"}},
		{form: "delete", lose: "delete", success: "deleted API_KEY in", want: stored{value: "v1", deleted: true}},
		{form: "restore", lose: "restore", success: "restored API_KEY in", deleted: true, want: stored{value: "v1"}},
	} {
		t.Run("aws: "+c.form, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1"))
			if c.deleted {
				if _, err := local.DeleteSecret(context.Background(), &secretsmanager.DeleteSecretInput{SecretId: aws.String(policytest.ID("API_KEY"))}); err != nil {
					t.Fatal(err)
				}
			}
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, &raceOnTag{secretsAPI: local, loseAnswer: c.lose}, testAccount, adaSignIn)
			if c.stdin != "" {
				useStdin(t, c.stdin)
			}
			stdout, stderr, code := runSecret(c.form, "API_KEY")
			assertOpenOutcome(t, c.form, stdout, stderr, code, c.success)
			out, err := local.DescribeSecret(context.Background(), &secretsmanager.DescribeSecretInput{SecretId: aws.String(policytest.ID("API_KEY"))})
			if err != nil {
				t.Fatal(err)
			}
			got := stored{deleted: out.DeletedDate != nil, value: c.want.value}
			if !got.deleted {
				// A deleted secret refuses value reads; its value is not this row's question.
				got.value = valueOf(t, local, "API_KEY")
			}
			if got != c.want {
				t.Fatalf("store after %s = %+v; want %+v: the fake applied the write", c.form, got, c.want)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
				t.Fatalf("rereads after %s = %v; want one of API_KEY, since the write may have changed the secret", c.form, rereads)
			}
		})
		t.Run("gcp: "+c.form, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			settings := gcpTestSettings()
			broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
			id := testGCPPrefix + "api-key"
			secret := &secretmanagerpb.Secret{Name: id, Annotations: policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, "v1").Tags, Etag: "1"}
			if c.deleted {
				secret.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(time.Now().Add(24 * time.Hour))}
			}
			fake.secrets[id] = &fakeSecret{
				secret:      secret,
				versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
				payloads:    [][]byte{[]byte("v1")},
				etagCounter: 1,
			}
			method := map[string]string{"put": "AddSecretVersion", "delete": "UpdateSecret", "restore": "UpdateSecret"}[c.lose]
			fake.answerAfter = map[string]codes.Code{method: codes.Unavailable}
			if c.stdin != "" {
				useStdin(t, c.stdin)
			}
			stdout, stderr, code := runSecret(c.form, "API_KEY")
			assertOpenOutcome(t, c.form, stdout, stderr, code, c.success)
			fake.mu.Lock()
			entry := fake.secrets[id]
			got := stored{value: string(entry.payloads[len(entry.payloads)-1]), deleted: entry.secret.GetExpireTime() != nil}
			fake.mu.Unlock()
			if got != c.want {
				t.Fatalf("store after %s = %+v; want %+v: the fake applied the write", c.form, got, c.want)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "API_KEY" {
				t.Fatalf("rereads after %s = %v; want one of API_KEY, since the write may have changed the secret", c.form, rereads)
			}
		})
	}
}

// assertOpenOutcome holds a form's answer to a write whose outcome is open: exit 1, the store's
// answer said to leave the outcome open, the `secret show` that reads the store, the broker's
// answer printed, and no success line.
func assertOpenOutcome(t *testing.T, form, stdout, stderr string, code int, success string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("%s whose answer was lost: exit %d stderr %q; want 1", form, code, stderr)
	}
	for _, said := range []string{"leaves open whether this write landed", "Read what it holds now with `agent-secrets secret show API_KEY`"} {
		if !strings.Contains(stderr, said) {
			t.Fatalf("%s whose answer was lost: stderr %q does not say %q", form, stderr, said)
		}
	}
	if strings.Contains(stdout, success) {
		t.Fatalf("%s whose answer was lost: stdout %q says %q, though the outcome is unknown", form, stdout, success)
	}
	if !strings.Contains(stdout, "broker: ") {
		t.Fatalf("%s whose answer was lost: stdout %q prints no broker answer to the reread", form, stdout)
	}
}
