//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/cipher/secret_gcp_linux_test.go
//
// A gcloud call that reaches its deadline kills every process left in gcloud's process group
// (runGcloud), a child that holds gcloud's output among them.
// These tests read /proc to see a stub gcloud's child running while the call runs and gone once
// the call has ended, through readProcStat, so they build where it does: Linux on amd64 or arm64.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/testwait"
)

// gcloudTestDeadline is the deadline on each store call (storeCallDeadline) these tests give a
// gcloud call: room for the stub gcloud's child to start and be seen running before it. A machine
// too loaded for that fails the test as unable to tell (callEnds), never passes it.
const gcloudTestDeadline = time.Second

// gcloudChildFile is the variable that names the file the stub gcloud's child writes its pid to
// (useGcloudWhoseChildHoldsItsOutput). The child inherits it, so it also marks the child's
// environment as this test's (ours).
const gcloudChildFile = "AGENT_SECRETS_TEST_GCLOUD_CHILD"

// gcloudChild is the child of the stub gcloud useGcloudWhoseChildHoldsItsOutput writes, by the
// file it writes its pid to.
type gcloudChild struct {
	pidFile string
}

// useGcloudWhoseChildHoldsItsOutput is a stub gcloud that never answers, whatever it is asked: it
// starts a child that holds its standard output open, as gcloud's Python does under a shell
// script that does not exec it, and waits on it. The child writes its own pid to a file before it
// sleeps, so it names itself even where the stub is killed first. A child still running when t
// ends is killed (cleanUp), and t fails where /proc cannot say whether it still runs.
func useGcloudWhoseChildHoldsItsOutput(t *testing.T) (gcloudCommand, gcloudChild) {
	t.Helper()
	return gcloudStubIn(t, t.TempDir(), "wait")
}

// gcloudStubIn writes useGcloudWhoseChildHoldsItsOutput's stub gcloud into dir, the file its
// child names itself in beside it. The stub ends with last once it has started the child: "wait"
// waits on the child, never answering; "exit 0" exits at once, leaving the child holding the
// output, as a wrapper that does not wait for what it started does.
func gcloudStubIn(t *testing.T, dir, last string) (gcloudCommand, gcloudChild) {
	t.Helper()
	child := gcloudChild{pidFile: filepath.Join(dir, "child.pid")}
	t.Setenv(gcloudChildFile, child.pidFile)
	script := "#!/bin/sh\n" +
		`/bin/sh -c 'echo $$ >"$` + gcloudChildFile + `"; exec sleep 3600' &` + "\n" +
		last + "\n"
	gcloud := gcloudCommand{path: filepath.Join(dir, "gcloud")}
	if err := os.WriteFile(gcloud.path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.cleanUp(t) })
	return gcloud, child
}

// cleanUp kills the child where it still runs (kill). Only a failed run finds no pid written,
// since callEnds sees the child named: a child that started late, once the stub was killed, still
// names itself, so it is waited for, and t fails where none comes.
func (c gcloudChild) cleanUp(t *testing.T) {
	t.Helper()
	pid := c.pid()
	if pid == 0 && !testwait.Holds(2*time.Second, testwait.Poll, func() bool { pid = c.pid(); return pid != 0 }) {
		t.Errorf("the stub gcloud's child never named itself in %s: where it started, it may still run", c.pidFile)
		return
	}
	c.kill(t, pid)
}

// pid is the pid the child wrote, 0 while it has written none.
func (c gcloudChild) pid() int {
	data, err := os.ReadFile(c.pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// runs reports whether pid names a running process: not where /proc no longer names it or names
// a dead one (Z, a zombie its reaper has not collected yet, or X). A read of /proc that fails in
// any other way is an error, never an answer that the process is gone. From the child's naming
// itself to the two seconds after the call, pid is the child's: Linux hands pids out in turn, so
// it gives one out again only once its allocator has gone round all the others.
func (c gcloudChild) runs(pid int) (bool, error) {
	stat, err := readProcStat(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stat.state != 'Z' && stat.state != 'X', nil
}

// ours reports whether pid's environment holds this child's pid file, so that no process the
// kernel has given the pid since is killed for the child. A process that is exiting can refuse a
// read of its environment for a moment (permission denied, seen once in twenty runs), so ours
// asks again for up to two seconds before it answers that error.
func (c gcloudChild) ours(pid int) (bool, error) {
	var answer bool
	var readErr error
	testwait.Holds(2*time.Second, testwait.Poll, func() bool {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
			answer, readErr = false, nil
		case err != nil:
			readErr = err
			return false
		default:
			answer, readErr = slices.Contains(strings.Split(string(environ), "\x00"), gcloudChildFile+"="+c.pidFile), nil
		}
		return true
	})
	return answer, readErr
}

// kill kills pid where it still runs and is the child (ours). The process handle is taken before
// the check, so where the kernel gives one (a pidfd) the signal reaches the process checked,
// never one the kernel has given the pid since.
func (c gcloudChild) kill(t *testing.T, pid int) {
	t.Helper()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Errorf("the stub gcloud's child, pid %d, may still run: %v", pid, err)
		return
	}
	defer process.Release()
	running, err := c.runs(pid)
	if err == nil && running {
		var mine bool
		if mine, err = c.ours(pid); err == nil && mine {
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill the stub gcloud's child, pid %d: %v", pid, err)
			}
		}
	}
	if err != nil {
		t.Errorf("the stub gcloud's child, pid %d, may still run: %v", pid, err)
	}
}

// callEnds is call's error. It fails t where call has not returned within 10s, ten times the
// deadline it runs under, and where the child was not seen running while call ran: then /proc
// does not show this test the child, and its absence afterwards would prove nothing. Where it
// fails t before call has returned, t ends by killing the child (cleanUp), which lets a call held
// by the child's open output return, and then waits up to 10s for call, failing where it has not
// returned by then.
func (c gcloudChild) callEnds(t *testing.T, what string, call func() error) error {
	t.Helper()
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		done <- call()
	}()
	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		c.cleanUp(t)
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Errorf("%s was still waiting on gcloud 10s after its test ended", what)
		}
	})
	giveUp := time.After(10 * time.Second)
	poll := time.NewTicker(testwait.Poll)
	defer poll.Stop()
	seen := false
	for {
		select {
		case err := <-done:
			if !seen {
				t.Fatalf("the stub gcloud's child was never seen running while %s ran, so this run cannot say whether the kill at the deadline reaches it", what)
			}
			return err
		case <-giveUp:
			t.Fatalf("%s was still waiting on gcloud 10s on, past the %s deadline", what, gcloudTestDeadline)
			return nil
		case <-poll.C:
			if pid := c.pid(); !seen && pid != 0 {
				running, err := c.runs(pid)
				if err != nil {
					t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, err)
				}
				seen = running
			}
		}
	}
}

// requireGone fails t unless the child, seen running while the call ran (callEnds), is gone
// within two seconds of the call's end: the kill at the deadline reached it, not the stub gcloud
// alone.
func (c gcloudChild) requireGone(t *testing.T) {
	t.Helper()
	pid := c.pid()
	var readErr error
	gone := testwait.Holds(2*time.Second, testwait.Poll, func() bool {
		running, err := c.runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, readErr)
	}
	if !gone {
		t.Fatalf("the stub gcloud's child, pid %d, still runs 2s after the call that started it ended", pid)
	}
}

// TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild: a refresh of an expired gcloud token
// (gcloudToken.Token, `gcloud auth print-access-token`) that gcloud never answers ends at the
// deadline on each store call (storeCallDeadline), naming the account and the deadline, and the
// child that held gcloud's output is gone with it, not left running. The deadline and its error
// alone, on every platform, are TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's.
func TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	useStoreCallDeadline(t, gcloudTestDeadline)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcloudToken{ctx: context.Background(), gcloud: gcloud, account: "ada@example.com"})
	err := child.callEnds(t, "the refresh of an expired token", func() error {
		_, err := source.Token()
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "did not answer within 1s") {
		t.Fatalf("Token() = %v; want a failure naming the account and the 1s deadline", err)
	}
	child.requireGone(t)
}

// TestGcloudLoginListAtItsDeadlineKillsGcloudsChild: `gcloud auth list` (gcloudLogins) runs under
// storeCallDeadline as the token's refresh does, so a gcloud that never answers ends at that
// deadline, naming it, and the child that held gcloud's output is gone with it, not left running.
func TestGcloudLoginListAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	useStoreCallDeadline(t, gcloudTestDeadline)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	err := child.callEnds(t, "`gcloud auth list`", func() error {
		_, _, err := gcloudLogins(context.Background(), gcloud)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "`gcloud auth list` did not answer within 1s") {
		t.Fatalf("gcloudLogins() = %v; want a failure naming the 1s deadline", err)
	}
	child.requireGone(t)
}

// gcloudSignalAt is the variable that makes TestGcloudSignInSignalHelper interrupt itself at one
// moment of its gcloud call (onGcloudStep): gcloudStarted or gcloudWaited.
const gcloudSignalAt = "AGENT_SECRETS_GCLOUD_SIGNAL_AT"

// TestGcloudSignInSignalHelper is the process TestGcloudSignInEndedByASignalKillsGcloudsChild
// signals: `secret list` against a fake broker serving the GCP store, with no other source of a
// Google credential, so its sign-in runs `gcloud auth list` through the stub gcloud of
// gcloudStubIn, written beside the pid file AGENT_SECRETS_GCLOUD_SIGNAL_HELPER names, which never
// answers. The deadline on each store call stays 60s, so the signal comes while the call runs. It
// prints RETURNED <exit code> should the form return. With gcloudSignalAt set, it sends itself
// SIGINT from onGcloudStep at that step, once the stub's child has named itself; at gcloudWaited
// the stub exits at once, leaving its child holding its output
// (TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild). At gcloudStarted the hook
// then returns, since runRecorded holds runningGcloud's lock there and the signal's handler
// waits on it. At gcloudWaited it never returns, so the kill of what gcloud left
// (killGroupLeft), which comes next, cannot reach the child while a process the signal's default
// action ends is still ending.
func TestGcloudSignInSignalHelper(t *testing.T) {
	pidFile := os.Getenv("AGENT_SECRETS_GCLOUD_SIGNAL_HELPER")
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	at := os.Getenv(gcloudSignalAt)
	last := "wait"
	if at == gcloudWaited {
		last = "exit 0"
	}
	gcloud, child := gcloudStubIn(t, filepath.Dir(pidFile), last)
	if at != "" {
		restore := onGcloudStep
		t.Cleanup(func() { onGcloudStep = restore })
		onGcloudStep = func(step string) {
			if step != at {
				return
			}
			for child.pid() == 0 {
				time.Sleep(testwait.Poll)
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
			if step == gcloudWaited {
				select {}
			}
		}
	}
	t.Setenv("PATH", filepath.Dir(gcloud.path)+":/usr/bin:/bin")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	_, stderr, code := runSecret("list")
	fmt.Println("RETURNED", code, stderr)
}

// signInHelper is a run of TestGcloudSignInSignalHelper (startSignInHelper): its process, what it
// printed, and a channel closed once it has been waited for.
type signInHelper struct {
	cmd    *exec.Cmd
	out    *bytes.Buffer
	waited chan struct{}
}

// startSignInHelper starts TestGcloudSignInSignalHelper, its stub gcloud's child naming itself in
// child's pid file, with env added to its environment. It runs under `env --default-signal`, so
// it does not inherit a SIGINT this test process ignores (as a job run in the background of a
// script does), at which agent-secrets rightly goes on running. A helper still running when t
// ends is killed.
func startSignInHelper(t *testing.T, child gcloudChild, env ...string) *signInHelper {
	t.Helper()
	cmd := exec.Command("env", "--default-signal=INT", os.Args[0], "-test.run=^TestGcloudSignInSignalHelper$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_GCLOUD_SIGNAL_HELPER="+child.pidFile), env...)
	h := &signInHelper{cmd: cmd, out: &bytes.Buffer{}, waited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = h.out, h.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = cmd.Wait()
		close(h.waited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		h.ended(10 * time.Second)
	})
	return h
}

// ended reports whether the helper has been waited for within within.
func (h *signInHelper) ended(within time.Duration) bool {
	select {
	case <-h.waited:
		return true
	case <-time.After(within):
		return false
	}
}

// requireEndedByInterrupt fails t unless the helper, waited for already, ended as an interrupt
// ends it: by SIGINT, a shell's status 130.
func (h *signInHelper) requireEndedByInterrupt(t *testing.T) {
	t.Helper()
	status := h.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !(status.Signaled() && status.Signal() == syscall.SIGINT) && status.ExitStatus() != 130 {
		t.Fatalf("agent-secrets ended %v, saying %q; want it ended by its interrupt (SIGINT, a shell's 130)", h.cmd.ProcessState, h.out.String())
	}
}

// TestGcloudSignInEndedByASignalKillsGcloudsChild: a sign-in whose gcloud never answers, ended by
// an interrupt sent to agent-secrets alone (gcloud, in a group of its own, gets no signal sent to
// agent-secrets' group, a terminal's Ctrl-C included), ends as the interrupt ends it - by SIGINT,
// a shell's status 130 - and the child that held gcloud's output is gone with it.
func TestGcloudSignInEndedByASignalKillsGcloudsChild(t *testing.T) {
	child := gcloudChild{pidFile: filepath.Join(t.TempDir(), "child.pid")}
	t.Cleanup(func() { child.cleanUp(t) })
	helper := startSignInHelper(t, child)
	seen := testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		pid := child.pid()
		if pid == 0 {
			return false
		}
		running, err := child.runs(pid)
		return err == nil && running
	})
	if !seen {
		_ = helper.cmd.Process.Kill()
		helper.ended(10 * time.Second)
		t.Fatalf("the stub gcloud's child was never seen running; the helper said %q", helper.out.String())
	}
	if err := helper.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if !helper.ended(10 * time.Second) {
		_ = helper.cmd.Process.Kill()
		helper.ended(10 * time.Second)
		t.Fatalf("agent-secrets was still running 10s after its interrupt; it said %q", helper.out.String())
	}
	helper.requireEndedByInterrupt(t)
	child.requireGone(t)
}

// TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild: an interrupt that comes at
// either edge of a gcloud call still ends agent-secrets by SIGINT and kills gcloud's group, the
// child that holds gcloud's output among them: the moment gcloud has started, before
// agent-secrets has recorded it (gcloudStarted), and the moment a gcloud that exited leaving that
// child has been waited for, before the child is killed (gcloudWaited). The helper sends the
// interrupt to itself from onGcloudStep once the stub's child has named itself, so it lands in
// that moment on every run, whatever the machine's load.
func TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild(t *testing.T) {
	for _, step := range []string{gcloudStarted, gcloudWaited} {
		t.Run(step, func(t *testing.T) {
			child := gcloudChild{pidFile: filepath.Join(t.TempDir(), "child.pid")}
			t.Cleanup(func() { child.cleanUp(t) })
			helper := startSignInHelper(t, child, gcloudSignalAt+"="+step)
			testwait.Receive(t, "agent-secrets' end by the interrupt it sent itself once gcloud had "+step, helper.waited)
			helper.requireEndedByInterrupt(t)
			if child.pid() == 0 {
				t.Fatalf("the stub gcloud's child never named itself, so no interrupt came from onGcloudStep; the helper said %q", helper.out.String())
			}
			child.requireGone(t)
		})
	}
}

// TestGcloudThatExitsLeavingAChildTakesTheChildWithIt: a gcloud that exits by itself and leaves
// a child holding its output, which gcloudWaitDelay stops the wait for but does not kill, has
// that child killed once runGcloud has waited for gcloud, whatever the call's outcome.
func TestGcloudThatExitsLeavingAChildTakesTheChildWithIt(t *testing.T) {
	gcloud, child := gcloudStubIn(t, t.TempDir(), "exit 0")
	_ = child.callEnds(t, "a gcloud that exited leaving its child", func() error {
		_, err := runGcloud(context.Background(), gcloud, "auth", "list", "--format=json")
		return err
	})
	child.requireGone(t)
}

// TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild: `gcloud config list`
// (readGcloudSettings) runs under gcloudSettingsDeadline, 5s, so a gcloud that never answers
// ends at that deadline as a failed read, and the child that held gcloud's output is gone with
// it, not left running.
func TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	err := child.callEnds(t, "`gcloud config list`", func() error {
		_, err := readGcloudSettings(context.Background(), gcloud)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "`gcloud config list` failed") {
		t.Fatalf("readGcloudSettings() = %v; want the read failed", err)
	}
	child.requireGone(t)
}

// TestGcloudCancelOfAGroupGoneAlreadyAnswersProcessDone: the kill at the end of a gcloud's
// context (killGroupOnCancel) answers os.ErrProcessDone where the kernel says the group is gone
// already (ESRCH), as exec.Cmd's own kill does for a process that is done, so a gcloud that ended
// by itself keeps its own exit status; any other answer is returned as it came. signalGroup is
// replaced, so no signal reaches any process: a real kill of a reaped gcloud's pid could reach
// another process given that pid since. The live tests above drive the real kill.
func TestGcloudCancelOfAGroupGoneAlreadyAnswersProcessDone(t *testing.T) {
	restore := signalGroup
	t.Cleanup(func() { signalGroup = restore })
	for _, row := range []struct{ answer, want error }{
		{syscall.ESRCH, os.ErrProcessDone},
		{syscall.EPERM, syscall.EPERM},
		{nil, nil},
	} {
		var sent []string
		signalGroup = func(pgid int, sig syscall.Signal) error {
			sent = append(sent, fmt.Sprintf("%d %v", pgid, sig))
			return row.answer
		}
		cmd := exec.CommandContext(context.Background(), "gcloud")
		killGroupOnCancel(cmd)
		cmd.Process = &os.Process{Pid: 4242}
		if err := cmd.Cancel(); !errors.Is(err, row.want) || (row.want == nil && err != nil) {
			t.Errorf("Cancel() with the kernel answering %v = %v; want %v", row.answer, err, row.want)
		}
		if want := []string{"4242 " + syscall.SIGKILL.String()}; !slices.Equal(sent, want) {
			t.Errorf("Cancel() sent %q; want %q, the group's SIGKILL alone", sent, want)
		}
	}
}
