// packages/envoy/cmd/cipher/secret_e2e_test.go
//
// The secret forms against the real broker (ciphertest.NewRig: real handlers, real Postgres), its
// secrets.Local standing in for Secrets Manager as the CLI's AWS. They skip without
// BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/testwait"
)

// secretsLocalWithoutValue is an agent-tier secret owner holds with no value, which the broker
// refuses as no-current-value.
func secretsLocalWithoutValue(name, owner string) secrets.LocalSecret {
	return policytest.Secret(name, owner, policy.TierAgent, "")
}

// useRigSession makes a session the rig holds this process's broker identity for the rest of t:
// its key and enrollment in a key directory (AGENT_SECRETS_KEY_DIR), as a launcher leaves them, so
// the secret forms sign their rereads with it and the rig answers them in full.
func useRigSession(t *testing.T, rig *ciphertest.Rig) {
	t.Helper()
	enrollmentID, key := rig.NewSession(t, rig.Operator)
	t.Setenv("AGENT_SECRETS_KEY_DIR", writeKeyDir(t, key, enrollmentID))
}

// TestSecretCreateIsServedByTheBrokerAtOnce drives a create end to end: the CLI writes to (fake)
// AWS and the broker serves the secret on its very next read, no reload.
// Serial: it shares process-wide state - AGENT_SECRETS_URL, which brokerBaseURL (identity.go)
// reads the broker's address from, and the package-level seams the secret forms are reached
// through (awsClients, secretStdin); t.Setenv panics under t.Parallel.
func TestSecretCreateIsServedByTheBrokerAtOnce(t *testing.T) {
	rig := ciphertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	useRigSession(t, rig)
	restore := awsClients
	awsClients = func(context.Context, string) (secretsAPI, stsAPI, []aws.CredentialSource, error) {
		return rig.Secrets, fakeSTS{account: "111122223333", arn: "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/" + rig.Operator}, nil, nil
	}
	t.Cleanup(func() { awsClients = restore })
	stdin := secretStdin
	secretStdin = strings.NewReader("fresh-v1")
	t.Cleanup(func() { secretStdin = stdin })
	var out, errOut bytes.Buffer
	if code := cmdSecret([]string{"create", "FRESH_KEY", "--owner", "me", "--tier", "agent"}, &out, &errOut); code != 0 {
		t.Fatalf("create: exit %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "broker: serving FRESH_KEY") {
		t.Fatalf("stdout %q must confirm the broker serves it", out.String())
	}
}

// TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce: the broker stops serving a secret the
// moment the CLI deletes it, and serves it again the moment the CLI restores it, each on the
// CLI's own reread; a retag the broker would refuse is reported, and exits 1.
// Serial: it shares process-wide state - AGENT_SECRETS_URL, which brokerBaseURL (identity.go)
// reads the broker's address from, and awsClients, the seam the secret forms reach the store
// through (useAWS); t.Setenv panics under t.Parallel.
func TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce(t *testing.T) {
	rig := ciphertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	useRigSession(t, rig)
	useAWS(t, rig.Secrets, testAccount, "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/"+rig.Operator)
	for _, step := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"delete", "TEST_AUTO_SECRET"}, 0, "broker: TEST_AUTO_SECRET is deleted in "},
		{[]string{"restore", "TEST_AUTO_SECRET"}, 0, "broker: serving TEST_AUTO_SECRET"},
		{[]string{"retag", "TEST_AUTO_SECRET", "--tier", "human"}, 0, "broker: serving TEST_AUTO_SECRET"},
	} {
		stdout, stderr, code := runSecret(step.args...)
		if code != step.code || !strings.Contains(stdout, step.want) {
			t.Fatalf("secret %s: exit %d, stdout %q, stderr %q; want %d with %q", strings.Join(step.args, " "), code, stdout, stderr, step.code, step.want)
		}
	}
	// A secret Secrets Manager holds with no value is one the broker refuses: the CLI's write
	// stands and the broker's refusal is reported, exit 1.
	rig.Secrets.Put(secretsLocalWithoutValue("NO_VALUE_KEY", rig.Operator))
	stdout, stderr, code := runSecret("retag", "NO_VALUE_KEY", "--tier", "human")
	if code != 1 || !strings.Contains(stdout, "broker: refusing NO_VALUE_KEY from ") || !strings.Contains(stdout, "(reason=no-current-value)") {
		t.Fatalf("retag of a secret with no value: exit %d, stdout %q, stderr %q; want 1 with the broker's refusal", code, stdout, stderr)
	}
}

// TestASecretWriteWithNoBrokerIdentityIsStillServedAtOnce: a create run where this process has no
// broker identity at all sends its reread with no proof, which the real broker answers 202 at
// once and says nothing more of. The form exits 0, the write standing, and the broker still
// rereads the name after that answer and serves the new secret, with no reload. The line it
// prints about the unchecked view is TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned's.
func TestASecretWriteWithNoBrokerIdentityIsStillServedAtOnce(t *testing.T) {
	rig := ciphertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	withNoBrokerIdentity(t)
	useAWS(t, rig.Secrets, testAccount, "arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_User_abc/"+rig.Operator)
	useStdin(t, "fresh-v1")
	stdout, stderr, code := runSecret("create", "FRESH_KEY", "--owner", "me", "--tier", "agent")
	if code != 0 || !strings.Contains(stdout, "broker: rereading FRESH_KEY from ") || strings.Contains(stdout, "broker: serving") {
		t.Fatalf("create with no broker identity: exit %d, stdout %q, stderr %q; want 0 and the reread sent with no proof", code, stdout, stderr)
	}
	if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool {
		_, served := rig.Policy.Get().Secrets["FRESH_KEY"]
		return served
	}) {
		t.Fatal("the broker does not serve FRESH_KEY within 5s of the create's reread; want the reread a call with no proof asked for run after its answer")
	}
}
