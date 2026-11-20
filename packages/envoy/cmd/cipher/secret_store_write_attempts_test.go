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

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// sdkWrites is a secrets.Local whose PutSecretValue, DeleteSecret and RestoreSecret go through a
// real Secrets Manager client, so the SDK's own retries and the backend's attempt record
// (awsAttempts) run for set, delete and restore as they do in production; every other call goes
// to the Local.
type sdkWrites struct {
	*secrets.Local
	sm *secretsmanager.Client
}

func (w sdkWrites) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	return w.sm.PutSecretValue(ctx, in, o...)
}

func (w sdkWrites) DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	return w.sm.DeleteSecret(ctx, in, o...)
}

func (w sdkWrites) RestoreSecret(ctx context.Context, in *secretsmanager.RestoreSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.RestoreSecretOutput, error) {
	return w.sm.RestoreSecret(ctx, in, o...)
}

const (
	openOutcome   = "leaves open whether this write landed"
	storeRefused  = "the store refused"
	writeAPIKeyV1 = "v1"
)

// TestAnAWSStoreWriteIsJudgedByItsAttempts drives set, delete and restore through a real SDK
// client, so the backend's attempt record runs. A call whose every attempt was answered with a
// code awsDefiniteRefusals lists is a refusal: one AccessDeniedException, where the form names the
// refused sign-in, and a LimitExceededException the SDK retried as a throttle on every attempt
// (a set past the version quota); either asks for no reread and leaves the store unchanged. A
// first attempt that applied and lost its answer before a denied retry, and a service fault on
// every attempt, are open outcomes: the form says so, claims nothing, asks for one reread, and
// the store holds what the attempts did.
func TestAnAWSStoreWriteIsJudgedByItsAttempts(t *testing.T) {
	for _, c := range []struct {
		name, form, op, stdin string
		deleted               bool
		steps                 []string
		attempts, rereads     int
		open                  bool
		refusal               string
		value                 string
		wantDeleted           bool
	}{
		{name: "set: one AccessDenied", form: "set", op: "PutSecretValue", stdin: "v2", steps: []string{"deny"}, attempts: 1, value: writeAPIKeyV1},
		{name: "set: LimitExceededException every attempt (a throttle the SDK retried)", form: "set", op: "PutSecretValue", stdin: "v2", steps: []string{"limit"}, attempts: 3, refusal: "LimitExceededException", value: writeAPIKeyV1},
		{name: "set: applied, answer lost, retry denied", form: "set", op: "PutSecretValue", stdin: "v2", steps: []string{"apply-drop", "deny"}, attempts: 2, rereads: 1, open: true, value: "v2"},
		{name: "set: InternalServiceError every attempt", form: "set", op: "PutSecretValue", stdin: "v2", steps: []string{"ise"}, attempts: 3, rereads: 1, open: true, value: writeAPIKeyV1},
		{name: "delete: one AccessDenied", form: "delete", op: "DeleteSecret", steps: []string{"deny"}, attempts: 1, value: writeAPIKeyV1},
		{name: "delete: applied, answer lost, retry denied", form: "delete", op: "DeleteSecret", steps: []string{"apply-drop", "deny"}, attempts: 2, rereads: 1, open: true, wantDeleted: true},
		{name: "restore: one AccessDenied", form: "restore", op: "RestoreSecret", deleted: true, steps: []string{"deny"}, attempts: 1, wantDeleted: true},
		{name: "restore: applied, answer lost, retry denied", form: "restore", op: "RestoreSecret", deleted: true, steps: []string{"apply-drop", "deny"}, attempts: 2, rereads: 1, open: true, value: writeAPIKeyV1},
	} {
		t.Run(c.name, func(t *testing.T) {
			local := secrets.NewLocal(policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, writeAPIKeyV1))
			if c.deleted {
				if _, err := local.DeleteSecret(context.Background(), &secretsmanager.DeleteSecretInput{SecretId: aws.String(policytest.ID("API_KEY"))}); err != nil {
					t.Fatal(err)
				}
			}
			sm, st := startSDKServer(t, local, sdkScript{c.op: c.steps})
			broker := startSecretBroker(t, servedBy(local))
			useAWS(t, sdkWrites{Local: local, sm: sm}, testAccount, adaSignIn)
			if c.stdin != "" {
				useStdin(t, c.stdin)
			}
			stdout, stderr, code := runSecret(c.form, "API_KEY")
			if code != 1 {
				t.Fatalf("exit %d; want 1: stderr %q", code, stderr)
			}
			switch {
			case c.open:
				if !strings.Contains(stderr, openOutcome) || strings.Contains(stderr, storeRefused) {
					t.Errorf("stderr %q; want the open outcome, and no refusal", stderr)
				}
				assertNoClaim(t, c.form, stdout, stderr)
			case c.refusal != "":
				if !strings.Contains(stderr, c.refusal) || strings.Contains(stderr, openOutcome) {
					t.Errorf("stderr %q; want the store's refusal %s, and no open outcome", stderr, c.refusal)
				}
			case !strings.Contains(stderr, "AccessDeniedException") || !strings.Contains(stderr, storeRefused) || strings.Contains(stderr, openOutcome):
				t.Errorf("stderr %q; want the refusal, naming the refused sign-in, and no open outcome", stderr)
			}
			if got := st.count(c.op); got != c.attempts {
				t.Errorf("%s attempts %d; want %d", c.op, got, c.attempts)
			}
			if rereads, _ := broker.recorded(); len(rereads) != c.rereads {
				t.Errorf("rereads %v; want %d", rereads, c.rereads)
			}
			deleted := describe(t, local, "API_KEY").DeletedDate != nil
			if deleted != c.wantDeleted || (!deleted && valueOf(t, local, "API_KEY") != c.value) {
				t.Errorf("store: deleted %v; want deleted %v, value %q", deleted, c.wantDeleted, c.value)
			}
		})
	}
}

// TestAGCPStoreWriteIsJudgedByItsCode: a GCP write refused PERMISSION_DENIED or NOT_FOUND is a
// definite refusal, with no reread; one answered UNAVAILABLE or ABORTED before the fake applied it
// is an open outcome all the same, since the form cannot tell it from one answered after, and it
// claims nothing.
func TestAGCPStoreWriteIsJudgedByItsCode(t *testing.T) {
	for _, c := range []struct {
		name, form, stdin, method string
		deleted                   bool
		code                      codes.Code
		want                      []string
		open                      bool
	}{
		{"set: PermissionDenied", "set", "v2", "AddSecretVersion", false, codes.PermissionDenied, []string{"PermissionDenied", storeRefused}, false},
		{"delete: PermissionDenied", "delete", "", "UpdateSecret", false, codes.PermissionDenied, []string{"PermissionDenied", storeRefused}, false},
		{"restore: PermissionDenied", "restore", "", "UpdateSecret", true, codes.PermissionDenied, []string{"PermissionDenied", storeRefused}, false},
		{"set: NotFound", "set", "v2", "AddSecretVersion", false, codes.NotFound, []string{"NotFound"}, false},
		{"set: Unavailable", "set", "v2", "AddSecretVersion", false, codes.Unavailable, []string{openOutcome}, true},
		{"delete: Unavailable", "delete", "", "UpdateSecret", false, codes.Unavailable, []string{openOutcome}, true},
		{"restore: Aborted", "restore", "", "UpdateSecret", true, codes.Aborted, []string{openOutcome}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			startFakeSecretManager(t, fake)
			useGCPIdentity(t, "authorized_user", "ada@example.com")
			settings := gcpTestSettings()
			broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
			id := testGCPPrefix + "api-key"
			secret := &secretmanagerpb.Secret{Name: id, Annotations: policytest.Secret("API_KEY", "ada@example.com", policy.TierAgent, writeAPIKeyV1).Tags, Etag: "1"}
			if c.deleted {
				secret.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(time.Now().Add(24 * time.Hour))}
			}
			fake.secrets[id] = &fakeSecret{
				secret:      secret,
				versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
				payloads:    [][]byte{[]byte(writeAPIKeyV1)},
				etagCounter: 1,
			}
			fake.refuseMethods = map[string]codes.Code{c.method: c.code}
			if c.stdin != "" {
				useStdin(t, c.stdin)
			}
			stdout, stderr, code := runSecret(c.form, "API_KEY")
			if code != 1 {
				t.Fatalf("exit %d; want 1: stderr %q", code, stderr)
			}
			for _, said := range c.want {
				if !strings.Contains(stderr, said) {
					t.Errorf("stderr %q lacks %q", stderr, said)
				}
			}
			rereads, _ := broker.recorded()
			if c.open {
				if strings.Contains(stderr, storeRefused) || len(rereads) != 1 {
					t.Errorf("stderr %q, rereads %v; want no refusal and one reread", stderr, rereads)
				}
				assertNoClaim(t, c.form, stdout, stderr)
			} else if strings.Contains(stderr, openOutcome) || len(rereads) != 0 {
				t.Errorf("stderr %q, rereads %v; want a refusal with no open outcome and no reread", stderr, rereads)
			}
		})
	}
}
