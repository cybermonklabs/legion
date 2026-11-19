// packages/envoy/cmd/agent-secrets/secret_reread_test.go
//
// The reread a secret form sends after its write: signed by this process's broker identity where
// it has one, since the broker says what it now serves only to a session, and sent with no proof
// where it has none, which the broker takes all the same and answers 202, saying nothing more.
package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// TestASecretFormSignsItsRereadAsThisProcesssSession: a form run where this process has a broker
// identity, here a launcher's key directory, signs its reread with that session's proof, and
// reads the broker's whole answer: `broker: serving` and exit 0.
func TestASecretFormSignsItsRereadAsThisProcesssSession(t *testing.T) {
	local := secrets.NewLocal()
	broker := startSecretBroker(t, servedBy(local))
	useAWS(t, local, testAccount, adaSignIn)
	useStdin(t, "v1")
	stdout, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
	if code != 0 || !strings.Contains(stdout, "broker: serving NEW_KEY from ") {
		t.Fatalf("create with a key directory: exit %d, stdout %q, stderr %q; want 0 and the broker serving NEW_KEY", code, stdout, stderr)
	}
	if signers, unsigned := broker.rereadSigners(), broker.rereadUnsigned(); !slices.Equal(signers, []string{testEnrollmentID}) || len(unsigned) != 0 {
		t.Fatalf("the rereads were signed by %v, and %v went with no proof; want one, signed by %s, the key directory's session", signers, unsigned, testEnrollmentID)
	}
}

// TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned: a form run where this process has no
// broker identity at all still asks for the reread, with no proof, so its write is served at
// once. The broker answers that 202 and says nothing of what it serves, so the form says it took
// the reread and why the call went unsigned, and exits 0: the write stands and nothing
// contradicts it. A type secret's write reads its type tag back from the store, finds it, and
// exits 0 too, with the one line that says the broker's view of it was not checked and names the
// retag to run where the command has an identity.
func TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned(t *testing.T) {
	t.Run("a stored secret", func(t *testing.T) {
		local := secrets.NewLocal()
		broker := startSecretBroker(t, servedBy(local))
		withNoBrokerIdentity(t)
		useAWS(t, local, testAccount, adaSignIn)
		useStdin(t, "v1")
		stdout, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
		if code != 0 || !strings.Contains(stdout, "broker: rereading NEW_KEY from ") || strings.Contains(stdout, "broker: serving") ||
			!strings.Contains(stderr, "took the reread of NEW_KEY, and says what it serves only to a call a session signs; this call carried no proof (no broker identity for this process") {
			t.Fatalf("create with no broker identity: exit %d, stdout %q, stderr %q; want 0, the reread taken, and why it went with no proof", code, stdout, stderr)
		}
		if unsigned, signers := broker.rereadUnsigned(), broker.rereadSigners(); !slices.Equal(unsigned, []string{"NEW_KEY"}) || len(signers) != 0 {
			t.Fatalf("the rereads %v went with no proof and %v were signed; want NEW_KEY's alone, with no proof", unsigned, signers)
		}
	})
	t.Run("a type secret whose tag the store kept", func(t *testing.T) {
		local := secrets.NewLocal()
		broker := startSecretBroker(t, servedBy(local))
		withNoBrokerIdentity(t)
		useAWS(t, local, testAccount, adaSignIn)
		useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
		stdout, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin)
		unchecked := "LOGIN_KEY carries the type tag " + policy.TypeGoogleLogin + " in "
		if code != 0 || !strings.Contains(stdout, "broker: rereading LOGIN_KEY from ") || strings.Count(afterUsing(t, stderr, "secret create"), "\n") != 1 ||
			!strings.Contains(stderr, unchecked) || !strings.Contains(stderr, "was not checked: the reread carried no proof (no broker identity for this process") ||
			!strings.Contains(stderr, "secret retag LOGIN_KEY --type "+policy.TypeGoogleLogin) {
			t.Fatalf("a typed create with no broker identity: exit %d, stdout %q, stderr %q; want 0 and one line saying the broker's view was not checked and naming the retag that checks it", code, stdout, stderr)
		}
		if unsigned := broker.rereadUnsigned(); !slices.Equal(unsigned, []string{"LOGIN_KEY"}) {
			t.Fatalf("the rereads %v went with no proof; want LOGIN_KEY's", unsigned)
		}
	})
}

// plantingStore is the store the CLI writes to, which, once a create has made a secret, leaves
// the secret's type tag as plant leaves it: a store that did not keep the type the create wrote,
// as another writer's race or a tag policy that strips it would leave it.
type plantingStore struct {
	*secrets.Local
	plant func(t *testing.T, local *secrets.Local, id string)
	t     *testing.T
}

func (p plantingStore) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	out, err := p.Local.CreateSecret(ctx, in, o...)
	if err == nil {
		p.plant(p.t, p.Local, aws.ToString(in.Name))
	}
	return out, err
}

// TestATypedWriteReadsItsTypeTagBack: a typed create reads the secret's type tag back from the
// store under the sign-in it wrote with, and exits 1 when the store holds another type, or none,
// naming the secret, the type it wrote and the tag it found. The broker is asked to reread all the
// same, so its view follows the store; the read-back is what decides, with a broker identity or
// with none, since a type secret the store holds untyped is served as stored by any broker that
// reads it.
func TestATypedWriteReadsItsTypeTagBack(t *testing.T) {
	for _, row := range []struct {
		name, found string
		plant       func(t *testing.T, local *secrets.Local, id string)
	}{
		{"a wrong tag", policy.TypeGitHubApp, func(t *testing.T, local *secrets.Local, id string) {
			if _, err := local.TagResource(context.Background(), &secretsmanager.TagResourceInput{SecretId: aws.String(id),
				Tags: []smtypes.Tag{{Key: aws.String(policy.TagType), Value: aws.String(policy.TypeGitHubApp)}}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a missing tag", tagNone, func(t *testing.T, local *secrets.Local, id string) {
			if _, err := local.UntagResource(context.Background(), &secretsmanager.UntagResourceInput{SecretId: aws.String(id), TagKeys: []string{policy.TagType}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, identity := range []string{"a broker identity", "no broker identity"} {
			t.Run(row.name+" with "+identity, func(t *testing.T) {
				local := secrets.NewLocal()
				broker := startSecretBroker(t, servedBy(local))
				if identity == "no broker identity" {
					withNoBrokerIdentity(t)
				}
				useAWS(t, plantingStore{Local: local, plant: row.plant, t: t}, testAccount, adaSignIn)
				useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
				_, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin)
				want := "LOGIN_KEY's type tag there is " + row.found + ", not " + policy.TypeGoogleLogin
				if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "secret retag LOGIN_KEY --type "+policy.TypeGoogleLogin) {
					t.Fatalf("a typed create whose store holds %s: exit %d, stderr %q; want 1 saying %q and naming the retag", row.name, code, stderr, want)
				}
				if asked := append(broker.rereadUnsigned(), broker.rereadSigners()...); len(asked) != 1 {
					t.Fatalf("the broker was asked to reread %d times; want once, so its view follows the store", len(asked))
				}
			})
		}
	}
}
