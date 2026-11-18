// packages/envoy/cmd/agent-secrets/machine_linux_test.go
//go:build linux

// The machine and grant commands act under this machine's login: the CLI asks the helper for a
// launcher proof (sign-launcher) and calls the broker's operator routes with it. These tests drive
// the real CLI against a real helper and a real broker (brokertest.NewRig), as
// relogin_linux_test.go does. Linux-only for the real helper's pidfd pinning.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/testwait"
)

// tableRows splits a table the CLI printed into its header's columns and each row's fields.
func tableRows(t *testing.T, stdout string) (header []string, rows [][]string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	header = strings.Fields(lines[0])
	for _, line := range lines[1:] {
		rows = append(rows, strings.Fields(line))
	}
	return header, rows
}

// loggedInElsewhere logs another machine of the rig's operator in, with a key of its own, and
// returns its helper.Broker and the launcher credential its approval minted.
func loggedInElsewhere(t *testing.T, rig *brokertest.Rig, host string) (*helper.Broker, string) {
	t.Helper()
	b := &helper.Broker{URL: brokeraddr.New(rig.URL), OperatorFile: rig.OperatorFile, MachineFile: filepath.Join(t.TempDir(), "machine.json"), HTTP: http.DefaultClient}
	t.Cleanup(b.Close)
	code, err := b.Login(context.Background(), host)
	if err != nil {
		t.Fatalf("log %s in: %v", host, err)
	}
	id := rig.DecideMachineLogin(t, code, true)
	testwait.Until(t, host+"'s login to issue", func() (any, bool) {
		return b.LoginStatus(), b.HasCredential()
	})
	return b, id
}

// TestMachineListAndRevokeUnderTheMachinesLogin: once this machine is logged in, `machine list`
// shows its login and another of the operator's machines, and `--json` prints the broker's body
// verbatim, which with no service login on the rig is exactly what Dispatch's machine-login page
// reads for the operator. `machine revoke` ends the other machine's login, which the broker then
// refuses, and prints `revoked <id>`. Revoking this machine's own login, its id typed in upper
// case (the broker reads any case), warns that it ends this machine's access, and the broker
// refuses the next command, which says to log the machine in again.
func TestMachineListAndRevokeUnderTheMachinesLogin(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	machine := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"machine"}, args...)...)
	}
	laptop, laptopID := loggedInElsewhere(t, rig, "example-host-laptop")

	stdout, stderr, exit := machine("list")
	if exit != 0 || !strings.Contains(stdout, "testhost") { // serveRealHelper's Hostname is the HOST column
		t.Fatalf("machine list: %d %q %q", exit, stdout, stderr)
	}
	header, rows := tableRows(t, stdout)
	if want := []string{"CREDENTIAL_ID", "HOST", "APPROVED_BY", "ISSUED", "EXPIRES", "STATE"}; !slices.Equal(header, want) || len(rows) != 2 {
		t.Fatalf("machine list = %q, want the header %v and two rows", stdout, want)
	}
	if laptopRow := rows[0]; laptopRow[0] != laptopID || laptopRow[1] != "example-host-laptop" || laptopRow[2] != rig.Operator || laptopRow[5] != "ok" {
		t.Fatalf("machine list's first row = %v, want the laptop's login %s, newest first", laptopRow, laptopID)
	}
	own := rows[1][0]
	if rows[1][1] != "testhost" || rows[1][5] != "ok" {
		t.Fatalf("machine list's second row = %v, want this machine's login", rows[1])
	}

	stdout, stderr, exit = machine("list", "--json")
	status, page := rig.UI(t, http.MethodGet, "/v1/launcher-credentials?approver="+url.QueryEscape(rig.Operator), nil)
	if exit != 0 || status != http.StatusOK || stdout != string(page) {
		t.Fatalf("machine list --json: exit %d, stdout %q, stderr %q; want Dispatch's page's body %q", exit, stdout, stderr, page)
	}

	// Every line names the broker it acted at, so no answer of this CLI's reads as another
	// secrets CLI's (source.go).
	at := " at " + brokerSource(brokeraddr.New(rig.URL)).String() + "\n"
	if stdout, stderr, exit := machine("revoke", laptopID); exit != 0 || stdout != "revoked "+laptopID+at || stderr != "" {
		t.Fatalf("machine revoke the laptop: exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := laptop.EnrollSessionKey(context.Background(), "laptop-box", thumbprint, nil); err == nil || laptop.HasCredential() {
		t.Fatalf("the laptop enrolling after its login was revoked: %v, credential held %v; want the broker's refusal", err, laptop.HasCredential())
	}
	if stdout, _, _ := machine("list"); strings.Contains(stdout, laptopID) || !strings.Contains(stdout, own) {
		t.Fatalf("machine list after revoking the laptop = %q, want this machine's login alone", stdout)
	}

	const ownWarning = "agent-secrets machine revoke: revoking this machine's own login ends every session it enrolled, this one's broker access included\n"
	if stdout, stderr, exit := machine("revoke", strings.ToUpper(own)); exit != 0 || stdout != "revoked "+strings.ToUpper(own)+at || stderr != ownWarning {
		t.Fatalf("machine revoke this machine's own login in upper case: exit %d, stdout %q, stderr %q; want %q", exit, stdout, stderr, ownWarning)
	}
	refused := "agent-secrets machine list: the launcher credential is not valid (LAUNCHER_INVALID); this machine's login to " + brokerSource(brokeraddr.New(rig.URL)).String() + " is expired or revoked (run: agent-secrets machine login)\n"
	if stdout, stderr, exit := machine("list"); exit != 1 || stdout != "" || stderr != refused {
		t.Fatalf("machine list after revoking this machine's own login: exit %d, stdout %q, stderr %q; want 1, %q", exit, stdout, stderr, refused)
	}
}

// TestGrantListAndRevokeUnderTheMachinesLogin: a session key this machine enrolled gets an
// agent-tier secret automatically. `grant list` shows that grant, and `--json` prints exactly what
// Dispatch's Live grants page reads for the operator. `grant revoke` ends it, as the operator's
// revoke on that page does: the session can no longer read the value, and its next request for
// the secret, which now names a reason because the revoke withheld the name and so put it to a
// person, waits for approval.
func TestGrantListAndRevokeUnderTheMachinesLogin(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	grant := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"grant"}, args...)...)
	}
	boxKey, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&boxKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := "grant-box-" + t.Name()
	enrolled, err := helper.Call(sock, helper.Request{Op: "enroll-session-key", RuntimeID: runtimeID, Thumbprint: thumbprint}, 10*time.Second)
	if err != nil || !enrolled.OK {
		t.Fatalf("enroll-session-key: %+v %v", enrolled, err)
	}
	keyDir := writeKeyDir(t, boxKey, enrolled.EnrollmentID)
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json")
	var granted RequestResult
	if err := json.Unmarshal([]byte(stdout), &granted); exit != 0 || err != nil || granted.State != "granted" || granted.GrantID == nil {
		t.Fatalf("request TEST_AUTO_SECRET from the box: exit %d, stdout %q, stderr %q; want it granted at once", exit, stdout, stderr)
	}
	grantID := *granted.GrantID

	stdout, stderr, exit = grant("list")
	header, rows := tableRows(t, stdout)
	if want := []string{"GRANT_ID", "SECRETS", "GRANTED", "APPROVER", "DECIDED_BY", "SESSION", "OPERATOR", "EXPIRES"}; exit != 0 || !slices.Equal(header, want) || len(rows) != 1 {
		t.Fatalf("grant list: exit %d, stdout %q, stderr %q; want the header %v and one row", exit, stdout, stderr, want)
	}
	if row := rows[0]; !slices.Equal(row[:7], []string{grantID, "TEST_AUTO_SECRET", "automatic", "-", "-", "session/" + runtimeID, rig.Operator}) {
		t.Fatalf("grant list's row = %v, want the session key's automatic grant %s", row, grantID)
	}

	stdout, stderr, exit = grant("list", "--json")
	status, page := rig.UI(t, http.MethodGet, "/v1/grants?approver="+url.QueryEscape(rig.Operator), nil)
	if exit != 0 || status != http.StatusOK || stdout != string(page) {
		t.Fatalf("grant list --json: exit %d, stdout %q, stderr %q; want the Live grants page's body %q", exit, stdout, stderr, page)
	}

	if stdout, stderr, exit := grant("revoke", grantID); exit != 0 || stdout != "revoked "+grantID+" at "+brokerSource(brokeraddr.New(rig.URL)).String()+"\n" || stderr != "" {
		t.Fatalf("grant revoke: exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	if _, err := newClient(brokeraddr.New(rig.URL)).GrantValues(context.Background(), &fileSigner{key: boxKey, enrollmentID: enrolled.EnrollmentID}, grantID, nil, releaseCall{}); err == nil {
		t.Fatal("the box read the revoked grant's values")
	}
	if stdout, _, _ := grant("list"); strings.Contains(stdout, grantID) {
		t.Fatalf("grant list after the revoke = %q, want the grant gone", stdout)
	}
	if stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, nil, "request", "TEST_AUTO_SECRET", "--json", "--reason", "asking again after the operator's revoke"); exit != exitPending {
		t.Fatalf("the box asking again after the operator's revoke: exit %d, stdout %q, stderr %q; want it waiting for approval (%d)", exit, stdout, stderr, exitPending)
	}
	if stdout, stderr, exit := grant("revoke", uuid.NewString()); exit != 1 || stdout != "" || !strings.Contains(stderr, "(NOT_FOUND)") {
		t.Fatalf("grant revoke of an unknown grant: exit %d, stdout %q, stderr %q; want 1 naming NOT_FOUND", exit, stdout, stderr)
	}
}

// TestMachineAndGrantCommandsRefusedInsideASession: a process inside a registered session's
// process tree gets no launcher proof from the helper, so `machine list`, `machine revoke` and
// `grant list` run from inside one say so and exit 1. `grant revoke` is no longer here: a session
// ends a grant it was given from inside the session (TestGrantRevokeInsideASessionEndsItsOwnGrant).
func TestMachineAndGrantCommandsRefusedInsideASession(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock := realHelper(t)
	for _, args := range [][]string{{"machine", "list"}, {"machine", "revoke", uuid.NewString()}, {"grant", "list"}} {
		code, stdout, stderr := runInSession(t, binary, sock, args...)
		prefix := "agent-secrets " + args[0] + " " + args[1] + ": IN_SESSION: pid "
		if code != 1 || stdout != "" || !strings.HasPrefix(stderr, prefix) || !strings.HasSuffix(stderr, "is inside a registered machine session, which acts on itself alone; run machine and grant commands from your own shell\n") {
			t.Fatalf("%v inside a session: exit %d, stdout %q, stderr %q; want 1 and IN_SESSION", args, code, stdout, stderr)
		}
	}
}

// TestMachineAndGrantCommandsOnAMachineNotLoggedIn: from the operator's own shell on a machine
// whose helper holds no machine login, every machine and grant command says to log it in.
func TestMachineAndGrantCommandsOnAMachineNotLoggedIn(t *testing.T) {
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, "http://127.0.0.1:1", "ada@example.com")
	for _, args := range [][]string{{"machine", "list"}, {"machine", "revoke", uuid.NewString()}, {"grant", "list"}, {"grant", "revoke", uuid.NewString()}} {
		stdout, stderr, exit := runAgentSecrets(t, binary, "http://127.0.0.1:1", t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, args...)
		want := "agent-secrets " + args[0] + " " + args[1] + ": this machine is not logged in to the secrets broker; run: agent-secrets machine login\n"
		if exit != 1 || stdout != "" || stderr != want {
			t.Fatalf("%v with no machine login: exit %d, stdout %q, stderr %q; want 1, %q", args, exit, stdout, stderr, want)
		}
	}
}

// grantSessionResult is what the in-session helper reports for one command it ran.
type grantSessionResult struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// TestGrantSessionHelper is the session root the tests below run commands inside: register --exec
// replaces it, so it and its children are one registered machine session, and every CLI command
// it runs as its own child resolves to that session (the real case a session ending its own grant
// is). It reads one JSON array of arguments per line from stdin, runs the built CLI with them,
// and writes one JSON grantSessionResult line per command to the results file, so the parent can
// read each command's outcome without racing the test binary's own stdout. The parent polls that
// file, so each write renames a finished file holding every line so far into place
// (writeFileAtomic): a line appended in place can be read before all of it is there.
func TestGrantSessionHelper(t *testing.T) {
	if os.Getenv("AGENT_SECRETS_GRANT_HELPER") == "" {
		t.Skip("subprocess helper")
	}
	binary := os.Getenv("AGENT_SECRETS_GRANT_BINARY")
	resultsPath := os.Getenv("AGENT_SECRETS_GRANT_RESULTS")
	in := bufio.NewScanner(os.Stdin)
	var results []byte
	for in.Scan() {
		var args []string
		if json.Unmarshal(in.Bytes(), &args) != nil {
			continue
		}
		cmd := exec.Command(binary, args...)
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		_ = cmd.Run()
		line, _ := json.Marshal(grantSessionResult{Code: cmd.ProcessState.ExitCode(), Stdout: so.String(), Stderr: se.String()})
		results = append(append(results, line...), '\n')
		_ = writeFileAtomic(resultsPath, results, 0o600)
	}
}

// grantSession is a live registered machine session a test runs CLI commands inside, one at a
// time, reading each command's outcome.
type grantSession struct {
	t           *testing.T
	stdin       io.WriteCloser
	resultsPath string
	seen        int
}

// startGrantSession launches `register --exec -- <this test binary> -test.run TestGrantSessionHelper`
// against the helper at sock and the broker at url, waiting for the session to enrol, so the
// helper's sign op answers OK for every command the session runs (the in-session identity R4's
// chain signs with).
func startGrantSession(t *testing.T, binary, sock, url string) *grantSession {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	results := filepath.Join(home, "results")
	if err := os.WriteFile(results, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "register", "--exec", "--wait", "5", "--", self, "-test.run", "^TestGrantSessionHelper$")
	cmd.Env = append(os.Environ(),
		"AGENT_SECRETS_GRANT_HELPER=1",
		"AGENT_SECRETS_GRANT_BINARY="+binary,
		"AGENT_SECRETS_GRANT_RESULTS="+results,
		"AGENT_SECRETS_HELPER_SOCK="+sock,
		"AGENT_SECRETS_URL="+url,
		"AGENT_SECRETS_KEY_DIR=",
		"XDG_RUNTIME_DIR="+t.TempDir(),
		"HOME="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = stdin.Close()
		testwait.Receive(t, "the session's process to exit once its stdin closed", waited)
	})
	return &grantSession{t: t, stdin: stdin, resultsPath: results}
}

// run runs one CLI command inside the session and returns its outcome, waiting for the helper to
// write the result line for it.
func (s *grantSession) run(args ...string) grantSessionResult {
	s.t.Helper()
	line, err := json.Marshal(args)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("sending %v to the session: %v", args, err)
	}
	s.seen++
	line = []byte(testwait.Until(s.t, fmt.Sprintf("the session to report a result for %v", args), func() (string, bool) {
		data, _ := os.ReadFile(s.resultsPath)
		if len(data) == 0 {
			return "", false
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) < s.seen {
			return "", false
		}
		return lines[s.seen-1], true
	}))
	var res grantSessionResult
	if err := json.Unmarshal(line, &res); err != nil {
		s.t.Fatalf("the session's result for %v was %q: %v", args, line, err)
	}
	return res
}

// requestAutoSecret asks for TEST_AUTO_SECRET from inside the session and returns the id of the
// grant it is given at once (the rig grants the operator's own agent-tier secret with no
// approval), the grant this session may then end.
func (s *grantSession) requestAutoSecret() string {
	s.t.Helper()
	res := s.run("request", "TEST_AUTO_SECRET", "--json")
	var granted RequestResult
	if err := json.Unmarshal([]byte(res.Stdout), &granted); res.Code != 0 || err != nil || granted.State != "granted" || granted.GrantID == nil {
		s.t.Fatalf("request TEST_AUTO_SECRET inside the session: exit %d, stdout %q, stderr %q; want it granted at once", res.Code, res.Stdout, res.Stderr)
	}
	return *granted.GrantID
}

// TestGrantRevokeInsideASessionEndsItsOwnGrant is this change's case: a session ends a
// grant it was given, with `grant revoke`, from inside the session — where before it answered
// IN_SESSION and the grant lived to its expiry.
func TestGrantRevokeInsideASessionEndsItsOwnGrant(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	grantOutside := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"grant"}, args...)...)
	}

	session := startGrantSession(t, binary, sock, rig.URL)
	grantID := session.requestAutoSecret()
	if stdout, _, exit := grantOutside("list"); exit != 0 || !strings.Contains(stdout, grantID) {
		t.Fatalf("grant list outside the session before the revoke: exit %d, stdout %q; want it to list %s", exit, stdout, grantID)
	}

	res := session.run("grant", "revoke", grantID)
	want := "revoked " + grantID + " at " + brokerSource(brokeraddr.New(rig.URL)).String() + "\n"
	if res.Code != 0 || res.Stdout != want || res.Stderr != "" {
		t.Fatalf("grant revoke inside the session: exit %d, stdout %q, stderr %q; want 0 and %q", res.Code, res.Stdout, res.Stderr, want)
	}
	if stdout, _, exit := grantOutside("list"); exit != 0 || strings.Contains(stdout, grantID) {
		t.Fatalf("grant list after the revoke: exit %d, stdout %q; want %s gone", exit, stdout, grantID)
	}
}

// TestGrantRevokeInsideASessionRefusesAnotherEnrollmentsGrant: the broker ends only a grant of the
// enrollment that signs, and two sessions the helper registered are two enrollments. So a second
// session's `grant revoke` of the first one's grant is refused NOT_YOURS: it prints that one line
// and nothing more, exits 1, and the grant stays live. Inside a registered session the helper
// refuses every launcher proof, so the grant would stay live even had the command fallen through
// to the operator route; the whole of stderr being that one line is what shows no other route
// printed a failure here. TestGrantRevokeTriesNoOtherRouteAfterNotYours holds the case where the
// helper would sign the operator's route.
func TestGrantRevokeInsideASessionRefusesAnotherEnrollmentsGrant(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	_, sock := serveRealHelper(t, rig.URL, rig.Operator)
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("machine login (code %s): exit %d, stderr %q", code, exit, stderr)
	}
	grantOutside := func(args ...string) (string, string, int) {
		t.Helper()
		return runAgentSecrets(t, binary, rig.URL, t.TempDir(), []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}, append([]string{"grant"}, args...)...)
	}

	holder := startGrantSession(t, binary, sock, rig.URL)
	grantID := holder.requestAutoSecret()

	other := startGrantSession(t, binary, sock, rig.URL)
	res := other.run("grant", "revoke", grantID)
	if want := notYoursStderr(grantID); res.Code != 1 || res.Stdout != "" || res.Stderr != want {
		t.Fatalf("another session's grant revoke: exit %d, stdout %q, stderr %q; want 1 and exactly %q", res.Code, res.Stdout, res.Stderr, want)
	}
	if stdout, _, exit := grantOutside("list"); exit != 0 || !strings.Contains(stdout, grantID) {
		t.Fatalf("grant list after the refused revoke: exit %d, stdout %q; want %s still live", exit, stdout, grantID)
	}
}
