//go:build unix

// packages/envoy/cmd/cipher/pod_enroll_test.go
//
// `enroll --pod` against a projected token file and a fake broker: what the pod reads, what it
// sends the broker, what it writes, and what it says when the file, the token or the broker
// refuses it. The broker here is servePodBroker (pod_broker_test.go), which records the token
// login, the enrollment and the renewal this mode makes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api/unreadablekeysettest"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestEnrolAPodFromItsProjectedToken: the whole of a pod's enrollment. It logs in with the token
// its projected file holds, enrols kind pod under the pod UID that token's own claims name,
// carrying the token again and a launcher proof, writes key.pem and enrollment into --out, prints
// the enrollment id alone on stdout and names the renewer on stderr. Run again on the same
// directory it reuses key.pem, so the enrollment the broker holds live for that key is the one it
// gets back, and it renews that lease.
func TestEnrolAPodFromItsProjectedToken(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:46).
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker, calls := servePodBroker(t, "enrollment-of-the-pod")
	out := t.TempDir()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", out)

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod: exit %d, stderr %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-pod" {
		t.Fatalf("stdout = %q, want the enrollment id alone", stdout.String())
	}
	// The key directory is part of the command because renew reads AGENT_SECRETS_KEY_DIR and
	// never --out. Only the shape of the line is read here, never the paths in it: both are
	// shell-quoted, so a temporary directory holding a space or an apostrophe is correctly
	// printed unlike itself. pod_renew_command_test.go runs the printed line from exactly such
	// a directory, which is what proves it names the right one.
	for _, want := range []string{"keep this lease alive with: AGENT_SECRETS_KEY_DIR=", "agent-secrets renew --pod-token-file "} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name the next command's %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the pod's token reached stderr")
	}
	got := calls()
	if len(got) != 2 || got[0].path != "/v1/launcher-credentials/token" || got[1].path != "/v1/enrollments" {
		t.Fatalf("broker calls = %+v, want the token login then the enrollment", got)
	}
	if got[0].body["token"] != token {
		t.Fatalf("token login sent %q, want the projected token", got[0].body["token"])
	}
	// The signed request object is what the real broker verifies and reads --service from, so it
	// is verified here rather than merely counted (record.VerifyRequestObject, as main_test.go's
	// fake broker does for a secret request).
	login, err := record.VerifyRequestObject(got[0].body["request"], broker.URL, time.Minute, time.Now())
	if err != nil {
		t.Fatalf("record.VerifyRequestObject(the pod's login request): %v", err)
	}
	if len(login.Details) != 1 || login.Details[0].Type != "launcher_credential" {
		t.Fatalf("login request details = %+v, want exactly one launcher_credential detail", login.Details)
	}
	if login.Details[0].Service != testPodService {
		t.Fatalf("login request service = %q, want --service's %q", login.Details[0].Service, testPodService)
	}
	if login.Details[0].Identifier != testPodUID {
		t.Fatalf("login request identifier = %q, want the pod UID %q", login.Details[0].Identifier, testPodUID)
	}
	enrol := got[1]
	if enrol.proof == "" {
		t.Fatal("the enrollment carried no Proof header")
	}
	if enrol.body["kind"] != "pod" {
		t.Fatalf("enrollment kind = %q, want pod", enrol.body["kind"])
	}
	if enrol.body["runtime_id"] != testPodUID {
		t.Fatalf("runtime_id = %q, want the pod UID the token names, %q", enrol.body["runtime_id"], testPodUID)
	}
	if enrol.body["pod_token"] != token {
		t.Fatalf("enrollment pod_token = %q, want the projected token", enrol.body["pod_token"])
	}
	// The thumbprint must be the key.pem this run wrote, and the enrollment must be signed by the
	// login's own key, not by the session key: a broker with the real verifier refuses either
	// mistake, so both are checked here with that verifier.
	sessionKey, err := loadKey(filepath.Join(out, keyFile), outKeyFile)
	if err != nil {
		t.Fatalf("read the session key the run wrote: %v", err)
	}
	sessionThumbprint, err := proof.Thumbprint(&sessionKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if enrol.body["thumbprint"] != sessionThumbprint {
		t.Fatalf("enrollment thumbprint = %q, want key.pem's %q", enrol.body["thumbprint"], sessionThumbprint)
	}
	verifier := &proof.Verifier{
		Skew: time.Minute,
		Lookup: func(_ context.Context, id string) (string, bool, error) {
			return sessionThumbprint, id == "enrollment-of-the-pod", nil
		},
		LookupLauncher: func(_ context.Context, id string) (string, bool, error) {
			return login.Thumbprint, id == "credential-for-the-pod", nil
		},
		Replay: func(context.Context, string, time.Time) (bool, error) { return true, nil },
	}
	subject, err := verifier.Verify(context.Background(), enrol.proof, http.MethodPost, broker.URL+"/v1/enrollments", time.Now())
	if err != nil {
		t.Fatalf("verify the enrollment's launcher proof: %v", err)
	}
	if subject.LauncherID != "credential-for-the-pod" {
		t.Fatalf("the enrollment's proof names launcher %q, want the credential the login issued", subject.LauncherID)
	}

	keyPath := filepath.Join(out, keyFile)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key.pem: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem mode = %v, want 0600", info.Mode().Perm())
	}
	if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != "enrollment-of-the-pod" {
		t.Fatalf("enrollment file = %q (%v), want the enrollment id", id, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("second enroll --pod: exit %d, stderr %q", code, stderr.String())
	}
	again, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyPEM, again) {
		t.Fatal("re-running enroll --pod wrote a new key.pem, so the live enrollment of the first key would be lost")
	}
	if got := strings.TrimSpace(stdout.String()); got != "enrollment-of-the-pod" {
		t.Fatalf("second run stdout = %q, want the same enrollment id alone", stdout.String())
	}
	after := calls()
	if len(after) != 5 {
		t.Fatalf("broker calls after the second run = %d, want 5 (login, enrollment, renewal)", len(after))
	}
	if tp := after[3].body["thumbprint"]; tp != enrol.body["thumbprint"] {
		t.Fatalf("second enrollment thumbprint = %q, want the first key's %q", tp, enrol.body["thumbprint"])
	}
	renew := after[4]
	if renew.path != "/v1/enrollments/enrollment-of-the-pod/renew" {
		t.Fatalf("the second run's last call was %q, want the renewal of the enrollment it already holds", renew.path)
	}
	if renew.body["pod_token"] != token {
		t.Fatalf("renewal pod_token = %q, want the projected token", renew.body["pod_token"])
	}
	// The renewal is the session key's own proof, naming the enrollment; the launcher key signs
	// nothing after the enrollment.
	renewal, err := verifier.Verify(context.Background(), renew.proof, http.MethodPost, broker.URL+"/v1/enrollments/enrollment-of-the-pod/renew", time.Now())
	if err != nil {
		t.Fatalf("verify the renewal's session proof: %v", err)
	}
	if renewal.EnrollmentID != "enrollment-of-the-pod" {
		t.Fatalf("the renewal's proof names enrollment %q, want the one it holds", renewal.EnrollmentID)
	}
}

// TestPodUsageRefusals: every way of naming the pod form wrongly is exit 2, and says what is
// missing or which two flags cannot be given together. With neither mode the refusal names both,
// so a pod that forgot --pod is told the mode exists.
func TestPodUsageRefusals(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:220).
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := servePodBroker(t, "never-reached")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"no mode at all", nil, nil, "--pod"},
		{"no mode at all names the helper", nil, nil, "--helper is required"},
		{"--pod with --helper", []string{"--pod", "--helper", "--service", testPodService}, nil, "mutually exclusive"},
		{"--pod with no service", []string{"--pod", "--pod-token-file", tokenFile}, nil, "--service"},
		{"--pod with no token file", []string{"--pod", "--service", testPodService, "--pod-token-file", ""}, nil, "--pod-token-file"},
		{"--pod with no key dir", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_KEY_DIR": ""}, "--out"},
		{"--pod with no broker URL", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile}, map[string]string{"AGENT_SECRETS_URL": ""}, brokerURLVar},
		// --helper's three, each refused by name rather than silently ignored, as --helper
		// itself is beside --pod.
		{"--pod with --runtime-id", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--runtime-id", "pod-1"}, nil, "--runtime-id is --helper's"},
		{"--pod with --thumbprint", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--thumbprint", "tp"}, nil, "--thumbprint is --helper's"},
		{"--pod with --session-id", []string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--session-id", "s-1"}, nil, "--session-id is --helper's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdEnroll(tc.args, &stdout, &stderr); code != exitUsageError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr %q does not name %q", stderr.String(), tc.want)
			}
		})
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none: a usage refusal calls nothing", n)
	}
}

// TestAPodTokenFileThatCannotBeReadIsNamed: a pod whose projected token file is missing or blank
// exits 1 naming the file by its flag, --pod-token-file, and not by its path, and asks the broker
// nothing — a blank file is never sent as a token.
func TestAPodTokenFileThatCannotBeReadIsNamed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:249).
	missing := filepath.Join(t.TempDir(), "absent")
	blank := writePodToken(t, "")
	for name, path := range map[string]string{"a missing file": missing, "an empty file": blank} {
		t.Run(name, func(t *testing.T) {
			broker, calls := servePodBroker(t, "never-reached")
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			var stdout, stderr bytes.Buffer
			code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", path, "--out", t.TempDir()}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--pod-token-file") || strings.Contains(stderr.String(), path) {
				t.Fatalf("stderr %q does not name the token file by --pod-token-file alone, without its path %q", stderr.String(), path)
			}
			if n := len(calls()); n != 0 {
				t.Fatalf("the broker was asked %d times, want none", n)
			}
		})
	}
}

// TestAPodTokenBoundToNoPodIsNamed: a token `kubectl create token` mints names no pod, so it
// enrols nothing — the broker lets a login enrol only the pod its own token is bound to. The
// refusal says so before anything is asked of the broker, rather than arriving as a 403 the
// operator has to decode.
func TestAPodTokenBoundToNoPodIsNamed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:273).
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, ""))
	broker, calls := servePodBroker(t, "never-reached")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{"bound to no pod", "kubernetes.io.pod.uid", "kubectl create token"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not say %q", stderr.String(), want)
		}
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none", n)
	}
}

// TestNotAJWTInThePodTokenFileIsNamed: a file holding something that is not a JWT at all is named
// as such, not sent to the broker to be refused there.
func TestNotAJWTInThePodTokenFileIsNamed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:294).
	tokenFile := writePodToken(t, "not-a-jwt")
	broker, calls := servePodBroker(t, "never-reached")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "is not a JWT") {
		t.Fatalf("stderr %q does not say the token is not a JWT", stderr.String())
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("the broker was asked %d times, want none", n)
	}
}

// TestARefusedPodLoginExitsAndNamesTheIdentity: a refusal names which pod was refused — its
// ServiceAccount and its UID — with the HTTP status, the broker's code and message, and never the
// token itself. POD_SELF_ENROLL_ONLY is what a login bound to another pod, or to none, is
// answered.
func TestARefusedPodLoginExitsAndNamesTheIdentity(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:324).
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "ACCOUNT_NOT_LISTED", "error": "this ServiceAccount logs in as no service",
		})
	}))
	defer broker.Close()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{testPodAccount, testPodUID, "HTTP 403", "ACCOUNT_NOT_LISTED", "could not log in"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the refusal printed the pod's token")
	}
}

// TestARefusedPodLoginBesideAnAddressThatIsNotCleanPrintsItsStatusAlone: beside an
// AGENT_SECRETS_URL that carries a credential, the refusal line still names the pod and the HTTP
// status, and none of what the answerer wrote - the words, which repeat that credential, or the
// code - since whoever answers at that address can fill any of them with it.
func TestARefusedPodLoginBesideAnAddressThatIsNotCleanPrintsItsStatusAlone(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:357).
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	var words string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "ACCOUNT_NOT_LISTED", "error": words})
	}))
	defer broker.Close()
	credentialed := strings.Replace(broker.URL, "://", "://demouser7q:demopass9z-not-a-secret@", 1)
	words = "the login names " + credentialed + ", which lists no service"
	t.Setenv("AGENT_SECRETS_URL", credentialed)
	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range []string{testPodAccount, testPodUID, "HTTP 403", "could not log in"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	for _, withheld := range []string{"demouser7q", "demopass9z-not-a-secret", "ACCOUNT_NOT_LISTED", "lists no service"} {
		if strings.Contains(stderr.String(), withheld) {
			t.Errorf("stderr %q names %q, which the answerer wrote", stderr.String(), withheld)
		}
	}
}

// TestARefusedEnrolmentLeavesTheRefusalBesideTheKey: a pod whose enrolment the broker refuses
// keeps its key — a new key while the old enrollment is live is ALREADY_ENROLLED — and that
// leaves the one state a later call cannot read on its own: key.pem with no enrollment, which
// selectSigner reaches on the key alone and then finds no id for. So the refusal is written
// into enrollment.error beside the key, and a later `agent-secrets` call says what the broker
// said. A re-run that succeeds takes that file away again.
func TestARefusedEnrolmentLeavesTheRefusalBesideTheKey(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:408); it signals the test process or changes its signal handling (main.buildSigner, main.buildChainSigner, main.selectSigner, main.signerChain, main.machineChainSource, main.registerMachine, main.registerMachineSession, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.buildSigner, main.buildChainSigner, main.selectSigner, main.signerChain, main.machineChainSource, main.registerMachine, main.registerMachineSession, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	out := t.TempDir()
	refuse := true
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/launcher-credentials/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour),
			})
			return
		}
		if refuse {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "POD_SELF_ENROLL_ONLY", "error": "this machine login enrols its own pod alone",
			})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_id": "enrollment-of-the-pod", "lease_expires_at": time.Now().Add(time.Hour), "slot": nil,
		})
	}))
	defer broker.Close()
	t.Setenv("AGENT_SECRETS_URL", broker.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", out)
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")

	var stdout, stderr bytes.Buffer
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("the refused enrolment = exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, keyFile)); err != nil {
		t.Fatalf("the key after a refused enrolment: %v; it is kept on purpose", err)
	}
	recorded, err := readTrimmed(filepath.Join(out, enrollmentErrorFile))
	if err != nil {
		t.Fatalf("read %s after a refused enrolment: %v", enrollmentErrorFile, err)
	}
	for _, want := range []string{"POD_SELF_ENROLL_ONLY", "HTTP 403", testPodUID} {
		if !strings.Contains(recorded, want) {
			t.Fatalf("%s holds %q, which does not name %q", enrollmentErrorFile, recorded, want)
		}
	}
	if strings.Contains(recorded, token) {
		t.Fatalf("%s holds the pod's token", enrollmentErrorFile)
	}

	// What a later call now says: the broker's refusal, not a missing file.
	if _, _, err := buildSigner(io.Discard); err == nil ||
		!strings.Contains(err.Error(), enrollmentErrorFile) || !strings.Contains(err.Error(), "POD_SELF_ENROLL_ONLY") {
		t.Fatalf("a later call after a refused enrolment = %v; want it to name %s and what the broker said", err, enrollmentErrorFile)
	}

	refuse = false
	stdout.Reset()
	stderr.Reset()
	if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("the run that succeeds = exit %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, enrollmentErrorFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after a run that succeeded: %v; want it gone", enrollmentErrorFile, err)
	}
	if _, _, err := buildSigner(io.Discard); err != nil {
		t.Fatalf("a later call after the run that succeeded: %v", err)
	}
}

// TestTwoRunsOnOneDirectoryTakeTurns: a pod can run `enroll --pod` more than once over one key
// directory, and without the lock the second logs in again — moving the launcher credential the
// first is still enrolling under — and the two race over key.pem and enrollment. The lock is the
// only thing holding that off, and nothing else in this package exercises it. Four runs at once
// on one directory must leave one key, one enrollment id, and must not have made four logins
// race: each run either made the enrollment or got that same one back.
func TestTwoRunsOnOneDirectoryTakeTurns(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at pod_enroll_test.go:463).
	tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
	broker, calls := servePodBroker(t, "enrollment-of-the-pod")
	out := filepath.Join(t.TempDir(), "keys")
	t.Setenv("AGENT_SECRETS_URL", broker.URL)

	const runs = 4
	codes := make([]int, runs)
	outs := make([]strings.Builder, runs)
	errs := make([]strings.Builder, runs)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range runs {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			codes[i] = cmdEnrollPod(testPodService, tokenFile, out, podProjects{}, &outs[i], &errs[i])
		}()
	}
	close(start)
	wait.Wait()

	for i := range runs {
		if codes[i] != 0 {
			t.Fatalf("run %d = exit %d, stderr %q; every run on one directory must succeed in its turn", i, codes[i], errs[i].String())
		}
		if got := strings.TrimSpace(outs[i].String()); got != "enrollment-of-the-pod" {
			t.Fatalf("run %d printed %q, want the one enrollment id", i, got)
		}
	}
	if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != "enrollment-of-the-pod" {
		t.Fatalf("the enrollment file = %q (%v), want the one id", id, err)
	}
	// One key, so the enrollment every run got back is the one that key holds: the first run
	// wrote it and the rest reused it rather than writing their own.
	thumbprints := map[string]bool{}
	for _, call := range calls() {
		if call.path == "/v1/enrollments" {
			thumbprints[call.body["thumbprint"]] = true
		}
	}
	if len(thumbprints) != 1 {
		t.Fatalf("the runs enrolled %d keys, want one: the lock did not serialize them", len(thumbprints))
	}
}

// TestEnrollPodWaitsOutA503sRetryAfter: the broker answers 503 with Retry-After, refusing
// nothing, when it could not check the pod's token because its issuer's key set could not be read,
// and enroll --pod waits that wait out and asks again: on its token login ("login"), on its
// enrolment ("enrolment") and on the renewal of an enrollment its key already holds ("renewal"),
// each answered here with the broker's own 503 (unreadablekeysettest) with its Retry-After alone
// changed per row. The run waits on a fake clock (enrolPod's podClock), which the test moves to
// the end of each wait once the run has armed it, so a row reads each wait the run made and the
// moment of each call on that clock: the call asked again comes no sooner than the wait after the
// first, signed anew, and the run then enrols. "90 s" reads a wait above a minute as a minute.
// "bounded": every wait of the run is inside one budget of two minutes from its start, so a login
// answered 503 twice and then an enrolment answered 503 every time end the run within the two
// minutes, at the last wait that fits, exit 1, with a line on stderr and in enrollment.error
// beside the key that names the outage and how long the run tried, and reads as no refusal.
func TestEnrollPodWaitsOutA503sRetryAfter(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv in its rows).
	const enrollmentID = "enrollment-of-the-pod"
	const login, enrolment, renewal = "/v1/launcher-credentials/token", "/v1/enrollments", "/v1/enrollments/" + enrollmentID + "/renew"
	fixture := unreadablekeysettest.Read(t).JSONRoute
	asking := func(retryAfter string) unreadablekeysettest.Answer {
		answer := fixture
		answer.RetryAfter = retryAfter
		return answer
	}
	start := time.Date(2026, time.November, 28, 12, 0, 0, 0, time.UTC)
	token := fakePodToken(t, testPodAccount, testPodUID)
	for _, row := range []struct {
		name   string
		outage podBrokerOutage
		// enrolled is a key that already holds its live enrollment, which a first run makes, so
		// the broker answers the run's enrolment 200 and the run renews that enrollment.
		enrolled bool
		wait     time.Duration
	}{
		{"login", podBrokerOutage{login, asking("5"), 1}, false, 5 * time.Second},
		{"enrolment", podBrokerOutage{enrolment, asking("5"), 1}, false, 5 * time.Second},
		{"renewal", podBrokerOutage{renewal, asking("5"), 1}, true, 5 * time.Second},
		{"90 s", podBrokerOutage{login, asking("90"), 1}, false, time.Minute},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := clocktest.New(start)
			broker, calls := servePodBroker(t, enrollmentID, podBrokerClock(fake.Now), podBrokerOutages(row.outage))
			t.Setenv("AGENT_SECRETS_URL", broker.URL)
			tokenFile := writePodToken(t, token)
			out := t.TempDir()
			if row.enrolled {
				if code, _, stderr, waits := enrolPodOnFakeClock(t, fake, tokenFile, out); code != 0 || len(waits) != 0 {
					t.Fatalf("the first run = exit %d after waits %v, stderr %q; want it to enrol the key at once", code, waits, stderr)
				}
			}
			earlier := len(calls())
			code, stdout, stderr, waits := enrolPodOnFakeClock(t, fake, tokenFile, out)
			if code != 0 || strings.TrimSpace(stdout) != enrollmentID {
				t.Fatalf("enroll --pod through a 503 asking %s = exit %d, stdout %q, stderr %q; want the enrollment id", row.outage.answer.RetryAfter, code, stdout, stderr)
			}
			if !slices.Equal(waits, []time.Duration{row.wait}) {
				t.Fatalf("the run waited %v on its clock through a 503 asking %s s, want %v", waits, row.outage.answer.RetryAfter, []time.Duration{row.wait})
			}
			var asked []podCall
			for _, call := range calls()[earlier:] {
				if call.path == row.outage.path {
					asked = append(asked, call)
				}
			}
			if len(asked) != 2 {
				t.Fatalf("the run asked %s %d times, want twice: the 503, then once its wait had passed", row.outage.path, len(asked))
			}
			if gap := asked[1].at.Sub(asked[0].at); gap < row.wait {
				t.Fatalf("the run asked %s again %s after the 503 on its clock, want no sooner than %s", row.outage.path, gap, row.wait)
			}
			// A login's request object and a proof each carry their moment, which the broker
			// takes only within its skew of now: the second try signs its own.
			if asked[1].body["request"] == asked[0].body["request"] && asked[1].proof == asked[0].proof {
				t.Fatalf("the run sent %s again with the first try's signed request object and proof", row.outage.path)
			}
			if id, err := readTrimmed(filepath.Join(out, enrollmentFile)); err != nil || id != enrollmentID {
				t.Fatalf("the enrollment file = %q (%v), want the enrollment id", id, err)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		fake := clocktest.New(start)
		broker, calls := servePodBroker(t, enrollmentID, podBrokerClock(fake.Now),
			podBrokerOutages(podBrokerOutage{login, asking("5"), 2}, podBrokerOutage{enrolment, asking("5"), 0}))
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		out := t.TempDir()
		code, stdout, stderr, waits := enrolPodOnFakeClock(t, fake, writePodToken(t, token), out)
		if code != 1 || stdout != "" {
			t.Fatalf("enroll --pod through a broker answering 503 every time = exit %d, stdout %q, stderr %q; want exit 1 and no enrollment id", code, stdout, stderr)
		}
		// The logins at 0 s and 5 s and the enrolments from 10 s on are each asked again 5 s on,
		// and the enrolment at 115 s is the last: a wait of 5 s from there ends at the budget.
		tried := fake.Now().Sub(start)
		if tried != podOutageBudget-5*time.Second || len(waits) != 23 {
			t.Fatalf("the run waited %v and ended %s after it began on its clock, want 23 waits of 5 s, ending 5 s short of the %s budget", waits, tried, podOutageBudget)
		}
		logins, enrolments := 0, 0
		for _, call := range calls() {
			switch call.path {
			case login:
				logins++
			case enrolment:
				enrolments++
			}
		}
		if logins != 3 || enrolments != 22 {
			t.Fatalf("the broker was asked %d logins and %d enrolments, want 3 and 22, all in the one budget", logins, enrolments)
		}
		recorded, err := os.ReadFile(filepath.Join(out, enrollmentErrorFile))
		if err != nil {
			t.Fatalf("read %s: %v", enrollmentErrorFile, err)
		}
		if strings.TrimSpace(string(recorded)) != strings.TrimSpace(stderr) {
			t.Fatalf("%s holds %q, want the line stderr printed, %q", enrollmentErrorFile, recorded, stderr)
		}
		for _, words := range []string{
			"could not enrol: the broker could not check the pod's token, because its issuer's key set could not be read",
			"HTTP 503 ISSUER_UNAVAILABLE",
			"tried for 1m55s, and the wait of 5s the broker asked for runs past the 2m0s",
		} {
			if !strings.Contains(stderr, words) {
				t.Errorf("stderr %q does not say %q", stderr, words)
			}
		}
		if strings.Contains(strings.ToLower(stderr), "refus") {
			t.Errorf("stderr %q reads as a refusal of the pod, which the broker never made", stderr)
		}
		if exists(filepath.Join(out, enrollmentFile)) {
			t.Fatal("the run wrote an enrollment file though the broker enrolled nothing")
		}
	})
}

// enrolPodOnFakeClock runs enrolPod for tokenFile into out on fake, and moves fake to the end of
// each wait the run arms on it once the run has armed it, until the run exits. It answers the
// run's exit code, stdout and stderr, and each wait the run made, in order.
func enrolPodOnFakeClock(t *testing.T, fake *clocktest.Fake, tokenFile, out string) (code int, stdout, stderr string, waits []time.Duration) {
	t.Helper()
	var runOut, runErr bytes.Buffer
	exited := make(chan int, 1)
	go func() {
		exited <- enrolPod(fake, testPodService, tokenFile, out, podProjects{}, brokerBaseURL(), "agent-secrets enroll", &runOut, &runErr)
	}()
	type step struct {
		exited bool
		code   int
		until  time.Time
	}
	for {
		next := testwait.Until(t, "enroll --pod to arm a wait on its clock or to exit", func() (step, bool) {
			select {
			case code := <-exited:
				return step{exited: true, code: code}, true
			default:
			}
			if armed := fake.Armed(); len(armed) > 0 {
				return step{until: armed[0]}, true
			}
			return step{}, false
		})
		if next.exited {
			return next.code, runOut.String(), runErr.String(), waits
		}
		waits = append(waits, next.until.Sub(fake.Now()))
		fake.Set(next.until)
	}
}
