// packages/envoy/cmd/cipher/settings_words_test.go
//
// What the secret forms print of the broker's settings past the checks that read them: the account
// a sign-in is refused for, and the whole name, the id and the project of the secret a GCP create
// made. The settings are text whoever answered AGENT_SECRETS_URL wrote, and beside an address that
// carries a credential that text can repeat it, in a shape the checks admit: twelve digits for an
// account (checkSettingsKeyARN), letters, digits, hyphens and underscores for a secret-id prefix
// (checkSettingsKMSKeyName). So each prints as the broker's address lets its answer print
// (Settings.words): beside such an address, as cipheraddr.WordsNotPrinted.
package main

import (
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// useCredentialedBroker makes AGENT_SECRETS_URL the stand-in broker's address with secret as the
// password of its user information: an address that is not clean, whose every request carries it.
func useCredentialedBroker(t *testing.T, secret string) {
	t.Helper()
	address, err := url.Parse(brokerURLFromEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	address.User = url.UserPassword("demouser7q", secret)
	t.Setenv("AGENT_SECRETS_URL", address.String())
}

// TestTheBrokersAccountPrintsOnlyAsItsAddressLetsIt: a sign-in in another account than the
// broker's settings name is refused, naming that account; beside an AGENT_SECRETS_URL whose
// address's password is twelve digits, an answer that names that password as its account (and its
// key ARN's) passes checkSettingsKeyARN, and the refusal prints no run of it: the account prints as
// cipheraddr.WordsNotPrinted. Control: beside a clean address the refusal names the account.
// Captures agent-secrets.answer-words/settings-account.
func TestTheBrokersAccountPrintsOnlyAsItsAddressLetsIt(t *testing.T) {
	const echoed = "731942586015"
	settings := Settings{SecretsPrefix: policytest.Prefix, KMSKeyARN: strings.Replace(policytest.KeyARN, testAccount, echoed, 1),
		AWSAccountID: echoed, AWSRegion: "us-east-1"}
	for _, clean := range []bool{false, true} {
		name := "beside a credentialed address"
		if clean {
			name = "control: beside a clean address"
		}
		t.Run(name, func(t *testing.T) {
			startSecretBrokerWith(t, settings, func(string) rereadReply { return rereadReply{} })
			if !clean {
				useCredentialedBroker(t, echoed)
			}
			useAWS(t, secrets.NewLocal(), testAccount, adaSignIn)
			stdout, stderr, code := runSecret("list")
			if code == 0 {
				t.Fatalf("exit 0, stderr %q; want the sign-in in account %s refused", stderr, testAccount)
			}
			out := stdout + stderr
			if clean {
				if place := "but the agent secrets are in account " + echoed; !strings.Contains(out, place) {
					t.Fatalf("the output %q does not name the broker's account (%q), so the row does not see it", out, place)
				}
				return
			}
			requireNoneEchoed(t, "settings-account", out, []string{echoed},
				"your AWS sign-in "+adaSignIn+" is in account "+testAccount+", but the agent secrets are in account "+cipheraddr.WordsNotPrinted)
		})
	}
}

// TestAGCPCreatesSecretPrintsOnlyAsTheBrokersAddressLetsIt: beside an AGENT_SECRETS_URL that
// carries a password, an answer whose secrets_prefix names that password passes
// checkSettingsKMSKeyName, and the secret a create makes is named under it. A create the store
// leaves an empty secret behind - its delete refused, or its own answer lost and the read back
// refused - says what it left and the gcloud commands that settle it, and prints no run of the
// password: the whole name, and the id and the project in each command, print as
// cipheraddr.WordsNotPrinted. Control: beside a clean address each prints as the broker wrote it.
// Captures agent-secrets.answer-words/gcp-create-<row>.
func TestAGCPCreatesSecretPrintsOnlyAsTheBrokersAddressLetsIt(t *testing.T) {
	const echoed = "demopass9zQx7k2Lw"
	settings := gcpTestSettings()
	settings.SecretsPrefix = "projects/" + testGCPProject + "/secrets/" + echoed + "-"
	whole := settings.SecretsPrefix + "new-test"
	for _, row := range []struct {
		capture       string
		refuse, after map[string]codes.Code
		says          string // what the line says, the whole name after it
	}{
		{"gcp-create-undeleted", map[string]codes.Code{"AddSecretVersion": codes.PermissionDenied, "DeleteSecret": codes.PermissionDenied}, nil,
			"an empty secret named "},
		{"gcp-create-lost", map[string]codes.Code{"GetSecret": codes.PermissionDenied}, map[string]codes.Code{"CreateSecret": codes.Unavailable},
			"the store's answer leaves open whether it made "},
	} {
		for _, clean := range []bool{false, true} {
			name := row.capture + " beside a credentialed address"
			if clean {
				name = "control: " + row.capture + " beside a clean address"
			}
			t.Run(name, func(t *testing.T) {
				fake := newFakeSecretManager()
				fake.refuseMethods, fake.answerAfter = row.refuse, row.after
				startFakeSecretManager(t, fake)
				useGCPIdentity(t, "authorized_user", "ada@example.com")
				startSecretBrokerWith(t, settings, gcpServedBy(fake, settings))
				if !clean {
					useCredentialedBroker(t, echoed)
				}
				useStdin(t, "shh")
				stdout, stderr, code := runSecret("create", "NEW_TEST", "--owner", "me", "--tier", "agent")
				if code != 1 {
					t.Fatalf("exit %d, stdout %q, stderr %q; want 1", code, stdout, stderr)
				}
				out := stdout + stderr
				if clean {
					for _, place := range []string{row.says + whole, "gcloud secrets delete " + echoed + "-new-test --project " + testGCPProject} {
						if !strings.Contains(out, place) {
							t.Fatalf("the output %q does not name %q, so the row does not see the settings' text", out, place)
						}
					}
					return
				}
				requireNoneEchoed(t, row.capture, out, []string{echoed}, row.says+cipheraddr.WordsNotPrinted)
			})
		}
	}
}
