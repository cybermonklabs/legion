package main

import (
	"context"
	"maps"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"

	// racingTagWrites is a awstest.Local whose first tag write lets another writer's write (race) land
	// first, as a second retag of the same secret between this one's read and its write does.
	awsstore "github.com/sjawhar/envoy/internal/cipher/secretstore/aws"
)

type racingTagWrites struct {
	*awstest.Local
	race  func()
	raced bool
}

func (r *racingTagWrites) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	r.first()
	return r.Local.TagResource(ctx, in, o...)
}

func (r *racingTagWrites) UntagResource(ctx context.Context, in *secretsmanager.UntagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.UntagResourceOutput, error) {
	r.first()
	return r.Local.UntagResource(ctx, in, o...)
}

func (r *racingTagWrites) first() {
	if !r.raced {
		r.raced = true
		r.race()
	}
}

// loginWithOwnersLeft is a google-login secret that still holds the github-owners tag a github-app
// type left on it, as retagging a GitHub App secret to a Google login leaves it.
func loginWithOwnersLeft(name string) map[string]string {
	tags := policytest.TypeSecret(name, "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin)).Tags
	tags[policy.TagGitHubOwners] = "ada"
	return tags
}

// TestAConcurrentRetagIsReportedAndOverwritesNothing: `--github-owners none` on a google-login
// secret is checked against that type, which reads no owners. When another writer retags the
// secret github-app, keeping its owners, between that check and the write, the removal leaves a
// github-app secret with no owners, which the broker refuses. On GCP the write is made against
// the read it was checked on (its etag), so the store refuses it, naming the change, and the
// other writer's tags stand. AWS has no conditional tag write, so the retag reads the tags back,
// finds the pair the broker refuses, and reports it: it names the tags it read and the tags it
// found, and the command that sets the secret right, and it writes nothing, so the other
// writer's retag stands rather than being taken back.
func TestAConcurrentRetagIsReportedAndOverwritesNothing(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.startSecretBrokerWith, t.Setenv at secret_test.go:293); it signals the test process or changes its signal handling (main.useGCPIdentity, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	t.Run("aws", func(t *testing.T) {
		secret := policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin))
		secret.Tags = loginWithOwnersLeft("APP_KEY")
		local := awstest.NewLocal(secret)
		broker := startSecretBroker(t, servedBy(local))
		writes := &racingTagWrites{Local: local}
		writes.race = func() {
			if _, err := local.TagResource(context.Background(), &secretsmanager.TagResourceInput{
				SecretId: aws.String(policytest.ID("APP_KEY")), Tags: awsstore.TagList(map[string]string{policy.TagType: policy.TypeGitHubApp}),
			}); err != nil {
				t.Errorf("the other writer's retag: %v", err)
			}
		}
		useAWS(t, writes, testAccount, adaSignIn)
		_, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "none")
		for _, said := range []string{
			"another write changed the secret after this retag read it",
			"nothing was restored",
			"type=" + policy.TypeGoogleLogin,
			policy.TagGitHubOwners + "=ada",
			"type=" + policy.TypeGitHubApp,
			"agent-secrets secret retag APP_KEY --type " + policy.TypeGitHubApp,
			"agent-secrets secret retag APP_KEY --type " + policy.TagRemoved,
		} {
			if !strings.Contains(stderr, said) {
				t.Fatalf("racing retag: stderr %q does not say %q", stderr, said)
			}
		}
		if code != 1 {
			t.Fatalf("racing retag: exit %d; want 1", code)
		}
		want := maps.Clone(secret.Tags)
		want[policy.TagType] = policy.TypeGitHubApp
		delete(want, policy.TagGitHubOwners)
		if tags := awsstore.TagMap(describe(t, local, "APP_KEY").Tags); !maps.Equal(tags, want) {
			t.Fatalf("tags after the race = %v; want the other writer's type with this retag's removal, %v, and nothing restored", tags, want)
		}
		if rereads, _ := broker.recorded(); len(rereads) != 1 || rereads[0] != "APP_KEY" {
			t.Fatalf("rereads = %v; want one of APP_KEY: the tags changed twice, so the broker is asked to read them as they now stand", rereads)
		}
	})
	t.Run("gcp", func(t *testing.T) {
		fake := newFakeSecretManager()
		startFakeSecretManager(t, fake)
		useGCPIdentity(t, "authorized_user", "ada@example.com")
		settings := gcpTestSettings()
		broker := startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
		id := testGCPPrefix + "app-key"
		fake.secrets[id] = &fakeSecret{
			secret:      &secretmanagerpb.Secret{Name: id, Annotations: loginWithOwnersLeft("APP_KEY"), Etag: "1"},
			versions:    []*secretmanagerpb.SecretVersion{{Name: id + "/versions/1", State: secretmanagerpb.SecretVersion_ENABLED}},
			payloads:    [][]byte{[]byte(policytest.TypeValue(policy.TypeGoogleLogin))},
			etagCounter: 1,
		}
		fake.race = func(entry *fakeSecret) {
			annotations := maps.Clone(entry.secret.Annotations)
			annotations[policy.TagType] = policy.TypeGitHubApp
			entry.secret.Annotations = annotations
			entry.etagCounter++
			entry.secret.Etag = "2"
		}
		_, stderr, code := runSecret("retag", "APP_KEY", "--github-owners", "none")
		if code != 1 || !strings.Contains(stderr, "another write changed the secret after this retag read it") {
			t.Fatalf("racing retag: exit %d stderr %q; want 1 naming the change", code, stderr)
		}
		fake.mu.Lock()
		annotations, gets := fake.secrets[id].secret.GetAnnotations(), fake.gets
		fake.mu.Unlock()
		if annotations[policy.TagType] != policy.TypeGitHubApp || annotations[policy.TagGitHubOwners] != "ada" {
			t.Fatalf("annotations after the race = %v; want the other writer's github-app with its owners", annotations)
		}
		if gets != 1 {
			t.Fatalf("the retag read the secret %d times; want once, the read its write was made against", gets)
		}
		if rereads, _ := broker.recorded(); len(rereads) != 0 {
			t.Fatalf("rereads = %v; want none after a refused write", rereads)
		}
	})
}

// TestARetagExitsOneWhileTheBrokerServesTheOldType: `broker: serving` alone does not say the
// broker reads the tags a retag wrote, since a reread can read them stale. A retag to a type whose
// reread answers the secret served as stored (or as another type) exits 1, saying the broker still
// sees the old tags, to run the retag again, and to store no value yet; the reread answering the
// type the retag wrote exits 0. A retag that removes the type is held to the same rule.
//
// Each refused row also reads the type the refusal names. Without that, the message could name
// any type at all, or none, and this test would stay green: the operator it is written for has
// to be told which type the broker is still serving, because that is what says whether the retag
// has to be run again or something else wrote the secret. A secret the broker serves with no
// type at all is named "a stored secret" (typeName), the words the refusal uses for it.
func TestARetagExitsOneWhileTheBrokerServesTheOldType(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useAWS, t.Setenv at secret_test.go:372); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	for _, row := range []struct {
		name, served string
		args         []string
		code         int
		// refusal is what the stderr of a refused row must name: the type the broker serves and
		// the type the retag wrote, in the refusal's own words.
		refusal string
	}{
		{"stale: still stored", "", []string{"--type", policy.TypeGoogleLogin}, 1, "serves LOGIN_KEY as a stored secret, not " + policy.TypeGoogleLogin},
		{"stale: the other type", policy.TypeGitHubApp, []string{"--type", policy.TypeGoogleLogin}, 1, "serves LOGIN_KEY as " + policy.TypeGitHubApp + ", not " + policy.TypeGoogleLogin},
		{"read", policy.TypeGoogleLogin, []string{"--type", policy.TypeGoogleLogin}, 0, ""},
		{"removal stale", policy.TypeGoogleLogin, []string{"--type", "none"}, 1, "serves LOGIN_KEY as " + policy.TypeGoogleLogin + ", not a stored secret"},
		{"removal read", "", []string{"--type", "none"}, 0, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := awstest.NewLocal(policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policytest.TypeValue(policy.TypeGoogleLogin)))
			if row.args[1] == "none" {
				local = awstest.NewLocal(policytest.TypeSecret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, policytest.TypeValue(policy.TypeGoogleLogin)))
			}
			startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Served: true, Type: row.served} })
			useAWS(t, local, testAccount, adaSignIn)
			stdout, stderr, code := runSecret(append([]string{"retag", "LOGIN_KEY"}, row.args...)...)
			if code != row.code {
				t.Fatalf("retag %v with the broker serving type %q: exit %d, stdout %q, stderr %q; want %d", row.args, row.served, code, stdout, stderr, row.code)
			}
			stale := strings.Contains(stderr, "it still sees the old tags") && strings.Contains(stderr, "Run this retag again") && strings.Contains(stderr, "store no value in LOGIN_KEY")
			if stale != (row.code == 1) {
				t.Fatalf("stderr %q; want the stale-tags message exactly when the type is stale", stderr)
			}
			if row.refusal != "" && !strings.Contains(stderr, row.refusal) {
				t.Fatalf("stderr %q does not say %q: the refusal has to name the type the broker serves, which is what tells an operator whether to run the retag again", stderr, row.refusal)
			}
			if row.served != "" && (!strings.Contains(stdout, "broker: serving LOGIN_KEY from ") || !strings.Contains(stdout, "(type "+row.served+")")) {
				t.Fatalf("stdout %q does not name the served type and the store it is in", stdout)
			}
		})
	}
}
