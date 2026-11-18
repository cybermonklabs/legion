// packages/envoy/cmd/agent-secrets/brokercall_linux_test.go
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

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown: the broker makes the enrollment
// at once, and the helper's transport holds its answer until the op's bound ends the request.
// `enroll --helper` exits 1 naming ENROLL_OUTCOME_UNKNOWN and saying the outcome is unknown, and
// writes no enrollment file, while the broker holds the row. The answer is the one the bound's end
// of the request gives (requestReach.settle): its cause says the request may have reached the
// broker, and it comes once the helper's clock reaches the bound, while the helper's timer, a
// second later on that clock, has not fired and so cannot answer with a cause of its own. The same
// command again, answered at once, exits 0 with that row's id, and the broker still holds that one
// live row.
func TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	slow := &slowEnrollments{base: http.DefaultTransport}
	srv, sock := serveRealHelperWith(t, rig.URL, rig.Operator, &http.Client{Transport: slow})
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved machine login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	// The helper reads its clock at each op. The enroll comes from another process, so this
	// process's own call on the socket is what orders the write before the helper reads it.
	clock := clocktest.New(time.Now())
	srv.Clock = clock
	if status, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second); err != nil || status.LoginState != "issued" {
		t.Fatalf("login-status after the approved login: %+v %v; want issued", status, err)
	}
	runtimeID := "past-bound-box-" + t.Name()
	keyDir := filepath.Join(t.TempDir(), "keys")
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	args := []string{"enroll", "--helper", "--runtime-id", runtimeID, "--thumbprint", freshThumbprint(t)}

	slow.holds.Store(1)
	cmd := exec.Command(binary, args...)
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_URL="+rig.URL, "AGENT_SECRETS_KEY_DIR="+keyDir), env...)
	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &stdoutBuf, &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent-secrets %v: %v", args, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The op arms its bound and the timer that answers it a second later, both on the helper's
	// clock, and nothing answers it before that clock moves.
	now := clock.Now()
	want := []time.Time{now.Add(helper.BrokerCallBound), now.Add(helper.BrokerCallBound + time.Second)}
	if armed := clock.AwaitArmed(t, "the enroll's bound and the helper's timer", 2); !slices.EqualFunc(armed, want, time.Time.Equal) {
		t.Fatalf("the helper armed %v for the enroll; want its bound at %v and its timer at %v", armed, want[0], want[1])
	}
	// The broker has made the row and answered, and the transport holds that answer.
	testwait.Eventually(t, "the transport to hold the broker's answer to POST /v1/enrollments", func() bool { return slow.held.Load() == 1 })
	select {
	case err := <-exited:
		t.Fatalf("enroll --helper ended (%v) before the helper's clock reached its bound: stdout %q, stderr %q", err, stdoutBuf.String(), stderrBuf.String())
	default:
	}
	clock.Advance(helper.BrokerCallBound)
	err := testwait.Receive(t, "enroll --helper to exit once the helper's clock reached its bound", exited)
	exit := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		exit = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run agent-secrets %v: %v\nstdout: %s\nstderr: %s", args, err, stdoutBuf.String(), stderrBuf.String())
	}
	stdout, stderr := stdoutBuf.String(), stderrBuf.String()
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
// it. `enroll --helper` exits 1 naming enr-13 and the path it could not write, and prints no id.
func TestEnrollNamesTheEnrollmentWhenItCannotRecordIt(t *testing.T) {
	binary := buildAgentSecrets(t)
	sock, _ := fakeHelper(t, helper.Response{OK: true, EnrollmentID: "enr-13", LeaseExpires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	keyDir := t.TempDir()
	path := filepath.Join(keyDir, "enrollment")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, exit := runAgentSecrets(t, binary, "http://unused", keyDir, []string{"AGENT_SECRETS_HELPER_SOCK=" + sock},
		"enroll", "--helper", "--runtime-id", "box-13", "--thumbprint", "tp-13")
	if exit != 1 || stdout != "" || !strings.Contains(stderr, "enr-13") || !strings.Contains(stderr, path) {
		t.Fatalf("enroll --helper with the enrollment path a directory: exit %d, stdout %q, stderr %q; want 1 naming enr-13 and %s", exit, stdout, stderr, path)
	}
}

// TestEnrollMakesItsKeyDirectoryBeforeItAsksTheHelper holds plan D4: `enroll --helper` makes
// AGENT_SECRETS_KEY_DIR before it asks the helper, so a key directory it cannot make fails before
// the broker could enroll. The key dir is a path under an existing regular file, so MkdirAll cannot
// make it and names the file; the command exits 1 naming that path, and the fake helper — which
// would answer OK — receives no request. If the order went back, a key dir that cannot be made
// would exit 1 after the broker enrolled, with no id: an R2 violation.
func TestEnrollMakesItsKeyDirectoryBeforeItAsksTheHelper(t *testing.T) {
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
	if exit != 1 || stdout != "" || !strings.Contains(stderr, file) {
		t.Fatalf("enroll --helper with a key dir that cannot be made: exit %d, stdout %q, stderr %q; want 1 naming %s", exit, stdout, stderr, file)
	}
	if n := len(reqs); n != 0 {
		t.Fatalf("the fake helper received %d requests; want none, since the key dir must be made before the helper is asked", n)
	}
}
