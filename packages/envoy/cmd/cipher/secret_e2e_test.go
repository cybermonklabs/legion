// packages/envoy/cmd/cipher/secret_e2e_test.go
//
// The secret forms against the real broker (ciphertest.NewRig: real handlers, real Postgres) on
// two stores: AWS, over the awstest.Local standing in for Secrets Manager as the CLI's AWS, and
// the memory store, a file the rig's broker follows and the CLI writes. They skip without
// BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
	"github.com/sjawhar/envoy/internal/cipher/settingsfields"
)

// rigOperator is the person ciphertest's rig enrolls sessions for and whose secrets it holds.
const rigOperator = "ada@example.com"

// awsRig is a ciphertest rig whose broker reads the AWS store over an awstest.Local, its settings
// naming that store on awstest.KeyARN, and the Local, which holds fixtures and is the store the
// CLI's AWS writes here once a test points the CLI's AWS clients at it.
func awsRig(t *testing.T, options ...ciphertest.Option) (*ciphertest.Rig, *awstest.Local) {
	t.Helper()
	local := awstest.NewLocal()
	for _, s := range e2eFixtures() {
		local.Put(awsSecret(s))
	}
	fields := settingsfields.Of(awsstore.SettingsFields{KMSKeyARN: awstest.KeyARN, AWSAccountID: testAccount, AWSRegion: "us-east-1"})
	store := awsstore.Store{Client: local, KMS: local, KeyARN: awstest.KeyARN}
	return ciphertest.NewRig(t, append(options, ciphertest.WithSecretStore(awsstore.Name, store, fields))...), local
}

// e2eFixtures are the rig's two fixtures, which every e2e store holds.
func e2eFixtures() []memory.Secret {
	return []memory.Secret{
		policytest.Secret("TEST_SECRET", rigOperator, policy.TierHuman, "test-secret-v1"),
		policytest.Secret("TEST_AUTO_SECRET", rigOperator, policy.TierAgent, "test-auto-secret-v1"),
	}
}

// e2eRig is a ciphertest rig whose broker reads store ("aws" or "memory"), holding the rig's two
// fixtures, with the CLI's writer of that store pointed at the same secrets, and put, which holds
// one more secret there as a writer other than the CLI.
func e2eRig(t *testing.T, store string, options ...ciphertest.Option) (rig *ciphertest.Rig, put func(memory.Secret)) {
	t.Helper()
	switch store {
	case "aws":
		rig, local := awsRig(t, options...)
		awstest.UseClients(t, func(context.Context, string) (awsstore.WriterAPI, awsstore.STSAPI, []aws.CredentialSource, error) {
			return local, awstest.STS{Account: testAccount, ARN: "arn:aws:sts::" + testAccount + ":assumed-role/AWSReservedSSO_User_abc/" + rigOperator}, nil, nil
		})
		return rig, func(s memory.Secret) { local.Put(awsSecret(s)) }
	case "memory":
		path := filepath.Join(t.TempDir(), "secrets.json")
		writeMemoryFile(t, path, e2eFixtures())
		opened, err := memory.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		fields := settingsfields.Of(memory.SettingsFields{File: path})
		rig = ciphertest.NewRig(t, append(options, ciphertest.WithSecretStore(memory.Name, opened, fields))...)
		return rig, func(s memory.Secret) { writeMemoryFile(t, path, append(readMemoryFile(t, path), s)) }
	}
	t.Fatalf("no e2e rig for the store %s", store)
	return nil, nil
}

// onE2EStores runs test on the stores the e2e tests cover, aws and memory, by name.
func onE2EStores(t *testing.T, test func(t *testing.T, store string)) {
	for _, store := range []string{"aws", "memory"} {
		t.Run(store, func(t *testing.T) { test(t, store) })
	}
}

// useRigSession makes a session the rig holds this process's broker identity for the rest of t:
// its key and enrollment in a key directory (AGENT_SECRETS_KEY_DIR), as a launcher leaves them, so
// the secret forms sign their rereads with it and the rig answers them in full.
func useRigSession(t *testing.T, rig *ciphertest.Rig) {
	t.Helper()
	enrollmentID, key := rig.NewSession(t, rig.Operator)
	t.Setenv("AGENT_SECRETS_KEY_DIR", writeKeyDir(t, key, enrollmentID))
}

// TestSecretCreateIsServedByTheBrokerAtOnce drives a create end to end: the CLI writes to the
// store and the broker serves the secret on its very next read, no reload.
func TestSecretCreateIsServedByTheBrokerAtOnce(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_e2e_test.go:42); it signals the test process or changes its signal handling (ciphertest.NewRig, api.Register, api.routes, (*server).listLauncherCredentials, (*server).machineLoginsOwnOnly, projectaccess.Restricted, (*Client).MemberKind, (*Client).askKind, (*Client).readKind, (*Client).get, (Reader).Get, (*GoogleAccount).Read, (*GoogleAccount).read, (*GoogleAccount).mint, (*GoogleAccount).generateIDToken, (*GoogleAccount).accessToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (awsClients set at secret_e2e_test.go:45).
	onE2EStores(t, func(t *testing.T, store string) {
		rig, _ := e2eRig(t, store)
		t.Setenv("AGENT_SECRETS_URL", rig.URL)
		useRigSession(t, rig)
		stdin := secretStdin
		secretStdin = strings.NewReader("fresh-v1")
		t.Cleanup(func() { secretStdin = stdin })
		var out, errOut bytes.Buffer
		if code := cmdSecret([]string{"create", "FRESH_KEY", "--owner", rigOperator, "--tier", "agent"}, &out, &errOut); code != 0 {
			t.Fatalf("create: exit %d, stderr %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "broker: serving FRESH_KEY") {
			t.Fatalf("stdout %q must confirm the broker serves it", out.String())
		}
	})
}

// TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce: the broker stops serving a secret the
// moment the CLI deletes it, and serves it again the moment the CLI restores it, each on the
// CLI's own reread; a retag the broker would refuse is reported, and exits 1.
func TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_e2e_test.go:67); it signals the test process or changes its signal handling (ciphertest.NewRig, api.Register, api.routes, (*server).listLauncherCredentials, (*server).machineLoginsOwnOnly, projectaccess.Restricted, (*Client).MemberKind, (*Client).askKind, (*Client).readKind, (*Client).get, (Reader).Get, (*GoogleAccount).Read, (*GoogleAccount).read, (*GoogleAccount).mint, (*GoogleAccount).generateIDToken, (*GoogleAccount).accessToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	onE2EStores(t, func(t *testing.T, store string) {
		rig, put := e2eRig(t, store)
		t.Setenv("AGENT_SECRETS_URL", rig.URL)
		useRigSession(t, rig)
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
		// A secret the store holds with no value is one the broker refuses: the CLI's write stands
		// and the broker's refusal is reported, exit 1.
		put(policytest.Secret("NO_VALUE_KEY", rig.Operator, policy.TierAgent, ""))
		stdout, stderr, code := runSecret("retag", "NO_VALUE_KEY", "--tier", "human")
		if code != 1 || !strings.Contains(stdout, "broker: refusing NO_VALUE_KEY from ") || !strings.Contains(stdout, "(reason=no-current-value)") {
			t.Fatalf("retag of a secret with no value: exit %d, stdout %q, stderr %q; want 1 with the broker's refusal", code, stdout, stderr)
		}
	})
}

// TestASecretWriteWithNoBrokerIdentityIsStillServedAtOnce: a create run where this process has no
// broker identity at all sends its reread with no proof, which the real broker answers 202 at
// once and says nothing more of. The form exits 0, the write standing, and the broker still
// rereads the name after that answer and serves the new secret, with no reload. The line it
// prints about the unchecked view is TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned's.
func TestASecretWriteWithNoBrokerIdentityIsStillServedAtOnce(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_e2e_test.go:101); it signals the test process or changes its signal handling (ciphertest.NewRig, api.Register, api.routes, (*server).listLauncherCredentials, (*server).machineLoginsOwnOnly, projectaccess.Restricted, (*Client).MemberKind, (*Client).askKind, (*Client).readKind, (*Client).get, (Reader).Get, (*GoogleAccount).Read, (*GoogleAccount).read, (*GoogleAccount).mint, (*GoogleAccount).generateIDToken, (*GoogleAccount).accessToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	onE2EStores(t, func(t *testing.T, store string) {
		rig, _ := e2eRig(t, store)
		t.Setenv("AGENT_SECRETS_URL", rig.URL)
		withNoBrokerIdentity(t)
		useStdin(t, "fresh-v1")
		stdout, stderr, code := runSecret("create", "FRESH_KEY", "--owner", rigOperator, "--tier", "agent")
		if code != 0 || !strings.Contains(stdout, "broker: rereading FRESH_KEY from ") || strings.Contains(stdout, "broker: serving") {
			t.Fatalf("create with no broker identity: exit %d, stdout %q, stderr %q; want 0 and the reread sent with no proof", code, stdout, stderr)
		}
		if !heldBeforeGiveUp(t, func() bool {
			_, served := rig.Policy.Get().Secrets["FRESH_KEY"]
			return served
		}) {
			t.Fatalf("the broker did not serve FRESH_KEY after the create's reread, %s before the test binary's deadline; want the reread a call with no proof asked for run after its answer", waitMargin)
		}
	})
}
