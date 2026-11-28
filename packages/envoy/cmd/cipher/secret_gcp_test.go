// packages/envoy/cmd/cipher/secret_gcp_test.go
//
// The GCP secret forms run in-process here, against a fake Secret Manager gRPC server
// (gcptest.FakeSecretManager) standing in for the real API, with the writer's credential hooks
// swapped (gcp.SwapHooks, through gcptest's helpers) so no form makes a network call for its
// identity, a stub gcloud alone on PATH (gcptest.UseGcloud) where a form can reach for a gcloud
// login, and the shared fake broker (secretBroker, secret_test.go) answering GET /v1/settings with
// the GCP store's shape and the reread.
// secret_gcp_order_test.go runs the order of those sources through the real ones instead.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"golang.org/x/oauth2/google"
	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/testexec"
)

const (
	// testGCPProject is a project NUMBER, the form the broker's gcp_project setting and the
	// secrets' own resource-name prefix take.
	testGCPProject = "100000000001"
	testKMSKeyName = "projects/acme-deployment/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
	testGCPPrefix  = "projects/100000000001/secrets/agent-secrets-"
)

// gcpTestSettings are a GCP broker's settings, as the clean address https://broker.example
// answered them (Settings.answeredBy), so a refusal a test calls directly names their fields.
func gcpTestSettings() Settings {
	return Settings{
		Store:         gcp.Name,
		SecretsPrefix: testGCPPrefix,
		SecretTypeTag: true,
		answeredBy:    cipheraddr.New("https://broker.example"),
		Fields:        map[string]string{"kms_key_name": testKMSKeyName, "gcp_project": testGCPProject},
	}
}

// gcpServedBy answers a reread as the broker would from what f holds: absent for a secret it does
// not hold, or one scheduled for deletion (expire_time set, whether or not it has passed - a
// broker reread treats a scheduled deletion as absent the moment it is scheduled, as AWS's
// DeletedDate does), pending-value for one with no version that carries the create label (a
// create waiting for its value: the fake makes every secret now, so it is always young enough),
// no-current-value for any other with no version, and otherwise what its annotations answer
// (servedAs): refused for a type without the GitHub owners it needs, served as its type if not.
func gcpServedBy(f *gcptest.FakeSecretManager, settings Settings) func(name string) Reread {
	return func(name string) Reread {
		slug, err := policy.NameToSlug(name)
		if err != nil {
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		}
		f.Mu.Lock()
		defer f.Mu.Unlock()
		entry, ok := f.Secrets[settings.SecretsPrefix+slug]
		switch {
		case !ok:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case entry.Secret.GetExpireTime() != nil:
			return Reread{Name: name, Reason: policy.ReasonAbsent}
		case len(entry.Versions) == 0 && entry.Secret.GetLabels()[secretstore.CreateLabel] != "":
			return Reread{Name: name, Reason: policy.ReasonPendingValue}
		case len(entry.Versions) == 0:
			return Reread{Name: name, Reason: policy.ReasonNoCurrentValue}
		}
		return servedAs(name, entry.Secret.GetAnnotations())
	}
}

// machineEmail is the service account a machine's own Application Default Credentials and its
// own gcloud login name.
const machineEmail = "robot@acme-deployment.iam.gserviceaccount.com"

// machineADCKinds are the Application Default Credentials that are a machine's own identity, as
// useGCPIdentity takes them: a service account key file, a workload identity federation credential
// (a dev box's), and the metadata server's identity, which carries no JSON.
var machineADCKinds = []string{"service_account", "external_account", ""}

// Federation logins as `gcloud auth list` names them after `gcloud auth login --cred-file`: by the
// principal identifier token introspection answers, a workforce one ending in the person's email.
const (
	workloadPrincipal  = "principal://iam.googleapis.com/projects/100000000001/locations/global/workloadIdentityPools/acme-pool/subject/robot"
	workforcePrincipal = "principal://iam.googleapis.com/locations/global/workforcePools/acme-staff/subject/ada@example.com"
)

// refuseValueReads makes reading a secret's value fail t for the rest of it (failingStdin): no
// value may be asked for before the sign-in is settled.
func refuseValueReads(t *testing.T) {
	t.Helper()
	restore := secretStdin
	secretStdin = failingStdin{t}
	t.Cleanup(func() { secretStdin = restore })
}

func alwaysServed(name string) Reread { return Reread{Name: name, Served: true} }

func TestGCPSecretCreateShowSetRetagDeleteRestoreLifecycle(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")
	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	// Each line names the store it wrote to, so an answer of this CLI's is never read as
	// another secrets CLI's (source.go).
	if !strings.Contains(stdout, "created SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=agent)") {
		t.Fatalf("create stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner: ada@example.com") || !strings.Contains(stdout, "tier: agent") {
		t.Fatalf("show stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("show printed the value: %q", stdout)
	}

	useStdin(t, "shh2")
	_, stderr, code = runSecret("set", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("set: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "retagged SCRATCH_TEST in GCP Secret Manager project ") || !strings.Contains(stdout, "(owner=ada@example.com, tier=human)") {
		t.Fatalf("retag stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("delete", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted SCRATCH_TEST in GCP Secret Manager project ") {
		t.Fatalf("delete stdout: %q", stdout)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after delete: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after delete did not print deleted: %q", stdout)
	}
	if strings.Contains(stdout, "earliest_purge") {
		t.Fatalf("GCP show printed an AWS-only earliest_purge line: %q", stdout)
	}

	_, stderr, code = runSecret("restore", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("restore: exit %d, stderr %q", code, stderr)
	}

	stdout, stderr, code = runSecret("show", "SCRATCH_TEST")
	if code != 0 {
		t.Fatalf("show after restore: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stdout, "deleted:") {
		t.Fatalf("show after restore still shows deleted: %q", stdout)
	}
}

// fakeOwnerOf is the owner annotation of the secret f holds under slug, "" when it holds none.
func fakeOwnerOf(f *gcptest.FakeSecretManager, slug string) string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	entry, ok := f.Secrets[testGCPPrefix+slug]
	if !ok {
		return ""
	}
	return entry.Secret.GetAnnotations()[policy.TagOwner]
}

// requireNothingWritten fails t unless Secret Manager holds no secret and the broker was asked
// for no reread: what a refused write leaves.
func requireNothingWritten(t *testing.T, fake *gcptest.FakeSecretManager, broker *secretBroker) {
	t.Helper()
	if n := fake.Written(); n != 0 {
		t.Fatalf("Secret Manager holds %d secrets after a refused write", n)
	}
	if rereads, _ := broker.recorded(); len(rereads) != 0 {
		t.Fatalf("rereads = %v after a refused write, want none", rereads)
	}
}

// requireRunUnder fails t unless every Secret Manager call so far, and at least one, carried the
// token the stub gcloud lends account, and gcloud lent tokens for account alone.
func requireRunUnder(t *testing.T, fake *gcptest.FakeSecretManager, gcloud gcptest.GcloudStub, account string) {
	t.Helper()
	if got, calls := fake.CalledUnder(); got != "Bearer "+gcptest.StubTokenPrefix+account || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s's gcloud token", got, calls, account)
	}
	lent := gcloud.Lent(t)
	if len(lent) == 0 || slices.ContainsFunc(lent, func(a string) bool { return a != account }) {
		t.Fatalf("gcloud lent tokens for %v, want %s's alone", lent, account)
	}
}

// holdScratchTest puts SCRATCH_TEST in f, owned by ada@example.com at tier agent, with one
// enabled version, as an earlier write left it, and answers its resource name.
func holdScratchTest(f *gcptest.FakeSecretManager) string {
	id := testGCPPrefix + "scratch-test"
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Secrets[id] = &gcptest.FakeSecret{
		Secret:      &secretmanagerpb.Secret{Name: id, Annotations: map[string]string{policy.TagOwner: "ada@example.com", policy.TagTier: policy.TierAgent}, Etag: "1"},
		Versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
		EtagCounter: 1,
	}
	return id
}

// TestGCPSecretWriteUnderAMachineIdentityRunsUnderIt: every write runs under a machine's own
// credentials file whatever identity it is - a service account key or a workload identity
// federation credential (a dev box's own) - and never under the gcloud login gcloud also holds:
// create, set, retag, delete and restore each write, every Secret Manager call carrying the
// credentials' own token, with gcloud never run. The metadata server's identity comes after the
// active gcloud account in R1's order, so where gcloud holds a login the same writes run under
// that login. Where the store's IAM refuses the identity (PermissionDenied), the refusal (exit 1)
// names its source and principal, and nothing changes.
func TestGCPSecretWriteUnderAMachineIdentityRunsUnderIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	for _, kind := range machineADCKinds {
		t.Run(cmp.Or(kind, "metadata server"), func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcloud := gcptest.UseGcloud(t, "ada@example.com")
			gcptest.UseIdentity(t, kind, machineEmail)
			bearer := "Bearer fake"
			identity := "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: " + machineEmail
			if kind == "" {
				bearer = "Bearer " + gcptest.StubTokenPrefix + "ada@example.com"
				identity = "the active gcloud account (gcloud configuration default): ada@example.com"
			}
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			for _, step := range []struct {
				stdin string
				args  []string
			}{
				{"first", []string{"create", "SCRATCH_TEST", "--owner", "ada@example.com", "--tier", "agent"}},
				{"second", []string{"set", "SCRATCH_TEST"}},
				{"", []string{"retag", "SCRATCH_TEST", "--tier", "human"}},
				{"", []string{"delete", "SCRATCH_TEST"}},
				{"", []string{"restore", "SCRATCH_TEST"}},
			} {
				useStdin(t, step.stdin)
				if _, stderr, code := runSecret(step.args...); code != 0 {
					t.Fatalf("%v: exit %d, stderr %q; want 0", step.args, code, stderr)
				}
			}
			id := testGCPPrefix + "scratch-test"
			fake.Mu.Lock()
			annotations, expire, versions := maps.Clone(fake.Secrets[id].Secret.GetAnnotations()), fake.Secrets[id].Secret.GetExpireTime(), len(fake.Secrets[id].Versions)
			fake.Mu.Unlock()
			if annotations[policy.TagTier] != policy.TierHuman || expire != nil || versions != 2 {
				t.Fatalf("after the writes: annotations %v, expire_time %v, %d versions; want tier human, none, 2", annotations, expire, versions)
			}
			if got, calls := fake.CalledUnder(); got != bearer || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only %s", got, calls, bearer)
			}
			if commands := gcloud.Commands(t); kind != "" && len(commands) != 0 {
				t.Fatalf("gcloud ran %q, want nothing", commands)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 5 {
				t.Fatalf("rereads = %v, want one after each of the five writes", rereads)
			}

			fake.Mu.Lock()
			fake.Refuse = bearer
			fake.Mu.Unlock()
			_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, "PermissionDenied") || !strings.Contains(stderr, "the store refused "+identity) {
				t.Fatalf("a refused retag: exit %d, stderr %q; want 1 naming the store's refusal of %s", code, stderr, identity)
			}
			fake.Mu.Lock()
			tier := fake.Secrets[id].Secret.GetAnnotations()[policy.TagTier]
			fake.Mu.Unlock()
			if tier != policy.TierHuman {
				t.Fatalf("tier = %q after a refused retag, want human", tier)
			}
		})
	}
}

// TestGCPSecretWritesUnderALentTokenWithoutAnEmailScope: an access token lent in
// CLOUDSDK_AUTH_ACCESS_TOKEN and minted for the cloud-platform scope alone, whose account Google's
// userinfo endpoint refuses to name, writes an existing secret all the same: set, retag, and
// delete then restore each write, every Secret Manager call carrying the lent token, and the
// broker is asked to reread each.
func TestGCPSecretWritesUnderALentTokenWithoutAnEmailScope(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useCloudPlatformToken, gcpUserinfo set at secret_gcp_test.go:569).
	for _, row := range []struct {
		name  string
		stdin string
		forms [][]string
		wrote func(entry *gcptest.FakeSecret) bool
	}{
		{"set", "second", [][]string{{"set", "SCRATCH_TEST"}}, func(e *gcptest.FakeSecret) bool { return len(e.Versions) == 2 }},
		{"retag", "", [][]string{{"retag", "SCRATCH_TEST", "--tier", "human"}}, func(e *gcptest.FakeSecret) bool {
			return e.Secret.GetAnnotations()[policy.TagTier] == policy.TierHuman
		}},
		{"delete and restore", "", [][]string{{"delete", "SCRATCH_TEST"}, {"restore", "SCRATCH_TEST"}}, func(e *gcptest.FakeSecret) bool {
			return e.Secret.GetExpireTime() == nil && e.EtagCounter == 3
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseCloudPlatformToken(t)
			id := holdScratchTest(fake)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, row.stdin)
			for _, args := range row.forms {
				if _, stderr, code := runSecret(args...); code != 0 {
					t.Fatalf("%v: exit %d, stderr %q; want 0", args, code, stderr)
				}
			}
			fake.Mu.Lock()
			wrote := row.wrote(fake.Secrets[id])
			fake.Mu.Unlock()
			if !wrote {
				t.Fatalf("%s left SCRATCH_TEST as it was", row.name)
			}
			if got, calls := fake.CalledUnder(); got != "Bearer "+gcptest.CloudPlatformToken || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
			}
			if rereads, _ := broker.recorded(); len(rereads) != len(row.forms) {
				t.Fatalf("rereads = %v, want one after each write", rereads)
			}
		})
	}
}

// TestGCPSecretOwnerMeUnderALentTokenWithoutAnEmailScopeIsRefused: --owner me is the email of the
// person the sign-in is, which Google's userinfo endpoint does not answer for a token without an
// email scope, so create and retag refuse it (exit 2), naming the lent token and --owner <email>,
// before the value is read and with nothing written.
func TestGCPSecretOwnerMeUnderALentTokenWithoutAnEmailScopeIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.refuseValueReads, secretStdin set at secret_gcp_test.go:751).
	for _, args := range [][]string{{"create", "NEW_TEST", "--owner", "me", "--tier", "agent"}, {"retag", "SCRATCH_TEST", "--owner", "me"}} {
		t.Run(args[0], func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseCloudPlatformToken(t)
			holdScratchTest(fake)
			broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
			refuseValueReads(t)

			_, stderr, code := runSecret(args...)
			if code != exitUsageError || !strings.Contains(stderr, "--owner <email>") || !strings.Contains(stderr, "CLOUDSDK_AUTH_ACCESS_TOKEN") {
				t.Fatalf("exit %d, stderr %q; want %d naming the lent token and --owner <email>", code, stderr, exitUsageError)
			}
			if n := fake.Written(); n != 1 {
				t.Fatalf("Secret Manager holds %d secrets after a refused write, want SCRATCH_TEST alone", n)
			}
			if owner := fakeOwnerOf(fake, "scratch-test"); owner != "ada@example.com" {
				t.Fatalf("SCRATCH_TEST's owner = %q after a refused retag", owner)
			}
			if rereads, _ := broker.recorded(); len(rereads) != 0 {
				t.Fatalf("rereads = %v, want none", rereads)
			}
		})
	}
}

// TestGCPSecretOwnerNamedByEmail: --owner names the owning person by email outright, lowercased,
// on create and on retag, so a sign-in whose person Google cannot name - here a lent token without
// an email scope - creates a secret for its person and gives one to another, every call under the
// lent token.
func TestGCPSecretOwnerNamedByEmail(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useCloudPlatformToken, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useCloudPlatformToken, gcpUserinfo set at secret_gcp_test.go:569).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseCloudPlatformToken(t)
	broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "Someone@Example.com", "--tier", "human")
	if code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if owner := fakeOwnerOf(fake, "scratch-test"); owner != "someone@example.com" || !strings.Contains(stdout, "owner=someone@example.com") {
		t.Fatalf("owner annotation %q, stdout %q; want someone@example.com", owner, stdout)
	}
	if _, stderr, code := runSecret("retag", "SCRATCH_TEST", "--owner", "grace@example.com"); code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if owner := fakeOwnerOf(fake, "scratch-test"); owner != "grace@example.com" {
		t.Fatalf("owner annotation %q after the retag, want grace@example.com", owner)
	}
	if got, calls := fake.CalledUnder(); got != "Bearer "+gcptest.CloudPlatformToken || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
	}
	if rereads, _ := broker.recorded(); !slices.Equal(rereads, []string{"SCRATCH_TEST", "SCRATCH_TEST"}) {
		t.Fatalf("rereads = %v, want one after each write", rereads)
	}
}

// TestGCPSecretALentTokenOutranksEveryOtherCredential: CLOUDSDK_AUTH_ACCESS_TOKEN is the caller's
// own choice, so every form signs in with it, a read and a write alike, beside any other
// credential - Application Default Credentials GOOGLE_APPLICATION_CREDENTIALS names, a person's
// own Application Default Credentials, or, with none, a gcloud login - with gcloud never run.
func TestGCPSecretALentTokenOutranksEveryOtherCredential(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	for _, row := range []struct {
		name string
		use  func(t *testing.T)
	}{
		{"over credentials GOOGLE_APPLICATION_CREDENTIALS names", func(t *testing.T) {
			gcptest.UseIdentity(t, "service_account", machineEmail)
		}},
		{"over a person's own Application Default Credentials", func(t *testing.T) { gcptest.UseIdentity(t, "authorized_user", "grace@example.com") }},
		{"over a gcloud login", gcptest.UseNoADC},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcloud := gcptest.UseGcloud(t, "grace@example.com")
			row.use(t)
			gcptest.UseLentAccessToken(t, "lent-ada-token", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
				t.Fatalf("create: exit %d, stderr %q", code, stderr)
			}
			if _, stderr, code := runSecret("list"); code != 0 {
				t.Fatalf("list: exit %d, stderr %q", code, stderr)
			}
			if owner := fakeOwnerOf(fake, "scratch-test"); owner != "ada@example.com" {
				t.Fatalf("owner annotation %q, want the lent token's ada@example.com", owner)
			}
			if got, calls := fake.CalledUnder(); got != "Bearer lent-ada-token" || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the lent token", got, calls)
			}
			if commands := gcloud.Commands(t); len(commands) != 0 {
				t.Fatalf("gcloud ran %q, want nothing", commands)
			}
		})
	}
}

// TestGCPSecretGcloudLoginIsTheOneGcloudUses: with no Application Default Credentials, every form
// runs under the login gcloud itself would use, whatever kind of account it is - a service
// account's, a workload or workforce identity federation login's, or a person's: the one
// CLOUDSDK_CORE_ACCOUNT names, else the active one. Where CLOUDSDK_CORE_ACCOUNT names a login
// gcloud does not hold, a form is refused (exit 1), naming that account and each login gcloud
// holds, with no token lent and no Secret Manager call - never run as another login, though gcloud
// holds only one.
func TestGCPSecretGcloudLoginIsTheOneGcloudUses(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1137); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	several := []string{machineEmail, "ada@example.com", "grace@example.com"}
	for _, row := range []struct {
		name, named, want string
		accounts          []string
	}{
		{"a service account's login alone", "", machineEmail, []string{machineEmail}},
		{"a workload identity federation login alone", "", workloadPrincipal, []string{workloadPrincipal}},
		{"a workforce identity federation login alone", "", workforcePrincipal, []string{workforcePrincipal}},
		{"the active one of several", "", machineEmail, several},
		{"the one CLOUDSDK_CORE_ACCOUNT names", "grace@example.com", "grace@example.com", several},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseNoADC(t)
			gcloud := gcptest.UseGcloud(t, row.accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "shh")

			if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "human"); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			requireRunUnder(t, fake, gcloud, row.want)
		})
	}
	for _, accounts := range [][]string{{machineEmail}, several} {
		t.Run(fmt.Sprintf("a named login gcloud does not hold, beside %d", len(accounts)), func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseNoADC(t)
			gcloud := gcptest.UseGcloud(t, accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "linus@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

			_, stderr, code := runSecret("list")
			if code != 1 || !strings.Contains(stderr, "CLOUDSDK_CORE_ACCOUNT names an account gcloud holds no login for (not printed;") || strings.Contains(stderr, "linus@example.com") {
				t.Fatalf("exit %d, stderr %q; want 1 naming CLOUDSDK_CORE_ACCOUNT, never the account it names", code, stderr)
			}
			for _, account := range accounts {
				if !strings.Contains(stderr, account) {
					t.Fatalf("stderr %q does not name the login %s gcloud holds", stderr, account)
				}
			}
			if _, calls := fake.CalledUnder(); calls != 0 {
				t.Fatalf("Secret Manager got %d calls, want none", calls)
			}
			if lent := gcloud.Lent(t); len(lent) != 0 {
				t.Fatalf("gcloud lent tokens for %v, want none", lent)
			}
		})
	}
}

// TestGCPSecretGcloudRunsWithItsOwnSettingsAndNoLogs: gcloud runs with the caller's own gcloud
// settings - here impersonation, which the token it lends then acts as, and which the "using"
// line names - and always with file logging, HTTP logging and prompts off, though this command's
// environment turns file logging and HTTP logging on and their token redaction off; its token
// command prints the token in the private format, which bypasses gcloud's log file, and clears
// none of gcloud's own auth properties.
func TestGCPSecretGcloudRunsWithItsOwnSettingsAndNoLogs(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1188); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	t.Run("the caller's settings, and no logs", func(t *testing.T) {
		fake := gcptest.NewFakeSecretManager()
		gcptest.UseFake(t, fake)
		gcptest.UseNoADC(t)
		gcloud := gcptest.UseGcloud(t, "ada@example.com")
		t.Setenv("CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT", "robot@example.iam.gserviceaccount.com")
		t.Setenv("CLOUDSDK_CORE_DISABLE_FILE_LOGGING", "false")
		t.Setenv("CLOUDSDK_CORE_LOG_HTTP", "true")
		t.Setenv("CLOUDSDK_CORE_LOG_HTTP_REDACT_TOKEN", "false")
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent")
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if !strings.Contains(stderr, "agent-secrets secret create: using the active gcloud account (gcloud configuration default): ada@example.com impersonating robot@example.iam.gserviceaccount.com\n") {
			t.Fatalf("stderr %q does not name the login and the account it impersonates", stderr)
		}
		requireGcloudRuns(t, gcloud, map[string]string{
			"CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT": "robot@example.iam.gserviceaccount.com",
			"CLOUDSDK_CORE_DISABLE_FILE_LOGGING":        "1",
			"CLOUDSDK_CORE_DISABLE_PROMPTS":             "1",
			"CLOUDSDK_CORE_LOG_HTTP":                    "false",
		}, "")
		var token []string
		for _, command := range gcloud.Commands(t) {
			if words := strings.Fields(command); len(words) > 1 && words[1] == "print-access-token" {
				token = words
			}
		}
		if !slices.Contains(token, "--format=value[private](token)") {
			t.Errorf("gcloud's token command %q lacks --format=value[private](token)", token)
		}
		for _, word := range token {
			for _, cleared := range []string{"--impersonate-service-account=", "--access-token-file=", "--credential-file-override="} {
				if strings.HasPrefix(word, cleared) {
					t.Errorf("gcloud's token command %q carries %s, which overrides gcloud's own setting", token, word)
				}
			}
		}
		if len(token) == 0 || token[len(token)-1] != "ada@example.com" {
			t.Errorf("gcloud's token command %q does not end with the account", token)
		}
	})
}

// requireGcloudRuns fails t unless gcloud ran, every run's environment held each variable of want
// at its value ("unset" for one it must not hold), and, where flag is not empty, every command
// line carried flag.
func requireGcloudRuns(t *testing.T, gcloud gcptest.GcloudStub, want map[string]string, flag string) {
	t.Helper()
	runs, commands := gcloud.Environments(t), gcloud.Commands(t)
	if len(runs) == 0 {
		t.Fatal("gcloud never ran")
	}
	for i, run := range runs {
		for name, value := range want {
			if run[name] != value {
				t.Errorf("gcloud run %d (%s): %s=%q, want %q", i, commands[i], name, run[name], value)
			}
		}
		if flag != "" && !slices.Contains(strings.Fields(commands[i]), flag) {
			t.Errorf("gcloud run %d (%s) lacks %s", i, commands[i], flag)
		}
	}
}

// TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice: the command a write prints for piping a
// value in, after refusing a value of more than one line at the prompt, starts with the
// CLOUDSDK_CORE_ACCOUNT that chose the gcloud login, shell-quoted, so running it picks the same
// login among several; with a single login it chose none, and the command carries none.
func TestGCPSecretPipeCommandKeepsTheGcloudAccountChoice(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1275); it signals the test process or changes its signal handling (var main.readHidden, main.readHiddenAtTerminal, (*promptTerminal).raise, a signal to this process (unix.Kill) at secret_prompt_unix.go:520); it sets a package variable the package's other tests read (readHidden set at secret_gcp_test.go:1278).
	for _, row := range []struct {
		name     string
		accounts []string
		named    string
		want     string
	}{
		{"several logins, one named", []string{machineEmail, "ada@example.com", "grace@example.com"}, "grace@example.com",
			"CLOUDSDK_CORE_ACCOUNT=grace@example.com agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"a named login that needs quoting", []string{"ada@example.com", "grace+ops@example.com"}, "grace+ops@example.com",
			"CLOUDSDK_CORE_ACCOUNT='grace+ops@example.com' agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
		{"one login, none named", []string{"ada@example.com"}, "",
			": agent-secrets secret create SCRATCH_TEST --owner me --tier agent < FILE"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseNoADC(t)
			gcptest.UseGcloud(t, row.accounts...)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", row.named)
			broker := startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useTerminal(t)
			readHidden = func(int, time.Time, func(), func(error)) ([]byte, error) { return nil, errMoreThanOneLine }

			_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
			if code != exitUsageError || !strings.Contains(stderr, row.want) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, exitUsageError, row.want)
			}
			requireNothingLeft(t, fake, broker, "SCRATCH_TEST")
		})
	}
}

// useGcloudThatAnswers puts a stub gcloud alone on PATH for the rest of t that holds one login,
// account, answers `auth print-access-token` with stubTokenPrefix + account, and answers
// `auth list` (account, active) and `config list` (no auth property) as authList and configList
// say: "" answers, "fail" fails as gcloud does, naming the failure on its standard error, and
// "hang" answers nothing for 30 s. It first clears every other source, as useNoADC does.
func useGcloudThatAnswers(t *testing.T, account, authList, configList string) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	gcptest.UseNoADC(t)
	behave := func(command, answer, mode string) string {
		switch mode {
		case "fail":
			return `echo 'ERROR: (gcloud.` + command + `) the test fails it' >&2; exit 1`
		case "hang":
			return "exec " + secretstore.ShellWord(sleep) + " 30"
		}
		return "printf '%s\\n' " + secretstore.ShellWord(answer)
	}
	script := strings.Join([]string{
		"#!/bin/sh",
		`case "$1 $2" in`,
		`"auth list") ` + behave("auth.list", `[{"account":"`+account+`","status":"ACTIVE"}]`, authList) + ` ;;`,
		`"config list") ` + behave("config.list", `{"auth":{}}`, configList) + ` ;;`,
		`"auth print-access-token") printf '%s\n' ` + secretstore.ShellWord(gcptest.StubTokenPrefix+account) + ` ;;`,
		`*) printf 'stub gcloud: unexpected command: %s\n' "$*" >&2; exit 2 ;;`,
		"esac",
		"",
	}, "\n")
	dir := t.TempDir()
	if err := testexec.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
}

// TestGCPSecretGcloudSettingsReadIsBestEffort: the gcloud source reads gcloud's auth settings only
// to name whom its token acts as (R2), so a `gcloud config list` that fails, or that does not
// answer within gcp.GcloudSettingsDeadline, never stops the command: the form runs under the
// login's token, and the using line names the login with the principal unknown. The form is timed
// against gcp.GcloudSettingsDeadline where the read hangs, and against no deadline where it fails,
// less the time this process was held up (requireOnTime).
func TestGCPSecretGcloudSettingsReadIsBestEffort(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGcloudThatAnswers, main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGcloudThatAnswers, main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireOnTime, onTimeWithin at waits_test.go:239).
	for _, mode := range []string{"fail", "hang"} {
		t.Run(mode, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			useGcloudThatAnswers(t, "ada@example.com", "", mode)
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			waits := time.Duration(0)
			if mode == "hang" {
				waits = gcp.GcloudSettingsDeadline
			}
			meter := startStallMeter(t)
			started := time.Now()
			_, stderr, code := runSecret("list")
			took := requireOnTime(t, meter, "the form whose gcloud settings read "+mode+"s", started, waits)
			if code != 0 {
				t.Fatalf("exit %d after %s, stderr %q; want 0", code, took, stderr)
			}
			if want := "agent-secrets secret list: using the active gcloud account (gcloud configuration default): ada@example.com (principal unknown: gcloud's auth settings could not be read)\n"; !strings.Contains(stderr, want) {
				t.Fatalf("stderr %q does not say %q", stderr, want)
			}
			if got, calls := fake.CalledUnder(); got != "Bearer "+gcptest.StubTokenPrefix+"ada@example.com" || calls == 0 {
				t.Fatalf("Secret Manager calls ran under %q (%d calls), want only ada@example.com's gcloud token", got, calls)
			}
		})
	}
}

// TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline: `gcloud auth list`, which
// decides whether the gcloud source is present, runs under secretstore.CallDeadline as the token's
// refresh does, so a gcloud that never answers stops the command at the deadline, naming it,
// before any Secret Manager call, timed against that deadline (requireOnTime).
func TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGcloudThatAnswers, main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStoreCallDeadline, storeCallDeadline set at secret_create_test.go:36); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireOnTime, onTimeWithin at waits_test.go:239).
	useStoreCallDeadline(t, 300*time.Millisecond)
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	useGcloudThatAnswers(t, "ada@example.com", "hang", "")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	meter := startStallMeter(t)
	started := time.Now()
	_, stderr, code := runSecret("list")
	took := requireOnTime(t, meter, "the form whose `gcloud auth list` never answers", started, 300*time.Millisecond)
	if code != 1 || !strings.Contains(stderr, "`gcloud auth list` did not answer within 300ms") {
		t.Fatalf("exit %d after %s, stderr %q; want 1 naming the 300ms deadline", code, took, stderr)
	}
	if _, calls := fake.CalledUnder(); calls != 0 {
		t.Fatalf("Secret Manager saw %d calls, want none", calls)
	}
}

// TestGCPSecretWithNoApplicationDefaultCredentialsUsesTheGcloudLogin: where there are no
// Application Default Credentials and GOOGLE_APPLICATION_CREDENTIALS is unset, every form, a read
// included, runs under the gcloud login.
func TestGCPSecretWithNoApplicationDefaultCredentialsUsesTheGcloudLogin(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseNoADC(t)
	gcloud := gcptest.UseGcloud(t, "ada@example.com", machineEmail)
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	held, err := fake.CreateSecret(context.Background(), &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/" + testGCPProject, SecretId: "agent-secrets-demo-deploy-token",
		Secret: &secretmanagerpb.Secret{Annotations: map[string]string{policy.TagOwner: "grace@example.com", policy.TagTier: "agent"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fake.AddSecretVersion(context.Background(), &secretmanagerpb.AddSecretVersionRequest{Parent: held.GetName()}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "DEMO_DEPLOY_TOKEN") {
		t.Fatalf("list stdout: %q", stdout)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")

	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	if owner := fakeOwnerOf(fake, "scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")
}

// TestGCPSecretWithNoCredentialAtAllIsRefused: with no CLOUDSDK_AUTH_ACCESS_TOKEN, no Application
// Default Credentials, and no gcloud login either, a read is refused (exit 1), naming the sources
// it tried, before any Secret Manager call.
func TestGCPSecretWithNoCredentialAtAllIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useNoADC, gcpMetadataToken set at secret_gcp_test.go:536).
	for _, row := range []struct {
		name   string
		gcloud func(t *testing.T)
	}{
		{"gcloud holds no login", func(t *testing.T) { gcptest.UseGcloud(t) }},
		{"no gcloud on PATH", gcptest.UseNoGcloud},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			gcptest.UseFake(t, fake)
			gcptest.UseNoADC(t)
			row.gcloud(t)
			startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

			_, stderr, code := runSecret("list")
			if code != 1 || !strings.Contains(stderr, "CLOUDSDK_AUTH_ACCESS_TOKEN") || !strings.Contains(stderr, "gcloud") {
				t.Fatalf("exit %d, stderr %q; want 1 naming each source it tried", code, stderr)
			}
			if _, calls := fake.CalledUnder(); calls != 0 {
				t.Fatalf("Secret Manager got %d calls, want none", calls)
			}
		})
	}
}

// TestGCPSecretCreateWithLentPersonsAccessToken: without ADC or a gcloud login, a person's lent
// token whose account Google's userinfo endpoint names creates the secret and supplies its
// --owner me email.
func TestGCPSecretCreateWithLentPersonsAccessToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useLentGCPAccessToken, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useLentGCPAccessToken, gcpUserinfo set at secret_gcp_test.go:551).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseNoADC(t)
	gcptest.UseNoGcloud(t)
	gcptest.UseLentAccessToken(t, "lent-ada-token", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if owner := fakeOwnerOf(fake, "scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	if got, calls := fake.CalledUnder(); got != "Bearer lent-ada-token" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want the lent token", got, calls)
	}
}

// TestGCPSecretOwnerMeUnderAServiceAccountIsRefused: --owner me under a sign-in Google's userinfo
// endpoint names as a service account - here a lent token beside a service account's Application
// Default Credentials, the lent token being the one userinfo is asked about - is refused (exit 2),
// naming the account and --owner <email>, before the value is read and with nothing written: a
// service account names no person.
func TestGCPSecretOwnerMeUnderAServiceAccountIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1521); it signals the test process or changes its signal handling ((gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1524).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseIdentity(t, "service_account", machineEmail)
	gcptest.UseNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-service-token")
	var inspected string
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: func(_ context.Context, creds *google.Credentials) (string, error) {
		token, err := creds.TokenSource.Token()
		if err != nil {
			return "", err
		}
		inspected = token.AccessToken
		return machineEmail, nil
	}}))
	broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	refuseValueReads(t)

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != exitUsageError || !strings.Contains(stderr, "--owner <email>") || !strings.Contains(stderr, machineEmail) {
		t.Fatalf("exit %d, stderr %q; want %d naming the service account and --owner <email>", code, stderr, exitUsageError)
	}
	if inspected != "lent-service-token" {
		t.Fatalf("userinfo inspected token %q, want the lent token", inspected)
	}
	requireNothingWritten(t, fake, broker)
}

// TestGCPSecretLentTokenWithNoUserinfoEmailIsRefused: no empty userinfo email can become an
// owner: --owner me is refused (exit 2), naming --owner <email>, before a value is read or
// anything is written.
func TestGCPSecretLentTokenWithNoUserinfoEmailIsRefused(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1555); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.refuseValueReads, secretStdin set at secret_gcp_test.go:751).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseNoADC(t)
	gcptest.UseNoGcloud(t)
	t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-unknown-token")
	broker := startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	refuseValueReads(t)

	_, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != exitUsageError || !strings.Contains(stderr, "names no email") || !strings.Contains(stderr, "--owner <email>") {
		t.Fatalf("exit %d, stderr %q; want the empty-userinfo refusal naming --owner <email>", code, stderr)
	}
	requireNothingWritten(t, fake, broker)
}

// TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud: GOOGLE_APPLICATION_CREDENTIALS
// names the credentials every form uses, so the gcloud login never stands in for them, though
// gcloud holds a person's login: a file there that cannot be loaded is refused before any Secret
// Manager call, and a write runs under a machine's credentials named there, with gcloud never run.
func TestGCPSecretUnderGoogleApplicationCredentialsNeverAsksGcloud(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1577); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	t.Run("a file that cannot be loaded", func(t *testing.T) {
		fake := gcptest.NewFakeSecretManager()
		gcptest.UseFake(t, fake)
		gcptest.UseNoADC(t)
		gcloud := gcptest.UseGcloud(t, "ada@example.com")
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "absent.json"))
		startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

		_, stderr, code := runSecret("list")
		if code != 1 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if _, calls := fake.CalledUnder(); calls != 0 {
			t.Fatalf("Secret Manager got %d calls, want none", calls)
		}
		if commands := gcloud.Commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
	t.Run("a machine's credentials, for a write", func(t *testing.T) {
		fake := gcptest.NewFakeSecretManager()
		gcptest.UseFake(t, fake)
		gcloud := gcptest.UseGcloud(t, "ada@example.com")
		gcptest.UseIdentity(t, "service_account", machineEmail)
		startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
		useStdin(t, "shh")

		if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "human"); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if got, calls := fake.CalledUnder(); got != "Bearer fake" || calls == 0 {
			t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the named credentials", got, calls)
		}
		if commands := gcloud.Commands(t); len(commands) != 0 {
			t.Fatalf("gcloud ran %q, want nothing", commands)
		}
	})
}

// TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud: a person's own Application Default
// Credentials (a laptop's) write as they always have, every call under them, with gcloud never
// run, though it holds another person's login.
func TestGCPSecretWriteUnderAPersonsADCNeverAsksGcloud(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcloud := gcptest.UseGcloud(t, "grace@example.com")
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if owner := fakeOwnerOf(fake, "scratch-test"); owner != "ada@example.com" {
		t.Fatalf("owner annotation %q, want ada@example.com", owner)
	}
	if got, calls := fake.CalledUnder(); got != "Bearer fake" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the person's own ADC", got, calls)
	}
	if commands := gcloud.Commands(t); len(commands) != 0 {
		t.Fatalf("gcloud ran %q, want nothing", commands)
	}
}

// TestGCPSecretListSucceedsWithoutUserinfoScope: a read makes no userinfo call, so it succeeds
// even under a token that lacks the scope that call needs. On a machine whose gcloud holds a login
// beside the machine's own identity, it runs under that login, the active gcloud account, which
// R1 puts before the metadata server.
func TestGCPSecretListSucceedsWithoutUserinfoScope(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1648).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcloud := gcptest.UseGcloud(t, "ada@example.com")
	gcptest.UseIdentity(t, "", machineEmail)
	var userinfoCalls int
	t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: func(context.Context, *google.Credentials) (string, error) {
		userinfoCalls++
		return "", errors.New("insufficient authentication scopes")
	}}))
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	requireRunUnder(t, fake, gcloud, "ada@example.com")
	if userinfoCalls != 0 {
		t.Fatalf("a read made %d userinfo calls, want none", userinfoCalls)
	}
}

// TestGCPSecretRetagKeepsUnrelatedAnnotations: retag changes only owner and tier, never an
// annotation it does not know about (an UpdateSecret whose mask names "annotations" replaces the
// whole map unless retag reads and merges the current one first).
func TestGCPSecretRetagKeepsUnrelatedAnnotations(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")
	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}

	id := testGCPPrefix + "scratch-test"
	fake.Mu.Lock()
	fake.Secrets[id].Secret.Annotations["unrelated-note"] = "do-not-delete"
	fake.Mu.Unlock()

	if _, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human"); code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}

	fake.Mu.Lock()
	got := fake.Secrets[id].Secret.Annotations["unrelated-note"]
	fake.Mu.Unlock()
	if got != "do-not-delete" {
		t.Fatalf("retag dropped an unrelated annotation: got %q", got)
	}
}

// TestGCPSecretOwnerMeIsTheSignInEmailLowercased: --owner me is the userinfo email, lowercased.
func TestGCPSecretOwnerMeIsTheSignInEmailLowercased(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	gcptest.UseFake(t, gcptest.NewFakeSecretManager())
	gcptest.UseIdentity(t, "authorized_user", "Ada@Example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	stdout, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=ada@example.com") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne: retag with only --tier keeps the held owner,
// sending both as one UpdateSecret call (the annotations map is written whole).
func TestGCPSecretRetagOnlyNamedFieldReplacesHeldOne(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	gcptest.UseFake(t, gcptest.NewFakeSecretManager())
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 0 {
		t.Fatalf("retag: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "owner=shared, tier=human") {
		t.Fatalf("stdout: %q", stdout)
	}
}

// TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt: IAM (PermissionDenied) refuses a
// person's change to a shared secret's annotations; the CLI prints GCP's refusal and says whose
// change it is.
func TestGCPSecretRetagOfASharedSecretSaysAnAdministratorChangesIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "shared", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	fake.Mu.Lock()
	fake.RefuseAnnotationUpdates = true
	fake.Mu.Unlock()

	_, stderr, code := runSecret("retag", "SCRATCH_TEST", "--tier", "human")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "PermissionDenied") {
		t.Fatalf("stderr does not show GCP's refusal: %q", stderr)
	}
	if !strings.Contains(stderr, "a shared secret's owner and tier are an administrator's to change") {
		t.Fatalf("stderr does not say whose change it is: %q", stderr)
	}
}

// TestGCPSecretListShowsOwnerTierAndValue: list shows every secret under the prefix, its owner,
// tier and whether it has a value, never the value.
func TestGCPSecretListShowsOwnerTierAndValue(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	gcptest.UseFake(t, gcptest.NewFakeSecretManager())
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	useStdin(t, "shh")

	if _, stderr, code := runSecret("create", "SCRATCH_TEST", "--owner", "me", "--tier", "agent"); code != 0 {
		t.Fatalf("create: exit %d, stderr %q", code, stderr)
	}
	stdout, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("list: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "SCRATCH_TEST") || !strings.Contains(stdout, "ada@example.com") || !strings.Contains(stdout, "agent") || !strings.Contains(stdout, "yes") {
		t.Fatalf("list stdout: %q", stdout)
	}
	if strings.Contains(stdout, "shh") {
		t.Fatalf("list printed the value: %q", stdout)
	}
}

// TestGCPSecretReadsRefuseBadSettings: an invalid kms_key_name is refused before any GCP call.
// TestCheckSettingsKMSKeyName covers the other checkSettingsKMSKeyName refusals directly.
func TestGCPSecretReadsRefuseBadSettings(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useGCPIdentity, gcpCredentialsFile set at secret_gcp_test.go:473).
	gcptest.UseFake(t, gcptest.NewFakeSecretManager())
	gcptest.UseIdentity(t, "authorized_user", "ada@example.com")
	startSecretBrokerWith(t, Settings{
		Store:         gcp.Name,
		SecretsPrefix: testGCPPrefix,
		Fields:        map[string]string{"gcp_project": testGCPProject, "kms_key_name": "not-a-key-name"},
	}, alwaysServed)

	_, stderr, code := runSecret("list")
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "invalid KMS key name") {
		t.Fatalf("stderr: %q", stderr)
	}
}

// TestGCPSecretChosenConfigurationNamesAGcloudLogin: CLOUDSDK_ACTIVE_CONFIG_NAME names a gcloud
// configuration, and a form runs under that configuration's login, though another is active.
func TestGCPSecretChosenConfigurationNamesAGcloudLogin(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1852); it sets a package variable the package's other tests read (main.startFakeSecretManager, main.useGCPDialTo, main.replaceGCPDial, gcpClientDial set at secret_gcp_test.go:381).
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	useGcloudConfigurations(t, "box", orderConfigurations)
	t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person")
	startSecretBrokerWith(t, gcpTestSettings(), alwaysServed)
	_, stderr, code := runSecret("list")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	if got, calls := fake.CalledUnder(); got != "Bearer gcloud-not-a-secret-for-grace@example.com" || calls == 0 {
		t.Fatalf("Secret Manager calls ran under %q (%d calls), want only the person configuration's grace@example.com", got, calls)
	}
}

// TestGCPSecretRefusalNamesTheSourceAndTheChoice: a store refusal names the source the
// credential came from and whom it acts as - or that the principal is unknown, with no userinfo
// call to learn it - and how to choose another, built from that source: a lent token outranks
// every other source, so only unsetting it chooses another; a credentials file outranks the gcloud
// variables, so they choose only once it is unset; under every later source the gcloud variables
// and GOOGLE_APPLICATION_CREDENTIALS choose, with the gcloud configurations here. Where
// CLOUDSDK_CORE_ACCOUNT is set, it outranks a configuration's account, so a configuration chooses
// only once it is unset, and the advice says so. A create that IAM lets make the secret but not
// add its value says the same.
func TestGCPSecretRefusalNamesTheSourceAndTheChoice(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1884); it sets a package variable the package's other tests read (gcpUserinfo set at secret_gcp_test.go:1924).
	const (
		later       = "choose another with a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME, an account in CLOUDSDK_CORE_ACCOUNT, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS (gcloud configurations here: box, person)"
		lent        = "choose another by unsetting CLOUDSDK_AUTH_ACCESS_TOKEN, which outranks every other source, or by lending another token in it"
		file        = "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME or an account in CLOUDSDK_CORE_ACCOUNT (gcloud configurations here: box, person)"
		account     = "choose another with an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or a credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting CLOUDSDK_CORE_ACCOUNT to choose a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME (gcloud configurations here: box, person)"
		fileAccount = "choose another by naming another credentials file in GOOGLE_APPLICATION_CREDENTIALS, or by unsetting it, which outranks every gcloud source, to choose an account in CLOUDSDK_CORE_ACCOUNT, which outranks a configuration's account, or, with CLOUDSDK_CORE_ACCOUNT unset too, a gcloud configuration in CLOUDSDK_ACTIVE_CONFIG_NAME (gcloud configurations here: box, person)"
	)
	useCredentialsFile := func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		writeServiceAccountFile(t, path, "gac@example.iam.gserviceaccount.com", startGoogleTokenServer(t))
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	}
	for _, row := range []struct {
		name, refused, choice string
		use                   func(t *testing.T)
	}{
		{"metadata alone", "the metadata server: machine@example.iam.gserviceaccount.com", later, func(t *testing.T) { t.Setenv("PATH", t.TempDir()) }},
		{"active gcloud", "the active gcloud account (gcloud configuration box): box@example.iam.gserviceaccount.com", later, func(*testing.T) {}},
		{"chosen configuration", "the gcloud configuration person (CLOUDSDK_ACTIVE_CONFIG_NAME): grace@example.com", later, func(t *testing.T) { t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "person") }},
		{"chosen account, a configuration chosen too", "the gcloud account (CLOUDSDK_CORE_ACCOUNT): grace@example.com", account, func(t *testing.T) {
			t.Setenv("CLOUDSDK_ACTIVE_CONFIG_NAME", "box")
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}},
		{"lent token", "the access token in CLOUDSDK_AUTH_ACCESS_TOKEN: principal unknown", lent, func(t *testing.T) { t.Setenv("CLOUDSDK_AUTH_ACCESS_TOKEN", "lent-token") }},
		{"credentials file", "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: gac@example.iam.gserviceaccount.com", file, useCredentialsFile},
		{"credentials file, an account exported", "the credentials file GOOGLE_APPLICATION_CREDENTIALS names: gac@example.iam.gserviceaccount.com", fileAccount, func(t *testing.T) {
			useCredentialsFile(t)
			t.Setenv("CLOUDSDK_CORE_ACCOUNT", "grace@example.com")
		}},
	} {
		for _, refusal := range []struct {
			name, says string
			setUp      func(fake *gcptest.FakeSecretManager)
		}{
			{"the store refuses the create", "PermissionDenied", func(fake *gcptest.FakeSecretManager) {
				fake.RefuseMethods = map[string]codes.Code{"CreateSecret": codes.PermissionDenied}
			}},
			{"IAM grants the create and not the add", "may create " + testGCPPrefix + "new-test but not add its value", func(fake *gcptest.FakeSecretManager) {
				fake.Withheld = []string{"secretmanager.versions.add"}
			}},
		} {
			t.Run(row.name+", "+refusal.name, func(t *testing.T) {
				fake := gcptest.NewFakeSecretManager()
				refusal.setUp(fake)
				gcptest.UseFake(t, fake)
				startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
				useGcloudConfigurations(t, "box", orderConfigurations)
				row.use(t)
				var userinfoCalls int
				t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: func(context.Context, *google.Credentials) (string, error) {
					userinfoCalls++
					return "", errors.New("the test answers no userinfo")
				}}))
				startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
				useStdin(t, "")

				_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "shared", "--tier", "agent")
				if code != 1 || !strings.Contains(stderr, refusal.says) {
					t.Fatalf("exit %d, stderr %q; want 1 saying %q", code, stderr, refusal.says)
				}
				for _, said := range []string{row.refused, row.choice} {
					if !strings.Contains(stderr, said) {
						t.Fatalf("stderr %q does not say %q", stderr, said)
					}
				}
				for _, other := range []string{later, lent, file, account, fileAccount} {
					if other != row.choice && strings.Contains(stderr, other) {
						t.Fatalf("stderr %q gives advice for another source than %s: %q", stderr, row.name, other)
					}
				}
				if userinfoCalls != 0 {
					t.Fatalf("the refusal made %d userinfo calls, want none", userinfoCalls)
				}
			})
		}
	}
}

// TestGCPSecretRefusalNamesThePrincipalOfEachSource proves R2's per-source naming: a store refusal
// names the account the credential acts as for each source that can name one - the metadata server
// (its email), a service-account key file (its client_email), a gcloud login (its account); names
// "principal unknown" for an authorized_user file that holds no email; and, when the metadata
// server's email read does not answer, still signs in under its token and names the generic
// identity. No source makes a userinfo call to learn a principal, and every row reaches a store
// call, which is what signing in proves.
func TestGCPSecretRefusalNamesThePrincipalOfEachSource(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_test.go:1973); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcpMetadataPrincipal set at secret_gcp_test.go:1977).
	const (
		dev   = "dev-box@acme.iam.gserviceaccount.com"
		robot = "robot@acme.iam.gserviceaccount.com"
		ada   = "ada@example.com"
	)
	for _, row := range []struct {
		name    string
		setUp   func(t *testing.T)
		refused string
	}{
		{"metadata names the account", func(t *testing.T) { gcptest.UseIdentity(t, "", dev); t.Setenv("PATH", t.TempDir()) }, "the metadata server: " + dev},
		{"metadata email read does not answer, generic wording", func(t *testing.T) {
			gcptest.UseIdentity(t, "", dev)
			t.Setenv("PATH", t.TempDir())
			t.Cleanup(gcp.SwapHooks(gcp.Hooks{MetadataPrincipal: func(context.Context) string { return "" }}))
		}, "the metadata server: this machine's or this pod's identity"},
		{"service-account key file names client_email", func(t *testing.T) { gcptest.UseIdentity(t, "service_account", robot) }, ": " + robot},
		{"authorized_user file cannot name", func(t *testing.T) { gcptest.UseIdentity(t, "authorized_user", ada) }, ": principal unknown"},
		{"gcloud login names the account", func(t *testing.T) {
			gcptest.UseNoADC(t)
			gcptest.UseGcloud(t, ada)
		}, "the active gcloud account (gcloud configuration default): " + ada},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := gcptest.NewFakeSecretManager()
			fake.RefuseMethods = map[string]codes.Code{"CreateSecret": codes.PermissionDenied}
			gcptest.UseFake(t, fake)
			row.setUp(t)
			var userinfoCalls int
			t.Cleanup(gcp.SwapHooks(gcp.Hooks{Userinfo: func(context.Context, *google.Credentials) (string, error) {
				userinfoCalls++
				return "", errors.New("the test answers no userinfo")
			}}))
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			useStdin(t, "")

			_, stderr, code := runSecret("create", "NEW_TEST", "--owner", "shared", "--tier", "agent")
			if code != 1 || !strings.Contains(stderr, "PermissionDenied") {
				t.Fatalf("exit %d, stderr %q; want 1 with the store's PermissionDenied", code, stderr)
			}
			if !strings.Contains(stderr, "the store refused ") || !strings.Contains(stderr, row.refused) {
				t.Fatalf("stderr %q does not name the refused principal %q", stderr, row.refused)
			}
			if _, calls := fake.CalledUnder(); calls == 0 {
				t.Fatalf("no store call was made, so the credential never signed in")
			}
			if userinfoCalls != 0 {
				t.Fatalf("the refusal made %d userinfo calls, want none", userinfoCalls)
			}
		})
	}
}
