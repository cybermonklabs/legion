// packages/envoy/cmd/cipher/brokercall_linux_test.go
//go:build linux

// `agent-secrets enroll --helper` and `unenroll --helper` when the outcome of the broker call is
// not known: the helper's op ran past its bound, or the helper gave no answer at all, as a helper
// from before the bound did. And the one failure after the broker answered OK: the enrollment
// file cannot be written. Linux-only for the real helper's pidfd pinning.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown: the broker makes the enrollment
// at once, and its answer would reach the helper an hour later, past the op's bound. `enroll
// --helper` exits 1 naming ENROLL_OUTCOME_UNKNOWN and saying the outcome is unknown, and writes
// no enrollment file, while the broker holds the row. The helper's two clocks are fakes the row
// moves. On the broker's, the helper's first poll of the machine login waits exactly the device
// authorization's 5 s interval. On the op's, the enroll arms its bound at exactly 20 s and the
// helper's timer at 21 s, gives no answer before the row moves that clock, and answers once the
// row moves it to the bound alone. The answer is the one the bound's end of the request gives
// (requestReach.settle): its cause says the request may have reached the broker, not the cause
// the helper's timer would give. The same command again, answered at once, exits 0 with that
// row's id, and the broker still holds that one live row.
func TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling (main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.serveRealHelperWith, (*Server).Serve, (*Server).handle, (*Server).unenrollSessionKey, (*Broker).Revoke, (*Broker).signingAuthority, (*SignInAuthority).Current, (*Client).Token, (*Client).tradeHeld, (*Client).trade, (*ExternalAccountIdentity).Token, (gcloudToken).Token, main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516).
	// The row's own durations, never read from the code under test: the device authorization's
	// poll interval (RFC 8628's 5 s, which the rig's broker sends), the helper's bound on one
	// broker-call op, and how long after the bound the helper's timer answers.
	const (
		pollWait = 5 * time.Second
		opBound  = 20 * time.Second
		opSlack  = time.Second
	)
	start := time.Date(2026, time.November, 28, 9, 0, 0, 0, time.UTC)
	opFake, loginFake := clocktest.New(start), clocktest.New(start)
	rig := ciphertest.NewRig(t)
	binary := buildAgentSecrets(t)
	slow := &slowEnrollments{base: http.DefaultTransport, hold: time.Hour}
	_, sock := serveRealHelperWith(t, rig.URL, rig.Operator, &http.Client{Transport: slow}, func(srv *helper.Server) {
		srv.Clock = opFake
		srv.Broker.Clock = loginFake
	})
	// Nothing moved loginFake before the approval, so the helper's first poll still waits on it:
	// no poll reached the broker before the code was approved, and no slow_down lengthened the
	// interval. One move to the interval is one poll, which the approved code answers.
	firstPoll := func() {
		armed := loginFake.AwaitArmed(t, "the helper's first poll of the machine login to arm its wait", 1)
		if want := start.Add(pollWait); len(armed) != 1 || !armed[0].Equal(want) {
			t.Fatalf("the helper's first poll of the machine login waits on its clock until %v; want only %s (%s after %s)", armed, want, pollWait, start)
		}
		loginFake.Advance(pollWait)
	}
	if code, exit, stderr := machineLoginAround(t, rig, binary, sock, "approve", nil, firstPoll); exit != 0 {
		t.Fatalf("approved machine login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	if status, err := helper.CallContext(testwait.Context(t), sock, helper.Request{Op: "login-status"}); err != nil || status.LoginState != "issued" {
		t.Fatalf("login-status after the approved login: %+v %v; want issued", status, err)
	}
	runtimeID := "past-bound-box-" + t.Name()
	keyDir := filepath.Join(t.TempDir(), "keys")
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	args := []string{"enroll", "--helper", "--runtime-id", runtimeID, "--thumbprint", freshThumbprint(t)}

	slow.holds.Store(1)
	cmd := exec.Command(binary, args...)
	cmd.Env = append(append(append(os.Environ(), hostIsolation(t)...), "AGENT_SECRETS_URL="+rig.URL, "AGENT_SECRETS_KEY_DIR="+keyDir), env...)
	c := startCall(t, cmd)
	// brokerCall arms the op's bound before it starts the op and its timer after, so the transport
	// can hold the answer before the timer is armed. The machine login's op left neither: brokerCall
	// ends the one and stops the other as it returns.
	type opState struct {
		Held  int64
		Armed []time.Time
	}
	// requireNoAnswer fails the row once enroll --helper has exited: no answer may come before the
	// row moves the op's clock.
	requireNoAnswer := func(state opState) {
		if c.ended() {
			t.Fatalf("enroll --helper exited before the row moved the helper's op to its bound (the op's clock holds %+v): exit %d, stdout %q, stderr %q", state, cmd.ProcessState.ExitCode(), c.stdout.String(), c.stderr.String())
		}
	}
	held := testwait.Until(t, "the transport to hold the enrollment's answer while the helper's op arms its bound and its timer", func() (opState, bool) {
		state := opState{Held: slow.held.Load(), Armed: opFake.Armed()}
		requireNoAnswer(state)
		return state, state.Held == 1 && len(state.Armed) == 2
	})
	requireNoAnswer(held)
	now := opFake.Now()
	if want := []time.Time{now.Add(opBound), now.Add(opBound + opSlack)}; !slices.EqualFunc(held.Armed, want, time.Time.Equal) {
		t.Fatalf("while the transport holds the enrollment's answer the helper's op has armed %v on its clock; want only %v, its %s bound and its timer %s after that", held.Armed, want, opBound, opSlack)
	}
	opFake.Advance(opBound)
	exit := c.end(t)
	var exitErr *exec.ExitError
	if c.err != nil && !errors.As(c.err, &exitErr) {
		t.Fatalf("run enroll --helper: %v\nstdout: %s\nstderr: %s", c.err, c.stdout.String(), c.stderr.String())
	}
	stdout, stderr := c.stdout.String(), c.stderr.String()
	rows := enrollmentsOf(t, rig, runtimeID)
	outcome := fmt.Sprintf("exit %d, stdout %q, stderr %q; the broker's rows for %s: %+v", exit, stdout, stderr, runtimeID, rows)
	if n := slow.held.Load(); n != 1 {
		t.Fatalf("the transport held %d answers of POST /v1/enrollments, want 1: %s", n, outcome)
	}
	if exit != 1 || stdout != "" || !strings.Contains(stderr, helper.CodeEnrollUnknown) || !strings.Contains(stderr, "the outcome is unknown") {
		t.Fatalf("enroll --helper past the helper's bound: %s; want exit 1 naming %s and saying the outcome is unknown", outcome, helper.CodeEnrollUnknown)
	}
	// settle's cause, which the helper's mayHaveReachedCause gives; the timer's says "did not end in".
	const settleCause = "the request may have reached the broker, but the helper did not get its result ("
	if !strings.Contains(stderr, settleCause) || strings.Contains(stderr, "did not end in") {
		t.Errorf("enroll --helper past the helper's bound: %s; want settle's cause %q, not the timer's \"did not end in\"", outcome, settleCause)
	}
	if _, err := os.Stat(filepath.Join(keyDir, "enrollment")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("enroll --helper past the helper's bound wrote an enrollment file (%v): %s", err, outcome)
	}
	live := liveIDs(rows)
	if len(live) != 1 {
		t.Fatalf("enroll --helper past the helper's bound: %s; want the one live row the broker made", outcome)
	}

	stdout, stderr, exit = runAgentSecrets(t, binary, rig.URL, keyDir, env, args...)
	rows = enrollmentsOf(t, rig, runtimeID)
	outcome = fmt.Sprintf("exit %d, stdout %q, stderr %q; the broker's rows for %s: %+v", exit, stdout, stderr, runtimeID, rows)
	if exit != 0 || stdout != live[0]+"\n" {
		t.Fatalf("the same enroll --helper again: %s; want exit 0 with %s, the row the first made", outcome, live[0])
	}
	if now := liveIDs(rows); len(now) != 1 || now[0] != live[0] {
		t.Fatalf("the same enroll --helper again: %s; want exactly one live row, %s", outcome, live[0])
	}
}

// hangUpHelper listens at a socket of its own and, for each connection, reads the request line
// and hangs up with no answer, as a helper from before the broker-call bound did when the broker
// answered after its 5 s deadline: its write of the answer failed, and it closed the connection
// without one. silentHelperAt is another fake: it keeps the connection open.
func hangUpHelper(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadBytes('\n')
			conn.Close()
		}
	}()
	return sock
}

// TestEnrollAndUnenrollSayTheOutcomeIsUnknownWhenTheHelperGivesNoAnswer: a helper that takes the
// request and closes without an answer leaves the client not knowing what the broker did. `enroll
// --helper` exits 1 saying it is not known if the broker made the enrollment, and `unenroll
// --helper` exits 1 saying it is not known if the broker revoked it.
func TestEnrollAndUnenrollSayTheOutcomeIsUnknownWhenTheHelperGivesNoAnswer(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + hangUpHelper(t)}
	keyDir := filepath.Join(t.TempDir(), "keys")

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, env, "enroll", "--helper", "--runtime-id", "box-12", "--thumbprint", "tp-12")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "not known if the broker made the enrollment") {
		t.Fatalf("enroll --helper with no answer from the helper: exit %d, stdout %q, stderr %q; want 1 saying it is not known if the broker made the enrollment", exit, stdout, stderr)
	}
	stdout, stderr, exit = runAgentSecrets(t, binary, "http://unused", keyDir, env, "unenroll", "--helper", "--enrollment", "e")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "not known if the broker revoked the enrollment") {
		t.Fatalf("unenroll --helper with no answer from the helper: exit %d, stdout %q, stderr %q; want 1 saying it is not known if the broker revoked the enrollment", exit, stdout, stderr)
	}
}

// TestEnrollNamesTheEnrollmentWhenItCannotRecordIt: the helper answers that the broker made
// enr-13, and the key directory's enrollment path is a directory, so the command cannot record
// it. `enroll --helper` exits 1 naming enr-13 and the key directory by its variable - never its
// path, since it holds key.pem - and prints no id.
func TestEnrollNamesTheEnrollmentWhenItCannotRecordIt(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-13", LeaseExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	keyDir := t.TempDir()
	path := filepath.Join(keyDir, "enrollment")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--runtime-id", "box-13", "--thumbprint", "tp-13")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "enr-13") || !strings.Contains(stderr, "AGENT_SECRETS_KEY_DIR") || strings.Contains(stderr, keyDir) {
		t.Fatalf("enroll --helper with the enrollment path a directory: exit %d, stdout %q, stderr %q; want 1 naming enr-13 and AGENT_SECRETS_KEY_DIR, not %s", exit, stdout, stderr, keyDir)
	}
}

// TestEnrollMakesItsKeyDirectoryBeforeItAsksTheHelper holds plan D4: `enroll --helper` makes
// AGENT_SECRETS_KEY_DIR before it asks the helper, so a key directory it cannot make fails before
// the broker could enroll. The key dir is a path under an existing regular file, so MkdirAll cannot
// make it; the command exits 1 naming AGENT_SECRETS_KEY_DIR and not the path (the directory holds
// key.pem), and the fake helper — which would answer OK — receives no request. If the order went
// back, a key dir that cannot be made would exit 1 after the broker enrolled, with no id: an R2
// violation.
func TestEnrollMakesItsKeyDirectoryBeforeItAsksTheHelper(t *testing.T) {
	t.Parallel()
	binary := buildAgentSecrets(t)
	sock, reqs := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-should-not-happen",
		LeaseExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(file, "keys") // under a regular file, so MkdirAll cannot make it

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--runtime-id", "box-d4", "--thumbprint", "tp-d4")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "AGENT_SECRETS_KEY_DIR") || strings.Contains(stderr, file) {
		t.Fatalf("enroll --helper with a key dir that cannot be made: exit %d, stdout %q, stderr %q; want 1 naming AGENT_SECRETS_KEY_DIR, not %s", exit, stdout, stderr, file)
	}
	if n := len(reqs); n != 0 {
		t.Fatalf("the fake helper received %d requests; want none, since the key dir must be made before the helper is asked", n)
	}
}
