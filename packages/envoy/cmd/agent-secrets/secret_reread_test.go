// packages/envoy/cmd/agent-secrets/secret_reread_test.go
//
// The reread a secret form sends after its write: signed by this process's broker identity where
// it has one, since the broker says what it now serves only to a session, and sent with no proof
// where it has none, which the broker takes all the same and answers 202, saying nothing more.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/sjawhar/envoy/internal/broker/helper"
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

// TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned: a form run where this process holds
// no broker identity at all still asks for the reread, with no proof, so the broker rereads the
// secret after answering. It answers that 202 and says nothing of what it serves, so the form says
// in one line that the broker's view was not checked, why, what its exit 0 then says, and the
// command that checks it, and exits 0: the write stands and nothing contradicts it. A type
// secret's write reads its type tag back from the store, finds it, and exits 0 too, the one line
// naming the retag to run where the command has an identity before any key is stored.
func TestASecretFormWithNoBrokerIdentitySendsItsRereadUnsigned(t *testing.T) {
	t.Run("a stored secret", func(t *testing.T) {
		local := secrets.NewLocal()
		broker := startSecretBroker(t, servedBy(local))
		withNoBrokerIdentity(t)
		useAWS(t, local, testAccount, adaSignIn)
		useStdin(t, "v1")
		stdout, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
		if code != 0 || !strings.Contains(stdout, "broker: rereading NEW_KEY from ") || strings.Contains(stdout, "broker: serving") ||
			strings.Count(afterUsing(t, stderr, "secret create"), "\n") != 1 ||
			!strings.Contains(stderr, "took the reread of NEW_KEY, but how it serves NEW_KEY was not checked ("+errNoHeldIdentity.Error()+"), so this exit 0 says only that the write stands") ||
			!strings.Contains(stderr, "agent-secrets sign --method POST") {
			t.Fatalf("create with no broker identity: exit %d, stdout %q, stderr %q; want 0 and one line saying the broker's view was not checked, why, and the signed reread that checks it", code, stdout, stderr)
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
		requireTypeUnchecked(t, "create", stdout, stderr, code, errNoHeldIdentity.Error())
		if unsigned := broker.rereadUnsigned(); !slices.Equal(unsigned, []string{"LOGIN_KEY"}) {
			t.Fatalf("the rereads %v went with no proof; want LOGIN_KEY's", unsigned)
		}
	})
}

// requireTypeUnchecked fails t unless a typed write of LOGIN_KEY as google-login, by form, exited 0
// on its type tag alone: the reread taken with no proof, and exactly one line after the sign-in's
// saying the broker's view was not checked, why (because), that exit 0 says only that the store
// holds the tag, and the retag to run where the command has an identity before any key is stored.
func requireTypeUnchecked(t *testing.T, form, stdout, stderr string, code int, because string) {
	t.Helper()
	if code != 0 || !strings.Contains(stdout, "broker: rereading LOGIN_KEY from ") || strings.Count(afterUsing(t, stderr, "secret "+form), "\n") != 1 ||
		!strings.Contains(stderr, "LOGIN_KEY carries the type tag "+policy.TypeGoogleLogin+" in ") ||
		!strings.Contains(stderr, "was not checked ("+because) ||
		!strings.Contains(stderr, "so this exit 0 says only that the store holds the tag") ||
		!strings.Contains(stderr, "Store no key or login in LOGIN_KEY until `agent-secrets secret retag LOGIN_KEY --type "+policy.TypeGoogleLogin+"`") {
		t.Fatalf("a typed %s whose broker view was not checked: exit %d, stdout %q, stderr %q; want 0 and the one line saying so, why (%q), what exit 0 says, and the retag to run", form, code, stdout, stderr, because)
	}
}

// plantingStore is the store the CLI writes to, which, once a form's own write has landed, leaves
// the secret's type tag as plant does: a store that did not keep the type the form wrote, as
// another writer's race or a tag policy that strips it would leave it. It plants after a create's
// CreateSecret, a set's PutSecretValue or a retag's TagResource, whichever the form writes with,
// and counts each plant, so a row that planted nothing fails rather than passing on nothing.
type plantingStore struct {
	*secrets.Local
	t       *testing.T
	found   string // the type tag to leave: a type, tagNone for none, or "" to plant nothing
	planted *int
}

func (p plantingStore) plant(id string) {
	if p.found == "" {
		return
	}
	ctx := context.Background()
	if p.found == tagNone {
		if _, err := p.Local.UntagResource(ctx, &secretsmanager.UntagResourceInput{SecretId: aws.String(id), TagKeys: []string{policy.TagType}}); err != nil {
			p.t.Fatal(err)
		}
	} else if _, err := p.Local.TagResource(ctx, &secretsmanager.TagResourceInput{SecretId: aws.String(id),
		Tags: []smtypes.Tag{{Key: aws.String(policy.TagType), Value: aws.String(p.found)}}}); err != nil {
		p.t.Fatal(err)
	}
	*p.planted++
}

func (p plantingStore) CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, o ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	out, err := p.Local.CreateSecret(ctx, in, o...)
	if err == nil {
		p.plant(aws.ToString(in.Name))
	}
	return out, err
}

func (p plantingStore) PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, o ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	out, err := p.Local.PutSecretValue(ctx, in, o...)
	if err == nil {
		p.plant(aws.ToString(in.SecretId))
	}
	return out, err
}

func (p plantingStore) TagResource(ctx context.Context, in *secretsmanager.TagResourceInput, o ...func(*secretsmanager.Options)) (*secretsmanager.TagResourceOutput, error) {
	out, err := p.Local.TagResource(ctx, in, o...)
	if err == nil {
		p.plant(aws.ToString(in.SecretId))
	}
	return out, err
}

// typedWrite sets local and the stdin up for form's typed write of LOGIN_KEY as google-login and
// answers the form's arguments: a create of a new secret, a set of a google-login secret's value,
// or a retag of a stored secret that holds a login to google-login.
func typedWrite(t *testing.T, local *secrets.Local, form string) []string {
	t.Helper()
	login := policytest.TypeValue(policy.TypeGoogleLogin)
	switch form {
	case "create":
		useStdin(t, login)
		return []string{"create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin}
	case "set":
		local.Put(secrets.LocalSecret{Name: policytest.ID("LOGIN_KEY"), KmsKeyID: policytest.KeyARN,
			Tags: policytest.TypeSecret("LOGIN_KEY", "ada@example.com", policy.TierAgent, policy.TypeGoogleLogin, "").Tags, Value: "old"})
		useStdin(t, login+"\n")
		return []string{"set", "LOGIN_KEY"}
	default:
		local.Put(secrets.LocalSecret{Name: policytest.ID("LOGIN_KEY"), KmsKeyID: policytest.KeyARN,
			Tags: policytest.Secret("LOGIN_KEY", "ada@example.com", policy.TierAgent, "").Tags, Value: login})
		return []string{"retag", "LOGIN_KEY", "--type", policy.TypeGoogleLogin}
	}
}

// TestATypedWriteReadsItsTypeTagBack: a typed create, set or retag reads the secret's type tag
// back from the store under the sign-in it wrote with, and exits 1 when the store holds another
// type, or none, naming the secret, the type it wrote and the tag it found. The broker is asked to
// reread once all the same, so its view follows the store. Where the store kept the tag, a form
// with a broker identity reads the broker serving the secret as its type and exits 0, and one with
// none exits 0 on the tag alone, with the one line that says the broker's view was not checked.
func TestATypedWriteReadsItsTypeTagBack(t *testing.T) {
	ran := 0
	for _, form := range []string{"create", "set", "retag"} {
		for _, found := range []string{policy.TypeAzureLogin, tagNone, ""} {
			for _, identity := range []string{"a broker identity", "no broker identity"} {
				kept := found
				if kept == "" {
					kept = "the right tag"
				}
				t.Run(form+" leaving "+kept+" with "+identity, func(t *testing.T) {
					ran++
					local := secrets.NewLocal()
					broker := startSecretBroker(t, servedBy(local))
					if identity == "no broker identity" {
						withNoBrokerIdentity(t)
					}
					planted := 0
					args := typedWrite(t, local, form)
					useAWS(t, plantingStore{Local: local, t: t, found: found, planted: &planted}, testAccount, adaSignIn)
					stdout, stderr, code := runSecret(args...)
					if found != "" && planted != 1 {
						t.Fatalf("planted %d times; want once, or the row tests nothing", planted)
					}
					if asked := len(broker.rereadUnsigned()) + len(broker.rereadSigners()); asked != 1 {
						t.Errorf("the broker was asked to reread %d times; want once, so its view follows the store", asked)
					}
					switch {
					case found != "":
						want := "LOGIN_KEY's type tag there is " + found + ", not " + policy.TypeGoogleLogin
						if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "secret retag LOGIN_KEY --type "+policy.TypeGoogleLogin) {
							t.Errorf("exit %d, stdout %q, stderr %q; want 1 saying %q and naming the retag", code, stdout, stderr, want)
						}
					case identity == "no broker identity":
						requireTypeUnchecked(t, form, stdout, stderr, code, errNoHeldIdentity.Error())
					default:
						if code != 0 || !strings.Contains(stdout, "broker: serving LOGIN_KEY from ") || strings.Contains(stderr, "was not checked") {
							t.Errorf("exit %d, stdout %q, stderr %q; want 0 and the broker serving LOGIN_KEY as its type", code, stdout, stderr)
						}
					}
				})
			}
		}
	}
	if ran != 18 {
		t.Fatalf("ran %d rows; want 18", ran)
	}
}

// signOnceHelper is an agent-secrets-helper that says the caller is in a session it enrolled, its
// answer to the first sign it is asked for (the identity probe), and then refuses every sign after
// it, as a helper still enrolling the session or one that no longer answers does.
func signOnceHelper(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for answered := 0; ; answered++ {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			answer := helper.Response{OK: true, Proof: "eyJ.probe.proof"}
			if answered > 0 {
				answer = helper.Response{Code: helper.CodeNotEnrolled, Error: "the session is still enrolling"}
			}
			data, _ := json.Marshal(answer)
			_, _ = conn.Write(append(data, '\n'))
			conn.Close()
		}
	}()
	return sock
}

// TestATypedWriteExitsOnItsTagWhenTheBrokersViewCannotBeChecked: wherever a typed write cannot
// have the broker's view checked, it still sends the reread, with no proof, and exits on the type
// tag the store holds: 0 with the one line saying the broker's view was not checked and why, or 1
// on another tag. The cases are a process with no broker identity, an identity that cannot sign
// (a helper that refuses the sign), a broker that cannot check the proof (503), and one that does
// not accept it (401). Before, each but the first exited 1 with no reread sent at all.
func TestATypedWriteExitsOnItsTagWhenTheBrokersViewCannotBeChecked(t *testing.T) {
	for _, row := range []struct {
		name    string
		setup   func(t *testing.T, broker *secretBroker)
		because string
	}{
		{"no broker identity", func(t *testing.T, _ *secretBroker) { withNoBrokerIdentity(t) }, errNoHeldIdentity.Error()},
		{"an identity that cannot sign", func(t *testing.T, _ *secretBroker) {
			withNoBrokerIdentity(t)
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", signOnceHelper(t))
		}, "the signed reread failed: sign proof: "},
		{"a broker that cannot check the proof", func(t *testing.T, broker *secretBroker) {
			broker.failSignedRereads(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE")
		}, "the signed reread failed: "},
		{"a broker that does not accept the proof", func(t *testing.T, broker *secretBroker) {
			broker.failSignedRereads(http.StatusUnauthorized, "PROOF_INVALID")
		}, "the signed reread failed: "},
	} {
		for _, found := range []string{"", policy.TypeAzureLogin} {
			name := row.name + ", the right tag"
			if found != "" {
				name = row.name + ", another tag"
			}
			t.Run(name, func(t *testing.T) {
				local := secrets.NewLocal()
				broker := startSecretBroker(t, servedBy(local))
				row.setup(t, broker)
				planted := 0
				args := typedWrite(t, local, "create")
				useAWS(t, plantingStore{Local: local, t: t, found: found, planted: &planted}, testAccount, adaSignIn)
				stdout, stderr, code := runSecret(args...)
				if unsigned := broker.rereadUnsigned(); !slices.Equal(unsigned, []string{"LOGIN_KEY"}) {
					t.Fatalf("the rereads that went with no proof were %v; want LOGIN_KEY's, so the write is served at once all the same (stderr %q)", unsigned, stderr)
				}
				if found == "" {
					requireTypeUnchecked(t, "create", stdout, stderr, code, row.because)
					return
				}
				if want := "LOGIN_KEY's type tag there is " + found; code != 1 || !strings.Contains(stderr, want) || strings.Contains(stderr, "was not checked") {
					t.Fatalf("exit %d, stderr %q; want 1 saying %q and no line about the broker's view", code, stderr, want)
				}
			})
		}
	}
}

// TestASecretWriteWithNoIdentityTradesNothingAndWaitsOnNothing: a form's reread looks only for an
// identity the process already holds. A key directory whose launcher is still enrolling (a fresh
// enrollment.pending and no key.pem) is not waited on, and a machine whose cloud identity the
// broker would trade for a session is traded for nothing: the broker is asked for its settings and
// the reread and nothing else, the metadata server is asked nothing, and the form is done well
// inside the enrollment wait it would otherwise have sat through.
func TestASecretWriteWithNoIdentityTradesNothingAndWaitsOnNothing(t *testing.T) {
	const enrollWait = 30 * time.Second
	for _, row := range []string{"a launcher still enrolling", "a machine with no session yet"} {
		t.Run(row, func(t *testing.T) {
			local := secrets.NewLocal()
			startSecretBrokerWith(t, Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: policytest.KeyARN, AWSAccountID: testAccount,
				AWSRegion: "us-east-1", SecretTypeTag: true, GoogleExchangeAudience: "agent-secrets-vm"}, servedBy(local))
			withNoBrokerIdentity(t)
			metadata := startMetadataServer(t, "machine@example.iam.gserviceaccount.com")
			if row == "a launcher still enrolling" {
				if err := os.WriteFile(filepath.Join(os.Getenv("AGENT_SECRETS_KEY_DIR"), pendingMarker), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGENT_SECRETS_ENROLL_WAIT", enrollWait.String())
			}
			calls := recordBrokerCalls(t)
			useAWS(t, local, testAccount, adaSignIn)
			useStdin(t, "v1")
			started := time.Now()
			_, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
			took := time.Since(started)
			if code != 0 {
				t.Fatalf("create: exit %d, stderr %q; want 0", code, stderr)
			}
			if want := []string{"GET /v1/settings", "POST /v1/secrets/NEW_KEY/reread"}; !slices.Equal(calls(), want) {
				t.Fatalf("the broker was asked %v; want %v and nothing else: no token trade, no enrollment", calls(), want)
			}
			if n := metadata.totalRequests(); n != 0 {
				t.Fatalf("the metadata server was asked %d times; want never", n)
			}
			if took >= enrollWait/3 {
				t.Fatalf("the create took %s; want it done well inside the %s enrollment wait it must not sit through", took, enrollWait)
			}
		})
	}
}

// TestATypedWriteReadsItsTypeTagBackWhenTheBrokerFailsItsReread: the read-back does not wait on
// the broker. A typed create whose reread the broker could not be asked for, or whose signed
// reread the broker answers with the secret refused, still reads the secret's type tag back from
// the store, names a tag the store did not keep beside the broker's own line, and exits 1: the
// broker's answer says nothing of what the store holds, which the broker serves from its next read.
func TestATypedWriteReadsItsTypeTagBackWhenTheBrokerFailsItsReread(t *testing.T) {
	for _, row := range []struct {
		name   string
		broker func(t *testing.T, local *secrets.Local)
		says   string
	}{
		{"a reread the broker could not be asked for", func(t *testing.T, local *secrets.Local) {
			startSecretBroker(t, servedBy(local)).failRereads(http.StatusServiceUnavailable)
		}, "could not be asked to reread LOGIN_KEY"},
		{"a reread the broker answers with the secret refused", func(t *testing.T, _ *secrets.Local) {
			startSecretBroker(t, func(name string) Reread { return Reread{Name: name, Reason: policy.ReasonOwnerTagMissing} })
		}, "refuses LOGIN_KEY after the write"},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal()
			row.broker(t, local)
			planted := 0
			useAWS(t, plantingStore{Local: local, t: t, found: policy.TypeGitHubApp, planted: &planted}, testAccount, adaSignIn)
			useStdin(t, policytest.TypeValue(policy.TypeGoogleLogin))
			_, stderr, code := runSecret("create", "LOGIN_KEY", "--owner", "me", "--tier", "agent", "--type", policy.TypeGoogleLogin)
			want := "LOGIN_KEY's type tag there is " + policy.TypeGitHubApp + ", not " + policy.TypeGoogleLogin
			if code != 1 || !strings.Contains(stderr, row.says) || !strings.Contains(stderr, want) {
				t.Fatalf("a typed create after %s, whose store holds %s: exit %d, stderr %q; want 1, the broker's line %q and the store's %q",
					row.name, policy.TypeGitHubApp, code, stderr, row.says, want)
			}
		})
	}
}
