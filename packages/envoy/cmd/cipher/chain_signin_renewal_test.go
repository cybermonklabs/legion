// packages/envoy/cmd/cipher/chain_signin_renewal_test.go
//
// A person's sign-in that this machine cannot renew, and that holds no live enrollment, has
// nothing for the caller: the chain goes on to the helper, else the machine, and says why on
// stderr. A sign-in whose enrollment the broker still holds, or that this machine can renew, is
// used as before, and `renew` never acts as another session in its place. The command tests run
// the built binary in a rig of its own (chainRig) against chainBroker, as chain_test.go does.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// helperEnrollment is the enrollment the fake helper's proofs name, so a broker records which
// source signed each request.
const helperEnrollment = "helper-enrollment"

// helperProof is the proof the fake helper signs every call with, naming helperEnrollment.
var helperProof = "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"eid":"`+helperEnrollment+`"}`)) + ".c2ln"

// liveHelperSession is a helper that says the caller is in one of its sessions: it signs every
// proof as helperProof and every request object, and answers no other op.
func liveHelperSession(t *testing.T) string {
	t.Helper()
	sock, _ := fakeHelperByOp(t, map[string]helper.Response{
		"sign":         {OK: true, Proof: helperProof},
		"sign-request": {OK: true, RequestObject: "helper-request-object"},
	})
	return sock
}

// signInRig is a rig whose broker answers a lapsed enrollment as the production broker did on
// 2026-11-22 (`PROOF_INVALID` "proof invalid: not live") and holds the rig's one sign-in,
// ada@example.com's enrollment signin-enrollment, to its box key.
func signInRig(t *testing.T, binary string) (*chainRig, *chainBroker) {
	t.Helper()
	broker := newChainBroker(t, storeSettings("gcp", "agent-secrets-vm"))
	broker.answerNotLiveAsProofInvalid()
	r := newChainRig(t, binary, broker)
	broker.pinKey("signin-enrollment", writeSignInFixture(t, r, "ada@example.com"))
	return r, broker
}

// withTheSignInsOwnClient gives the rig the device-flow client the fixture signed in with, which
// renews it.
func withTheSignInsOwnClient(t *testing.T, r *chainRig) {
	t.Helper()
	r.env["AGENT_SECRETS_SIGNIN_CLIENT_ID"] = "device-client"
	r.env["AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"] = writeLoginFile(t, "client-secret", "not-a-secret")
}

// TestASignInThisMachineCannotRenewGivesWay: on a machine told of no sign-in client (or of
// another client than the person signed in with), a sign-in whose enrollment the broker answers
// not live is passed over. The use command takes the helper where the caller is in one of its
// sessions, and the machine only where it is not, and one stderr line says why and which source
// it took. Only a sign-in this machine cannot renew is checked, by one read signed with its own
// box key; a live one, and one this machine can renew, serves as before.
func TestASignInThisMachineCannotRenewGivesWay(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process ((*chainRig).withMetadata, main.startMetadataServer, t.Setenv at secret_gcp_order_test.go:136).
	binary := chainBinary(t)
	rows := []struct {
		name string
		// setup prepares the rig beyond the sign-in and answers the enrollment the request
		// must be signed by and the source the notice must name ("" for no notice).
		setup             func(t *testing.T, r *chainRig, broker *chainBroker) (signedBy, source string)
		wantMachineTrades int
		wantSelfReads     int
	}{
		{name: "lapsed, no client, a helper session and a machine identity", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			broker.endEnrollment("signin-enrollment")
			r.withMetadata()
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return helperEnrollment, "helper"
		}, wantSelfReads: 1},
		{name: "lapsed, another client, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			broker.endEnrollment("signin-enrollment")
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_ID"] = "another-device-client"
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"] = writeLoginFile(t, "client-secret", "not-a-secret")
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return helperEnrollment, "helper"
		}, wantSelfReads: 1},
		{name: "lapsed, no client, no helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			broker.endEnrollment("signin-enrollment")
			r.withMetadata()
			return "machine-enrollment-1", "machine"
		}, wantMachineTrades: 1, wantSelfReads: 1},
		{name: "no enrollment yet, no client, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			rewriteSignInFixture(t, r, func(record map[string]any) { delete(record, "enrollment_id") })
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return helperEnrollment, "helper"
		}},
		{name: "live, no client, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return "signin-enrollment", ""
		}, wantSelfReads: 1},
		{name: "live, the sign-in's own client, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			withTheSignInsOwnClient(t, r)
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return "signin-enrollment", ""
		}},
		{name: "live, one of the two client settings, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_ID"] = "device-client"
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return "signin-enrollment", ""
		}},
		{name: "live, a client secret file that cannot be read, a helper session", setup: func(t *testing.T, r *chainRig, broker *chainBroker) (string, string) {
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_ID"] = "device-client"
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE"] = filepath.Join(t.TempDir(), "absent-client-secret")
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			return "signin-enrollment", ""
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r, broker := signInRig(t, binary)
			signedBy, source := row.setup(t, r, broker)

			stderr := r.mustUse()
			if got := broker.requestsSignedBy(); !slices.Equal(got, []string{signedBy}) {
				t.Fatalf("POST /v1/requests was signed by %q; want %q alone (stderr %q)", got, signedBy, stderr)
			}
			if got := len(machineExchanges(broker)); got != row.wantMachineTrades {
				t.Fatalf("the machine traded its identity %d times; want %d (stderr %q)", got, row.wantMachineTrades, stderr)
			}
			if got := broker.selfReadCount(); got != row.wantSelfReads {
				t.Fatalf("the broker answered %d reads of the sign-in's enrollment; want %d (stderr %q)", got, row.wantSelfReads, stderr)
			}
			ending := ""
			if source != "" {
				ending = "; using the " + source + " source"
			}
			assertPassedOver(t, stderr, ending)
		})
	}
}

// TestIdentitySourceNamesTheSourceAfterASignInThisMachineCannotRenew: `identity --source` takes
// the source the use command takes, and writes the same line.
func TestIdentitySourceNamesTheSourceAfterASignInThisMachineCannotRenew(t *testing.T) {
	t.Parallel()
	binary := chainBinary(t)
	r, broker := signInRig(t, binary)
	broker.endEnrollment("signin-enrollment")
	sock := liveHelperSession(t)
	r.env["AGENT_SECRETS_HELPER_SOCK"] = sock

	code, stdout, stderr := r.run("identity", "--source")
	if want := "helper: " + sock + fmt.Sprintf(" (a helper from before this release; its binary is /proc/%d/exe)\n", os.Getpid()); code != 0 || stdout != want {
		t.Fatalf("identity --source: exit %d, stdout %q, stderr %q; want exit 0 and stdout %q", code, stdout, stderr, want)
	}
	assertPassedOver(t, stderr, "; using the helper source")
	if broker.enrolled() != 0 || len(broker.exchanged()) != 0 {
		t.Fatalf("identity --source made %d enrollments and %d exchanges; it registers nothing", broker.enrolled(), len(broker.exchanged()))
	}
}

// TestAPassedOverSignInIsNamedWhenTheNextSourceFails: where the source tried after a passed-over
// sign-in stops the command (a stopped helper, or a broker that refuses the machine's exchange),
// the line still says why the sign-in was passed over, and which source was tried next, before
// that source's own refusal: the person learns that their sign-in is the cause.
func TestAPassedOverSignInIsNamedWhenTheNextSourceFails(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process ((*chainRig).withMetadata, main.startMetadataServer, t.Setenv at secret_gcp_order_test.go:136).
	binary := chainBinary(t)
	for _, row := range []struct {
		name, source, want string
		setup              func(t *testing.T, r *chainRig, broker *chainBroker)
	}{
		{name: "a stopped helper", source: "helper", want: "the helper is stopped", setup: func(t *testing.T, r *chainRig, _ *chainBroker) {
			r.env["AGENT_SECRETS_HELPER_SOCK"] = filepath.Join(shortTempDir(t), "absent.sock")
		}},
		{name: "a refused machine exchange", source: "machine", want: "no exchange entry lists this machine", setup: func(_ *testing.T, r *chainRig, broker *chainBroker) {
			r.withMetadata()
			broker.refuseExchanges()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, broker := signInRig(t, binary)
			broker.endEnrollment("signin-enrollment")
			row.setup(t, r, broker)

			code, stdout, stderr := r.use()
			if code != 1 || stdout != "" || !strings.Contains(stderr, row.want) {
				t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want exit 1 and stderr saying %q", code, stdout, stderr, row.want)
			}
			notice := assertPassedOver(t, stderr, "; tried the "+row.source+" source next")
			if refusal := strings.Index(stderr, row.want); refusal < notice {
				t.Fatalf("stderr %q; want the notice before the %s source's refusal", stderr, row.source)
			}
			if got := broker.requestsSignedBy(); len(got) != 0 {
				t.Fatalf("POST /v1/requests was signed by %q; want no request", got)
			}
		})
	}
}

// TestASignInIsPassedOverOnlyWhenItsEnrollmentIsNotLive: the pass-over is for the one case. A
// sign-in client this machine cannot read is a broken setting, not a missing one, and a refusal of
// the enrollment read other than not live is the broker's, named as that read; each stops the
// command as before, and no later source signs. The second row is also the check that the read is
// signed with the sign-in's own box key: the broker holds the enrollment to another key there.
func TestASignInIsPassedOverOnlyWhenItsEnrollmentIsNotLive(t *testing.T) {
	t.Parallel()
	binary := chainBinary(t)
	rows := []struct {
		name  string
		setup func(t *testing.T, r *chainRig, broker *chainBroker)
		want  []string // what stderr must say
	}{
		{name: "one of the two client settings", setup: func(t *testing.T, r *chainRig, broker *chainBroker) {
			broker.endEnrollment("signin-enrollment")
			r.env["AGENT_SECRETS_SIGNIN_CLIENT_ID"] = "device-client"
		}, want: []string{"needs both"}},
		{name: "the enrollment read refused otherwise", setup: func(t *testing.T, r *chainRig, broker *chainBroker) {
			broker.pinKey("signin-enrollment", "a-key-the-sign-in-does-not-hold")
		}, want: []string{
			"ask the broker AGENT_SECRETS_URL names whether ada@example.com's sign-in holds a live enrollment",
			"GET /v1/enrollments/self", "key is not the enrolled key",
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r, broker := signInRig(t, binary)
			r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)
			row.setup(t, r, broker)

			code, stdout, stderr := r.use()
			if code != 1 || stdout != "" {
				t.Fatalf("the use command: exit %d, stdout %q, stderr %q; want exit 1", code, stdout, stderr)
			}
			for _, want := range row.want {
				if !strings.Contains(stderr, want) {
					t.Fatalf("stderr %q; want it to say %q", stderr, want)
				}
			}
			if got := broker.requestsSignedBy(); slices.Contains(got, helperEnrollment) {
				t.Fatalf("POST /v1/requests was signed by %q; the helper must not sign after a broken sign-in", got)
			}
			assertPassedOver(t, stderr, "")
		})
	}
}

// TestRenewReportsASignInThisMachineCannotRenewAndActsAsNoOtherSession: `renew`'s contract is to
// report the enrollment the broker no longer holds (exit 76), so its chain never passes over the
// sign-in: beside a live helper session it renews the sign-in's own enrollment, is refused for
// good, and registers nothing.
func TestRenewReportsASignInThisMachineCannotRenewAndActsAsNoOtherSession(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process ((*chainRig).withMetadata, main.startMetadataServer, t.Setenv at secret_gcp_order_test.go:136).
	binary := chainBinary(t)
	r, broker := signInRig(t, binary)
	broker.endEnrollment("signin-enrollment")
	r.withMetadata()
	r.env["AGENT_SECRETS_HELPER_SOCK"] = liveHelperSession(t)

	cmd := r.command("renew")
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := time.AfterFunc(time.Minute, func() { _ = cmd.Process.Kill() })
	defer stop.Stop()
	_ = cmd.Wait()
	code, stderr := cmd.ProcessState.ExitCode(), errOut.String()
	if code != exitRenewRefused || !strings.Contains(stderr, "enrollment signin-enrollment permanently refused") {
		t.Fatalf("renew: exit %d, stdout %q, stderr %q; want exit %d and the sign-in's enrollment refused for good", code, out.String(), stderr, exitRenewRefused)
	}
	assertPassedOver(t, stderr, "")
	if broker.enrolled() != 0 || len(broker.exchanged()) != 0 || broker.selfReadCount() != 0 {
		t.Fatalf("renew made %d enrollments, %d exchanges and %d enrollment reads; it renews the sign-in's own and nothing else", broker.enrolled(), len(broker.exchanged()), broker.selfReadCount())
	}
}

// TestASecretFormsRereadPassesOverASignInThisMachineCannotRenew: a secret form's reread takes its
// held signer from the same chain, so beside a dead sign-in this machine cannot renew it signs
// with the helper's session, else with the machine session the cache holds, writes no line about
// it, and registers nothing.
func TestASecretFormsRereadPassesOverASignInThisMachineCannotRenew(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at chain_signin_renewal_test.go:297); it sets a package variable the package's other tests read (main.useAWS, awsClients set at secret_test.go:375).
	for _, row := range []struct {
		name     string
		signedBy string
		setup    func(t *testing.T, state string)
	}{
		{name: "a helper session", signedBy: helperEnrollment, setup: func(t *testing.T, _ string) {
			t.Setenv("AGENT_SECRETS_HELPER_SOCK", liveHelperSession(t))
		}},
		{name: "a cached machine session", signedBy: "cached-enrollment", setup: func(t *testing.T, state string) {
			writeSessionFiles(t, filepath.Join(state, "agent-secrets", "machine"), "cached-enrollment")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			local := secrets.NewLocal()
			broker := startSecretBroker(t, servedBy(local))
			withNoBrokerIdentity(t)
			broker.endEnrollment("signin-enrollment")
			writeSignInFixture(t, &chainRig{t: t, home: os.Getenv("HOME"), state: os.Getenv("XDG_STATE_HOME")}, "ada@example.com")
			t.Setenv("AGENT_SECRETS_SIGNIN_CLIENT_ID", "")
			t.Setenv("AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE", "")
			row.setup(t, os.Getenv("XDG_STATE_HOME"))
			useAWS(t, local, testAccount, adaSignIn)
			useStdin(t, "v1")

			stdout, stderr, code := runSecret("create", "NEW_KEY", "--owner", "me", "--tier", "agent")
			if code != 0 || !strings.Contains(stdout, "broker: serving NEW_KEY from ") {
				t.Fatalf("create beside a dead sign-in: exit %d, stdout %q, stderr %q; want 0 and the broker serving NEW_KEY", code, stdout, stderr)
			}
			if signers, unsigned := broker.rereadSigners(), broker.rereadUnsigned(); !slices.Equal(signers, []string{row.signedBy}) || len(unsigned) != 0 {
				t.Fatalf("the rereads were signed by %v, and %v went with no proof; want one, signed by %s", signers, unsigned, row.signedBy)
			}
			if strings.Contains(stderr, "; using the ") || strings.Contains(stderr, "; tried the ") {
				t.Fatalf("stderr %q; the reread writes no line about the sign-in it passed over", stderr)
			}
		})
	}
}

// assertPassedOver fails t unless stderr holds exactly one line saying the sign-in was passed
// over and why, ending exactly ending ("; using the helper source", "; tried the machine source
// next"); with ending "" it fails t if stderr holds any such line. No line may carry the broker's
// refusal of the dead enrollment as a failure of the command. It answers the line's byte offset
// in stderr, so a caller can check what comes after it.
func assertPassedOver(t *testing.T, stderr, ending string) int {
	t.Helper()
	var notices []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "sign-in holds no live enrollment") {
			notices = append(notices, line)
		}
	}
	if ending == "" {
		if len(notices) != 0 {
			t.Fatalf("stderr %q passes over a sign-in it should not", stderr)
		}
		return -1
	}
	if len(notices) != 1 {
		t.Fatalf("stderr %q; want one line that passes over the sign-in", stderr)
	}
	line := notices[0]
	if strings.Count(line, "ada@example.com") != 1 {
		t.Fatalf("the notice %q names the person %d times; want once", line, strings.Count(line, "ada@example.com"))
	}
	for _, want := range []string{"agent-secrets: ", "this machine cannot renew it: "} {
		if !strings.Contains(line, want) {
			t.Fatalf("the notice %q does not say %q", line, want)
		}
	}
	if !strings.HasSuffix(line, ending) {
		t.Fatalf("the notice %q does not end %q", line, ending)
	}
	if strings.Contains(stderr, "registering this session again") {
		t.Fatalf("stderr %q; the sign-in was registered again instead of passed over", stderr)
	}
	return strings.Index(stderr, line)
}

// rewriteSignInFixture changes the rig's one sign-in record with edit, as a JSON object, keeping
// the file's mode.
func rewriteSignInFixture(t *testing.T, r *chainRig, edit func(record map[string]any)) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(r.state, "agent-secrets", "signins", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("the rig's sign-in records: %v, %v; want one", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	edit(record)
	if data, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
}
