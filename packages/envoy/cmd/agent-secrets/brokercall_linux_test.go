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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/helper"
)

// TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown: the broker makes the enrollment
// at once, and its answer would reach the helper 5 s later, past the op's 2 s bound. `enroll
// --helper` exits 1 naming ENROLL_OUTCOME_UNKNOWN and saying the outcome is unknown, and writes
// no enrollment file, while the broker holds the row. The same command again, answered at once,
// exits 0 with that row's id, and the broker still holds that one live row.
func TestEnrollThroughTheHelperPastTheBoundSaysTheOutcomeIsUnknown(t *testing.T) {
	rig := brokertest.NewRig(t)
	binary := buildAgentSecrets(t)
	slow := &slowEnrollments{base: http.DefaultTransport, hold: 5 * time.Second}
	srv, sock := serveRealHelperWith(t, rig.URL, rig.Operator, &http.Client{Transport: slow})
	if code, exit, stderr := machineLoginThen(t, rig, binary, sock, "approve", nil); exit != 0 {
		t.Fatalf("approved machine login (code %s): exit %d, stderr %q; want 0", code, exit, stderr)
	}
	// The helper reads the bound at each op. The enroll comes from another process, so this
	// process's own call on the socket is what orders the write before the helper reads it.
	srv.BrokerCallBound = 2 * time.Second
	if status, err := helper.Call(sock, helper.Request{Op: "login-status"}, 10*time.Second); err != nil || status.LoginState != "issued" {
		t.Fatalf("login-status after the approved login: %+v %v; want issued", status, err)
	}
	runtimeID := "past-bound-box-" + t.Name()
	keyDir := filepath.Join(t.TempDir(), "keys")
	env := []string{"AGENT_SECRETS_HELPER_SOCK=" + sock}
	args := []string{"enroll", "--helper", "--runtime-id", runtimeID, "--thumbprint", freshThumbprint(t)}

	slow.holds.Store(1)
	stdout, stderr, exit := runAgentSecrets(t, binary, rig.URL, keyDir, env, args...)
	rows := enrollmentsOf(t, rig, runtimeID)
	outcome := fmt.Sprintf("exit %d, stdout %q, stderr %q; the broker's rows for %s: %+v", exit, stdout, stderr, runtimeID, rows)
	if n := slow.held.Load(); n != 1 {
		t.Fatalf("the transport held %d answers of POST /v1/enrollments, want 1: %s", n, outcome)
	}
	if exit != 1 || stdout != "" || !strings.Contains(stderr, helper.CodeEnrollUnknown) || !strings.Contains(stderr, "the outcome is unknown") {
		t.Fatalf("enroll --helper past the helper's bound: %s; want exit 1 naming %s and saying the outcome is unknown", outcome, helper.CodeEnrollUnknown)
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
// and hangs up with no answer, as a helper from before the broker-call bound did when its 5 s
// deadline passed while its broker call went on. silentHelperAt is another fake: it keeps the
// connection open.
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
