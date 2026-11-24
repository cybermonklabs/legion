// packages/envoy/cmd/cipher/secret_signin_reread_test.go
//
// A person's sign-in is the identity a secret form's reread is usually signed with, and nothing
// renews its lease, so its enrollment has usually lapsed by the time the person writes a secret.

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/aws/awstest"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// TestATypedWriteOnALapsedSignInRegistersItAgainAndIsChecked: a person signed in on this machine
// writes a type secret after their sign-in's lease lapsed: a create, and the retag of a stored
// secret to a type. The broker answers the reread's proof 401 "proof invalid: not live", so the
// form registers the sign-in's session again, once, under the same box key, and sends the reread
// signed by the new enrollment. That reread is answered in full, so the write is checked:
// `broker: serving … (type …)` and exit 0, no line saying the broker's view was not checked, and
// no reread sent with no proof. The broker counts one registration more, and the sign-in's record
// names the new enrollment afterwards. Before, the reread took the lapsed enrollment, was refused,
// went again unsigned at a second token of both reread buckets, and the write was never checked.
func TestATypedWriteOnALapsedSignInRegistersItAgainAndIsChecked(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_signin_reread_test.go:40); it signals the test process or changes its signal handling (main.login, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	const person = "ada@example.com"
	for _, form := range []string{"create", "retag"} {
		t.Run(form, func(t *testing.T) {
			broker := newLoginBroker(t)
			issuer := loginIssuer(t, person)
			withNoBrokerIdentity(t)
			t.Setenv(cipherclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
			t.Setenv(cipherclient.BrokerURLVar, broker.server.URL)
			t.Setenv(cipherclient.SessionVar, "")
			forceMetadataMachine(t)
			if err := login(context.Background(), io.Discard, &approvingWriter{t: t, issuer: issuer}); err != nil {
				t.Fatalf("login: %v", err)
			}
			store := cipherclient.SignIns(os.Getenv)
			first, err := store.Choose("")
			if err != nil {
				t.Fatalf("the sign-in login kept: %v", err)
			}
			broker.lapse()

			local := awstest.NewLocal()
			broker.mu.Lock()
			broker.settings = map[string]any{"secrets_prefix": policytest.Prefix, "kms_key_arn": awstest.KeyARN,
				"aws_account_id": testAccount, "aws_region": "us-east-1", "secret_type_tag": true}
			broker.rereadAnswer = servedBy(local)
			registered := broker.enrollments
			broker.mu.Unlock()
			useAWS(t, local, testAccount, adaSignIn)
			args := typedWrite(t, local, form)

			stdout, stderr, code := runSecret(args...)
			if code != 0 || !strings.Contains(stdout, "broker: serving LOGIN_KEY from ") || !strings.Contains(stdout, "(type "+policy.TypeGoogleLogin+")") ||
				strings.Contains(stderr, "was not checked") {
				t.Fatalf("a typed %s on a lapsed sign-in: exit %d, stdout %q, stderr %q; want 0, the broker serving LOGIN_KEY as its type, and no line saying its view was not checked", form, code, stdout, stderr)
			}
			kept, err := store.Choose("")
			if err != nil {
				t.Fatalf("the sign-in after the reread: %v", err)
			}
			if kept.EnrollmentID == first.EnrollmentID || kept.EnrollmentID != broker.liveEnrollment() || kept.Thumbprint != first.Thumbprint {
				t.Fatalf("the sign-in names %s under key %s; want %s, the enrollment the broker now holds, under the same key %s",
					kept.EnrollmentID, kept.Thumbprint, broker.liveEnrollment(), first.Thumbprint)
			}
			broker.mu.Lock()
			registrations := broker.enrollments - registered
			broker.mu.Unlock()
			if registrations != 1 {
				t.Fatalf("the %s registered %d sessions; want one, the lapsed sign-in's again", form, registrations)
			}
			if got, want := broker.rereadEnrollments(), []string{first.EnrollmentID, kept.EnrollmentID}; !slices.Equal(got, want) {
				t.Fatalf("the rereads named %q; want %q: the lapsed enrollment refused, then the new one, and none with no proof", got, want)
			}
		})
	}
}
