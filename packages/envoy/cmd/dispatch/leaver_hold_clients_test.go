//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	brokerhelper "github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/testtmp"
)

// The person's own commands through a hold (c-real-clients): agent-secrets and
// agent-secrets-helper, built from this tree, run as the processes a person's machine runs, against
// the leaver rig's real broker and real Dispatch router (leaver_hold_rig_test.go). A hold refuses
// their commands and revokes nothing: the helper keeps its machine login and the machine keeps the
// person's sign-in, and once the person signs in to Dispatch again the same commands are served,
// with no restart and no new login. The helper is linux-only, and so is this file.

const (
	// leaverRigSecret is the agent-tier secret every broker rig holds for ada (ciphertest.NewRig's
	// TEST_AUTO_SECRET), which her own sessions get at once, and leaverRigSecretValue its value.
	leaverRigSecret      = "TEST_AUTO_SECRET"
	leaverRigSecretValue = "test-auto-secret-v1"
	// leaverReleaseBound is how soon after a person's sign-in to Dispatch the broker serves again
	// each credential of theirs it held (R4).
	leaverReleaseBound = 40 * time.Second
	// leaverRegisterWait is the --wait, in seconds, each `agent-secrets register` gives the helper
	// to enrol the session it registers. The helper enrols a session after its register answers,
	// so a command run at once under a register that waits for nothing can ask the helper to sign
	// before the enrolment is made, and fail NOT_ENROLLED with no broker answer to name.
	leaverRegisterWait = "5"
	// leaverCommandTimeout bounds each command this file runs.
	leaverCommandTimeout = 2 * time.Minute
	// leaverDeviceSecret is the secret of the device-flow client a person signs in on the machine
	// with. The client's id is the audience the broker takes a person's identity token for
	// (ciphertest.PersonSignInAudience), because the fake Google mints each identity token for the
	// client it signs in.
	leaverDeviceSecret = "leaver-device-secret"
)

// leaverHeldCure is the words of the broker's refusal of ada's credentials while Dispatch holds
// her that say what ends the hold.
const leaverHeldCure = adaEmail + " signs in to Dispatch"

// TestLeaverHoldThroughTheRealClients is c-real-clients, in two runs on rigs of their own. In the
// first, the helper holds a machine login ada approved through Dispatch, and each command runs in a
// session the helper registered. In the second, no helper runs: ada signs in on the machine with
// `agent-secrets login`, and each command acts as her sign-in.
func TestLeaverHoldThroughTheRealClients(t *testing.T) {
	clients := leaverClients{
		agentSecrets: cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets", Dir: filepath.Join("..", "cipher")}),
		helper:       cmdtest.BuildCommand(t, cmdtest.Command{Name: "agent-secrets-helper", Dir: filepath.Join("..", "cipher-helper")}),
	}
	t.Run("the helper's machine login", clients.throughTheHelper)
	t.Run("a person's sign-in on the machine", clients.throughASignIn)
}

// leaverClients is the two binaries built from the tree.
type leaverClients struct {
	agentSecrets string
	helper       string
}

// throughTheHelper: the helper runs in machine-login mode for ada. Before the hold, a command in a
// session it registered prints the secret's value, through the helper. Held, the same command
// fails naming MEMBERSHIP_HELD and the cure, and the helper, still the same process, still holds
// the login its machine file keeps. Within 40 seconds of ada's sign-in to Dispatch, the same
// command through the same helper prints the value, under that same login.
func (c leaverClients) throughTheHelper(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate())
	ada := r.signIn(t, adaEmail)
	ada.dsp = r.mintToken(t, ada, "ada's agent")
	h := c.startHelper(t, r)
	h.logIn(t, r, ada)

	source := h.inSession(t, c.agentSecrets, "identity", "--source")
	if first, _, _ := strings.Cut(source.stdout, "\n"); source.exit != 0 || !strings.HasPrefix(first, "helper: ") {
		t.Fatalf("%s; want exit 0 and a first line starting %q: the command must take the helper's session", source, "helper: ")
	}
	secret := []string{c.agentSecrets, leaverRigSecret, "--", "printenv", leaverRigSecret}
	if served := h.inSession(t, secret...); !served.served() {
		t.Fatalf("before ada is held, %s; want exit 0 and %q", served, leaverRigSecretValue+"\n")
	}
	t.Logf("before ada is held, a session the helper registered was served %s", leaverRigSecret)

	holdAda(t, r, ada)
	held := h.inSession(t, secret...)
	if held.exit == 0 || !strings.Contains(held.stderr, "MEMBERSHIP_HELD") || !strings.Contains(held.stderr, leaverHeldCure) {
		t.Errorf("while Dispatch holds ada, %s; want a non-zero exit, and standard error naming MEMBERSHIP_HELD and saying %q", held, leaverHeldCure)
	} else {
		t.Logf("while Dispatch holds ada, the command exited %d, naming MEMBERSHIP_HELD and the cure", held.exit)
	}
	h.expectKeptLogin(t, "while Dispatch holds ada")

	r.signInAgain(t, adaEmail)
	if servedWithin(t, "the command in a session the helper registered", time.Now(), func() leaverRun { return h.inSession(t, secret...) }) {
		if session := lastServingSession(t, r); session.credential != h.credential {
			t.Errorf("after the release the broker served the command under the launcher credential %s; want %s, the machine login the helper kept", session.credential, h.credential)
		}
	}
	h.expectKeptLogin(t, "after ada signed in again")
}

// throughASignIn: no helper. Ada signs in on the machine with `agent-secrets login` against the
// rig's fake Google, which the machine's own Google identity, served by the rig's fake metadata
// server, acts beside; each command then takes her sign-in. Held, with Google refusing none of her
// grants, her sign-in's one-hour credential and its session's lease are made to lapse in the
// broker's database, so the next command registers her session again, and that trade is the one
// the hold refuses. Within 40 seconds of her sign-in to Dispatch the command registers again and
// is served, with no new `agent-secrets login`, under her sign-in and not the machine's identity.
func (c leaverClients) throughASignIn(t *testing.T) {
	r := newLeaverRig(t, withBrokerGate(), withPersonSignIns())
	ada := r.signIn(t, adaEmail)
	ada.dsp = r.mintToken(t, ada, "ada's agent")
	m := c.signInMachine(t, r)
	m.login(t, r)
	signedIn := m.signIn(t)

	m.expectSignInSource(t, "after `agent-secrets login`")
	if t.Failed() {
		t.FailNow()
	}
	secret := []string{leaverRigSecret, "--", "printenv", leaverRigSecret}
	if served := m.run(t, secret...); !served.served() {
		t.Fatalf("before ada is held, %s; want exit 0 and %q", served, leaverRigSecretValue+"\n")
	}
	t.Logf("before ada is held, her sign-in on the machine was served %s", leaverRigSecret)

	holdAda(t, r, ada)
	// Dispatch has recorded the hold, which ends only at her next sign-in to Dispatch. From here
	// Google grants her machine sign-in's refreshes again, so only the broker's hold can refuse
	// the trade below.
	r.google.RefuseRefresh(adaEmail, false)
	lapseSignIn(t, r, signedIn)
	held := m.run(t, secret...)
	if held.exit == 0 || !strings.Contains(held.stderr, oauthwire.MembershipHeld) || !strings.Contains(held.stderr, leaverHeldCure) {
		t.Errorf("while Dispatch holds ada, her sign-in's credential and lease lapsed, %s; want a non-zero exit, and standard error naming %s and saying %q", held, oauthwire.MembershipHeld, leaverHeldCure)
	} else {
		t.Logf("while Dispatch holds ada, the command's trade was refused %s, and it exited %d", oauthwire.MembershipHeld, held.exit)
	}
	m.expectSignInSource(t, "while Dispatch holds ada")

	r.signInAgain(t, adaEmail)
	if !servedWithin(t, "the command under ada's sign-in on the machine", time.Now(), func() leaverRun { return m.run(t, secret...) }) {
		return
	}
	again := m.signIn(t)
	switch session := lastServingSession(t, r); {
	case again.Thumbprint != signedIn.Thumbprint:
		t.Errorf("after the release the machine's sign-in holds the box key %s; want %s, the key `agent-secrets login` made", again.Thumbprint, signedIn.Thumbprint)
	case again.EnrollmentID == "" || again.EnrollmentID == signedIn.EnrollmentID:
		t.Errorf("after the release the machine's sign-in names the enrollment %q; want a new one in place of the lapsed %s", again.EnrollmentID, signedIn.EnrollmentID)
	case session.enrollment != again.EnrollmentID || session.thumbprint != signedIn.Thumbprint || session.operator != adaEmail ||
		exchange.ReadSubject(session.subject).Kind != exchange.SubjectSignIn:
		t.Errorf("after the release the broker served the command under %+v; want the enrollment %s, under the launcher credential of ada's sign-in (key %s, operator %s, a sign-in's subject), not the machine's own identity's",
			session, again.EnrollmentID, signedIn.Thumbprint, adaEmail)
	default:
		t.Logf("after the release the broker served the command under ada's sign-in: enrollment %s, launcher credential %s (subject %s)", session.enrollment, session.credential, session.subject)
	}
}

// holdAda makes Dispatch hold ada as c-leaver-end-to-end does: Google refuses her refresh, her
// confirmation is two hours old, and the first request with her dsp_ token makes Dispatch refresh
// and hold her. The broker's answer about ada from before is moved past, so its next check of her
// asks Dispatch. Without the hold nothing after it measures anything, so it stops the test.
func holdAda(t *testing.T, r *leaverRig, ada *leaverPerson) {
	t.Helper()
	r.refuse(adaEmail)
	r.ageConfirmation(t, adaEmail, confirmationAge)
	r.pastBrokerCache(t)
	expectAnswer(t, "ada@example.com's dsp_ token, the first request after Google refuses her", http.StatusUnauthorized, "MEMBERSHIP_HELD").got(r.personalToken(t, ada.dsp))
	if t.Failed() {
		t.FailNow()
	}
}

// servedWithin runs attempt about once a second until it is served, and fails t when no attempt
// that ended within leaverReleaseBound of since was served, naming the last answer. It reports
// whether one was.
func servedWithin(t *testing.T, what string, since time.Time, attempt func() leaverRun) bool {
	t.Helper()
	deadline := since.Add(leaverReleaseBound)
	for {
		run := attempt()
		took := time.Since(since)
		switch {
		case run.served() && took <= leaverReleaseBound:
			t.Logf("%s was served %s after ada's sign-in to Dispatch", what, took.Round(100*time.Millisecond))
			return true
		case run.served():
			t.Errorf("%s was served %s after ada's sign-in to Dispatch; want within %s", what, took.Round(100*time.Millisecond), leaverReleaseBound)
			return false
		case time.Now().Add(time.Second).After(deadline):
			t.Errorf("%s was not served within %s of ada's sign-in to Dispatch; the last attempt: %s", what, leaverReleaseBound, run)
			return false
		}
		time.Sleep(time.Second)
	}
}

// leaverRun is one command's run: what was run, and how it exited and what it printed.
type leaverRun struct {
	command        string
	exit           int
	stdout, stderr string
}

func (r leaverRun) String() string {
	return fmt.Sprintf("`%s` exited %d, stdout %q, stderr %q", r.command, r.exit, r.stdout, r.stderr)
}

// served reports whether the run printed the rig's agent-tier secret's value, and nothing else,
// and exited 0.
func (r leaverRun) served() bool {
	return r.exit == 0 && r.stdout == leaverRigSecretValue+"\n"
}

// runLeaverCommand runs binary with args in env and answers its run. A command that does not end
// within leaverCommandTimeout, or that cannot be started, fails t.
func runLeaverCommand(t *testing.T, env []string, binary string, args ...string) leaverRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), leaverCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	run := leaverRun{command: strings.Join(append([]string{filepath.Base(binary)}, args...), " "), stdout: stdout.String(), stderr: stderr.String()}
	var exited *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("%s did not end within %s; stdout %q, stderr %q", run.command, leaverCommandTimeout, run.stdout, run.stderr)
	case err != nil && !errors.As(err, &exited):
		t.Fatalf("run %s: %v", run.command, err)
	}
	run.exit = cmd.ProcessState.ExitCode()
	return run
}

// leaverMachineEnv is the environment of a person's processes on a machine of the test's own,
// whose home directory is home: this process's own, less every AGENT_SECRETS_* variable, every XDG
// directory, the metadata server's host and every agent session id, which name this machine and the
// session running the test, with home and XDG directories under it. The machine then holds no key
// directory, no sign-in and no machine session until a command of the test makes one.
func leaverMachineEnv(t *testing.T, home string) []string {
	t.Helper()
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case strings.HasPrefix(name, "AGENT_SECRETS_"), strings.HasPrefix(name, "XDG_"), name == "HOME", name == googleidentity.MetadataHostVar,
			name == "OMP_SESSION_ID", name == "ENVOY_SESSION_ID", name == "CLAUDE_CODE_SESSION_ID", name == "CODEX_THREAD_ID":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home, "XDG_RUNTIME_DIR="+t.TempDir(),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"))
}

// leaverHelper is agent-secrets-helper running as a child process for ada's machine, and the
// environment of the commands that reach it.
type leaverHelper struct {
	process *cmdtest.Process
	// pid is the helper process's, taken when it started.
	pid         int
	sock        string
	machineFile string
	// env is the environment of ada's commands on the machine, which name the helper's socket and
	// the broker.
	env          []string
	agentSecrets string
	// credential is the launcher credential Dispatch's approval of the machine login minted.
	credential string
}

// startHelper starts the helper in machine-login mode, pointed at r's broker, with the rig's
// operator file, which names ada, and waits until it listens. Its output is logged if t fails.
func (c leaverClients) startHelper(t *testing.T, r *leaverRig) *leaverHelper {
	t.Helper()
	home := t.TempDir()
	h := &leaverHelper{
		sock:         filepath.Join(testtmp.Dir(t, "helper.sock.lock"), "helper.sock"),
		machineFile:  filepath.Join(home, ".config", "agent-secrets", "machine.json"),
		agentSecrets: c.agentSecrets,
	}
	h.env = append(leaverMachineEnv(t, home), cipherclient.BrokerURLVar+"="+r.broker.URL, "AGENT_SECRETS_HELPER_SOCK="+h.sock)
	cmd := exec.Command(c.helper, "serve")
	cmd.Env = append(slices.Clone(h.env), "AGENT_SECRETS_OPERATOR_FILE="+r.broker.OperatorFile, "AGENT_SECRETS_MACHINE_FILE="+h.machineFile)
	h.process = cmdtest.Start(t, "agent-secrets-helper", cmd)
	h.pid = cmd.Process.Pid
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent-secrets-helper (pid %d) wrote:\n%s", h.pid, h.process.Output.String())
		}
	})
	h.process.WaitForOutput(t, "agent-secrets-helper listening")
	return h
}

// logIn runs `agent-secrets machine login` against the helper, approves the code it prints on
// Dispatch's machine-login page as p, and requires the command to end once the helper holds the
// login, and the helper's machine file to keep the credential the approval minted.
func (h *leaverHelper) logIn(t *testing.T, r *leaverRig, p *leaverPerson) {
	t.Helper()
	cmd := exec.Command(h.agentSecrets, "machine", "login")
	cmd.Env = append(slices.Clone(h.env), "AGENT_SECRETS_APPROVE_URL=")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	ended := false
	defer func() {
		if !ended {
			_ = cmd.Process.Kill()
			<-waited
		}
	}()
	lines := bufio.NewScanner(out)
	if !lines.Scan() {
		t.Fatalf("machine login printed no code; stderr %q", stderr.String())
	}
	code, ok := strings.CutPrefix(lines.Text(), "machine login code: ")
	if !ok {
		t.Fatalf("machine login's first line is %q, want the machine login code", lines.Text())
	}
	go func() { // the rest of its output, so the command never blocks writing it
		for lines.Scan() {
		}
	}()
	h.credential = approveMachineLoginOnDispatch(t, r, p, code)
	select {
	case err = <-waited:
		ended = true
	case <-time.After(leaverCommandTimeout):
		t.Fatalf("machine login never read ada's approval within %s; stderr %q", leaverCommandTimeout, stderr.String())
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		t.Fatalf("machine login: %v", err)
	}
	if exit := cmd.ProcessState.ExitCode(); exit != 0 {
		t.Fatalf("machine login exited %d once ada approved it; stderr %q", exit, stderr.String())
	}
	kept, err := brokerhelper.LoadMachineFile(h.machineFile)
	if err != nil || kept.CredentialID != h.credential {
		t.Fatalf("after the machine login, the helper's machine file %s holds %+v (%v); want the credential %s ada's approval minted", h.machineFile, kept, err, h.credential)
	}
	t.Logf("ada approved the helper's machine login through Dispatch: launcher credential %s, kept in %s", h.credential, h.machineFile)
}

// approveMachineLoginOnDispatch approves the pending machine login whose code is code as p, with
// p's cookie, on Dispatch's machine-login page, which relays the approval to the broker, and
// answers the launcher credential the broker minted.
func approveMachineLoginOnDispatch(t *testing.T, r *leaverRig, p *leaverPerson, code string) string {
	t.Helper()
	body := leaverBody(t, map[string]string{"code": code})
	looked := cookieRequest(r.handler, http.MethodPost, "/api/v1/credential-requests/machine-lookup", body, p.signedIn)
	var lookup struct {
		RecordID string `json:"record_id"`
	}
	if err := json.Unmarshal(looked.Body.Bytes(), &lookup); err != nil || lookup.RecordID == "" {
		t.Fatalf("Dispatch's machine-lookup of the helper's login code %s answered %d %s, want its record", code, looked.Code, looked.Body.String())
	}
	approved := cookieRequest(r.handler, http.MethodPost, "/api/v1/credential-requests/"+lookup.RecordID+"/approve", body, p.signedIn)
	var decided struct {
		State        string `json:"state"`
		CredentialID string `json:"credential_id"`
	}
	if err := json.Unmarshal(approved.Body.Bytes(), &decided); err != nil || decided.State != "approved" || decided.CredentialID == "" {
		t.Fatalf("Dispatch's approve of the helper's machine login as %s answered %d %s, want it approved with a credential", p.email, approved.Code, approved.Body.String())
	}
	return decided.CredentialID
}

// inSession runs args as `agent-secrets register --wait N --exec -- args...`, so the command runs
// as a process inside a session the helper registered, as a machine session launches.
func (h *leaverHelper) inSession(t *testing.T, args ...string) leaverRun {
	t.Helper()
	return runLeaverCommand(t, h.env, h.agentSecrets, append([]string{"register", "--wait", leaverRegisterWait, "--exec", "--"}, args...)...)
}

// expectKeptLogin requires that the helper is the process it started as, and that it still holds
// the machine login its machine file keeps, the credential ada approved: its machine file names that
// credential, and `agent-secrets machine login-status` says the helper holds it.
func (h *leaverHelper) expectKeptLogin(t *testing.T, when string) {
	t.Helper()
	select {
	case <-h.process.Exited:
		t.Errorf("%s, agent-secrets-helper (pid %d) has exited (%v); want it still running, the same process", when, h.pid, h.process.Cmd.ProcessState)
		return
	default:
	}
	kept, err := brokerhelper.LoadMachineFile(h.machineFile)
	status := runLeaverCommand(t, h.env, h.agentSecrets, "machine", "login-status")
	switch {
	case err != nil:
		t.Errorf("%s, the helper's machine file %s holds no login (%v); want the credential %s ada approved", when, h.machineFile, err, h.credential)
	case kept.CredentialID != h.credential:
		t.Errorf("%s, the helper's machine file %s holds the credential %s; want %s, the one ada approved", when, h.machineFile, kept.CredentialID, h.credential)
	case status.exit != 0 || status.stdout != "issued\n":
		t.Errorf("%s, agent-secrets-helper (pid %d) no longer holds the login its machine file %s keeps (credential %s): %s; want exit 0 and %q",
			when, h.pid, h.machineFile, h.credential, status, "issued\n")
	default:
		t.Logf("%s, agent-secrets-helper is still pid %d and holds the login its machine file %s keeps (credential %s)", when, h.pid, h.machineFile, h.credential)
	}
}

// leaverSignInMachine is a machine with no helper, on which a person signs in with
// `agent-secrets login`: the environment of its commands, and where they keep its sign-ins.
type leaverSignInMachine struct {
	env          []string
	signIns      cipherclient.SignInStore
	agentSecrets string
}

// signInMachine is a machine of r's broker whose sign-in client is a device-flow client of r's fake
// Google, and whose own Google identity is the one r's fake metadata server serves: its commands'
// environment names that server's host (r.metadataHost), the metadata identity and the broker's
// machine audience, so no variable of this test process reaches them.
func (c leaverClients) signInMachine(t *testing.T, r *leaverRig) *leaverSignInMachine {
	t.Helper()
	r.google.EnableDeviceFlow(r.googleKey, ciphertest.PersonSignInAudience, leaverDeviceSecret)
	home := t.TempDir()
	secretFile := filepath.Join(t.TempDir(), "device-client-secret")
	if err := os.WriteFile(secretFile, []byte(leaverDeviceSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &leaverSignInMachine{signIns: cipherclient.SignInStore{Dir: filepath.Join(home, "signins")}, agentSecrets: c.agentSecrets}
	m.env = append(leaverMachineEnv(t, home),
		cipherclient.BrokerURLVar+"="+r.broker.URL,
		cipherclient.SignInIssuerVar+"="+r.google.URL(),
		cipherclient.SignInClientIDVar+"="+ciphertest.PersonSignInAudience,
		cipherclient.SignInClientSecretFileVar+"="+secretFile,
		cipherclient.SignInDirVar+"="+m.signIns.Dir)
	if r.metadataHost == "" {
		t.Fatal("the rig has no metadata machine: build it withPersonSignIns")
	}
	m.env = append(m.env,
		cipherclient.IdentityVar+"="+cipherclient.IdentityMetadata,
		cipherclient.MachineAudienceVar+"="+ciphertest.GoogleMachineAudience,
		googleidentity.MetadataHostVar+"="+r.metadataHost)
	return m
}

// leaverUserCode is how `agent-secrets login` tells the person the code to type.
var leaverUserCode = regexp.MustCompile(`type the code (\S+)`)

// login runs `agent-secrets login`, and approves the code it prints at the fake Google as ada, the
// Workspace account her Dispatch sign-ins are made as. It requires the command to say she is signed
// in on the machine.
func (m *leaverSignInMachine) login(t *testing.T, r *leaverRig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), leaverCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.agentSecrets, "login")
	cmd.Env = m.env
	var stdout strings.Builder
	cmd.Stdout = &stdout
	errOut, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer errOut.Close()
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	var stderr strings.Builder
	read := make(chan struct{})
	// approved is the answer of the one approval: a code printed again is not typed in again.
	approved := make(chan error, 1)
	go func() {
		defer close(read)
		lines := bufio.NewScanner(errOut)
		for typed := false; lines.Scan(); {
			stderr.WriteString(lines.Text() + "\n")
			if match := leaverUserCode.FindStringSubmatch(lines.Text()); match != nil && !typed {
				typed = true
				r.google.SignInAs(map[string]any{"sub": "google-" + adaEmail, "email": adaEmail, "email_verified": true, "hd": leaverDomain})
				approved <- r.google.ApproveDeviceCode(match[1])
			}
		}
	}()
	err = cmd.Wait()
	<-read
	select {
	case approveErr := <-approved:
		if approveErr != nil {
			t.Fatalf("ada types the code `agent-secrets login` printed at the fake Google: %v; stderr %q", approveErr, stderr.String())
		}
	default:
		t.Fatalf("`agent-secrets login` printed no code to type; stderr %q", stderr.String())
	}
	var exited *exec.ExitError
	switch {
	case ctx.Err() != nil:
		t.Fatalf("`agent-secrets login` did not end within %s; stderr %q", leaverCommandTimeout, stderr.String())
	case err != nil && !errors.As(err, &exited):
		t.Fatalf("`agent-secrets login`: %v", err)
	case cmd.ProcessState.ExitCode() != 0 || !strings.HasPrefix(stdout.String(), adaEmail+" is signed in on this machine"):
		t.Fatalf("`agent-secrets login` exited %d, stdout %q, stderr %q; want 0, saying %s is signed in on this machine", cmd.ProcessState.ExitCode(), stdout.String(), stderr.String(), adaEmail)
	}
	t.Logf("`agent-secrets login` printed %q", strings.TrimSpace(stdout.String()))
}

// signIn is the one sign-in the machine keeps, ada's, with the enrollment it names.
func (m *leaverSignInMachine) signIn(t *testing.T) cipherclient.SignInRecord {
	t.Helper()
	records, err := m.signIns.List()
	if err != nil || len(records) != 1 || records[0].Person != adaEmail || records[0].EnrollmentID == "" {
		t.Fatalf("the sign-ins the machine keeps in %s are %+v (%v); want ada's alone, naming an enrollment", m.signIns.Dir, records, err)
	}
	return records[0]
}

// run runs `agent-secrets args...` on the machine, in no session the helper registered.
func (m *leaverSignInMachine) run(t *testing.T, args ...string) leaverRun {
	t.Helper()
	return runLeaverCommand(t, m.env, m.agentSecrets, args...)
}

// expectSignInSource requires `agent-secrets identity --source` to name ada's sign-in first.
func (m *leaverSignInMachine) expectSignInSource(t *testing.T, when string) {
	t.Helper()
	source := m.run(t, "identity", "--source")
	if first, _, _ := strings.Cut(source.stdout, "\n"); source.exit != 0 || first != "sign-in: "+adaEmail {
		t.Errorf("%s, %s; want exit 0 and a first line %q", when, source, "sign-in: "+adaEmail)
		return
	}
	t.Logf("%s, `agent-secrets identity --source` names %q", when, "sign-in: "+adaEmail)
}

// lapseSignIn ages, in the broker's database, the one-hour launcher credential the trade of record's
// sign-in minted, as an hour of the hold would, and the lease of the session record names, as its
// lease runs out with nothing renewing it. The next command finds that session not live and
// registers it again, which trades the sign-in.
func lapseSignIn(t *testing.T, r *leaverRig, record cipherclient.SignInRecord) {
	t.Helper()
	credential, err := r.broker.Store.Pool.Exec(t.Context(),
		`update launcher_credentials set expires_at = now() - interval '2 hours' where key_thumbprint = $1 and revoked_at is null`, record.Thumbprint)
	if err != nil || credential.RowsAffected() != 1 {
		t.Fatalf("age the launcher credential of %s's sign-in (key %s): %d rows (%v); want 1", record.Person, record.Thumbprint, credential.RowsAffected(), err)
	}
	lease, err := r.broker.Store.Pool.Exec(t.Context(),
		`update enrollments set lease_expires_at = now() - interval '1 minute' where id = $1::uuid and revoked_at is null`, record.EnrollmentID)
	if err != nil || lease.RowsAffected() != 1 {
		t.Fatalf("age the lease of %s's session %s: %d rows (%v); want 1", record.Person, record.EnrollmentID, lease.RowsAffected(), err)
	}
}

// leaverServingSession is the broker session that served a request: its enrollment, and the
// launcher credential it enrolled under, with that credential's operator, subject and key. A
// person's sign-in and the machine's own identity each mint a credential under the machine's
// service; the subject says which of the two minted it (exchange.ReadSubject).
type leaverServingSession struct {
	enrollment, credential, operator, subject, thumbprint string
}

// lastServingSession is the session of the last request the broker granted.
func lastServingSession(t *testing.T, r *leaverRig) leaverServingSession {
	t.Helper()
	var s leaverServingSession
	if err := r.broker.Store.Pool.QueryRow(t.Context(), `
		select e.id::text, c.id::text, coalesce(c.operator, ''), coalesce(c.subject, ''), c.key_thumbprint
		from requests q
		join enrollments e on e.id = q.enrollment_id
		join launcher_credentials c on c.id = e.launcher_credential_id
		where q.state = 'granted'
		order by q.created_at desc
		limit 1
	`).Scan(&s.enrollment, &s.credential, &s.operator, &s.subject, &s.thumbprint); err != nil {
		t.Fatalf("read the session of the last request the broker granted: %v", err)
	}
	return s
}
