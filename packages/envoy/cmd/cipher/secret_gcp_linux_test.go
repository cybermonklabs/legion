//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/cipher/secret_gcp_linux_test.go
//
// A gcloud call that reaches its deadline kills every process left in gcloud's process group
// (runGcloud), a child that holds gcloud's output among them.
// These tests read /proc to see a stub gcloud's child running while the call runs and gone once
// the call has ended, through readProcStat, so they build where it does: Linux on amd64 or arm64.
// No call here ends before its child has been seen running: each deadline is one the test passes
// once it has seen the child (cuedDeadline), and each other end waits on the test's cue too.
package main

import (
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
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/testwait"
)

// cuedDeadline is a context whose deadline passes when the test passes it, not at a time: the
// deadline of a gcloud call these tests give, passed once the stub gcloud's child has been seen
// running (callEnds), so the child has started before the deadline however loaded the machine.
// A call under it reads its end as a deadline (context.DeadlineExceeded), as it reads its own.
type cuedDeadline struct {
	context.Context
	done chan struct{}
	pass func()
}

func newCuedDeadline() *cuedDeadline {
	d := &cuedDeadline{Context: context.Background(), done: make(chan struct{})}
	d.pass = sync.OnceFunc(func() { close(d.done) })
	return d
}

func (d *cuedDeadline) Done() <-chan struct{} { return d.done }

func (d *cuedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// awaitCue lengthens the deadline on each store call (storeCallDeadline) and gcloud's settings
// read (gcloudSettingsDeadline) to a day for the rest of t, and answers the deadline the test
// passes instead, so only the test's cue ends a call these tests give.
func awaitCue(t *testing.T) *cuedDeadline {
	t.Helper()
	useStoreCallDeadline(t, 24*time.Hour)
	restore := gcloudSettingsDeadline
	gcloudSettingsDeadline = 24 * time.Hour
	t.Cleanup(func() { gcloudSettingsDeadline = restore })
	return newCuedDeadline()
}

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
	if pid == 0 && !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool { pid = c.pid(); return pid != 0 }) {
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
// asks again, within testwait's shared bound, before it answers that error.
func (c gcloudChild) ours(t *testing.T, pid int) (bool, error) {
	t.Helper()
	var answer bool
	var readErr error
	testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
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
		if mine, err = c.ours(t, pid); err == nil && mine {
			if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill the stub gcloud's child, pid %d: %v", pid, err)
			}
		}
	}
	if err != nil {
		t.Errorf("the stub gcloud's child, pid %d, may still run: %v", pid, err)
	}
}

// callEnds is call's error. call runs until cue, which callEnds calls once it has seen the child
// running: the deadline the test passes (cuedDeadline), or the go-ahead a stub gcloud waits on
// before it exits. So the child has started, and been seen running, before anything can end the
// call, however loaded the machine. A call that ended before the child was seen fails t: then
// /proc does not show this test the child, and its absence afterwards would prove nothing. Where
// t fails before call has returned, t ends by cueing the end and killing the child (cleanUp),
// which lets a call held by the child's open output return, and then waits for call within
// testwait's shared bound, failing where it has not returned by then. A call that never returns
// fails t at that bound too.
func (c gcloudChild) callEnds(t *testing.T, what string, cue func(), call func() error) error {
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
		cue()
		c.cleanUp(t)
		select {
		case <-finished:
		case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
			t.Errorf("%s was still waiting on gcloud after its test ended", what)
		}
	})
	giveUp := time.After(testwait.Bounded(t, time.Minute, time.Second))
	poll := time.NewTicker(testwait.Poll)
	defer poll.Stop()
	seen := false
	for {
		select {
		case err := <-done:
			if !seen {
				t.Fatalf("the stub gcloud's child was never seen running while %s ran, so this run cannot say whether the kill at its end reaches it", what)
			}
			return err
		case <-giveUp:
			t.Fatalf("%s was still waiting on gcloud (child seen running: %t)", what, seen)
			return nil
		case <-poll.C:
			if pid := c.pid(); !seen && pid != 0 {
				running, err := c.runs(pid)
				if err != nil {
					t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, err)
				}
				if seen = running; seen {
					cue()
				}
			}
		}
	}
}

// requireGone fails t unless the child, seen running while the call ran (callEnds), is gone once
// the call has ended, within testwait's shared bound: the kill at the call's end reached it, not
// the stub gcloud alone.
func (c gcloudChild) requireGone(t *testing.T) {
	t.Helper()
	pid := c.pid()
	var readErr error
	gone := testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
		running, err := c.runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, readErr)
	}
	if !gone {
		t.Fatalf("the stub gcloud's child, pid %d, still runs after the call that started it ended", pid)
	}
}

// TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild: a refresh of an expired gcloud token
// (gcloudToken.Token, `gcloud auth print-access-token`) that gcloud never answers ends at the
// deadline on each store call (storeCallDeadline), naming the account and the deadline, and the
// child that held gcloud's output is gone with it, not left running. The deadline passes once the
// child runs (awaitCue). The deadline's own timer and its error alone, on every platform, are
// TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's.
func TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	deadline := awaitCue(t)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcloudToken{ctx: deadline, gcloud: gcloud, account: "ada@example.com"})
	err := child.callEnds(t, "the refresh of an expired token", deadline.pass, func() error {
		_, err := source.Token()
		return err
	})
	if want := "did not answer within " + storeCallDeadlineText(); err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), want) {
		t.Fatalf("Token() = %v; want a failure naming the account and %q", err, want)
	}
	child.requireGone(t)
}

// TestGcloudLoginListAtItsDeadlineKillsGcloudsChild: `gcloud auth list` (gcloudLogins) runs under
// storeCallDeadline as the token's refresh does, so a gcloud that never answers ends at that
// deadline, naming it, and the child that held gcloud's output is gone with it, not left running.
// The deadline passes once the child runs (awaitCue).
func TestGcloudLoginListAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	deadline := awaitCue(t)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	err := child.callEnds(t, "`gcloud auth list`", deadline.pass, func() error {
		_, _, err := gcloudLogins(deadline, gcloud)
		return err
	})
	if want := "`gcloud auth list` did not answer within " + storeCallDeadlineText(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("gcloudLogins() = %v; want a failure naming %q", err, want)
	}
	child.requireGone(t)
}

// TestGcloudSignInSignalHelper is the process TestGcloudSignInEndedByASignalKillsGcloudsChild
// signals: `secret list` against a fake broker serving the GCP store, with no other source of a
// Google credential, so its sign-in runs `gcloud auth list` through the stub gcloud of
// gcloudStubIn, written beside the pid file AGENT_SECRETS_GCLOUD_SIGNAL_HELPER names, which never
// answers. The deadline on each store call is a day, so only the signal ends the call, whenever it
// comes. It prints RETURNED <exit code> should the form return.
func TestGcloudSignInSignalHelper(t *testing.T) {
	pidFile := os.Getenv("AGENT_SECRETS_GCLOUD_SIGNAL_HELPER")
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	useStoreCallDeadline(t, 24*time.Hour)
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	gcloud, _ := gcloudStubIn(t, filepath.Dir(pidFile), "wait")
	t.Setenv("PATH", filepath.Dir(gcloud.path)+":/usr/bin:/bin")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	_, stderr, code := runSecret("list")
	fmt.Println("RETURNED", code, stderr)
}

// TestGcloudSignInEndedByASignalKillsGcloudsChild: a sign-in whose gcloud never answers, ended by
// an interrupt sent to agent-secrets alone (gcloud, in a group of its own, gets no signal sent to
// agent-secrets' group, a terminal's Ctrl-C included), ends as the interrupt ends it - by SIGINT,
// a shell's status 130 - and the child that held gcloud's output is gone with it. Each wait here
// waits on its fact (the child running, the helper's end) within testwait's shared bound.
func TestGcloudSignInEndedByASignalKillsGcloudsChild(t *testing.T) {
	dir := t.TempDir()
	child := gcloudChild{pidFile: filepath.Join(dir, "child.pid")}
	t.Cleanup(func() { child.cleanUp(t) })
	helper := exec.Command(os.Args[0], "-test.run=^TestGcloudSignInSignalHelper$", "-test.count=1")
	helper.Env = append(os.Environ(), "AGENT_SECRETS_GCLOUD_SIGNAL_HELPER="+child.pidFile)
	var out syncWriter
	helper.Stdout, helper.Stderr = &out, &out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() {
		_ = helper.Wait()
		close(waited)
	}()
	ended := func() bool {
		select {
		case <-waited:
			return true
		case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
			return false
		}
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		ended()
	})
	seen := testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
		pid := child.pid()
		if pid == 0 {
			return false
		}
		running, err := child.runs(pid)
		return err == nil && running
	})
	if !seen {
		_ = helper.Process.Kill()
		ended()
		t.Fatalf("the stub gcloud's child was never seen running; the helper said %q", out.String())
	}
	if err := helper.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if !ended() {
		_ = helper.Process.Kill()
		ended()
		t.Fatalf("agent-secrets was still running after its interrupt; it said %q", out.String())
	}
	status := helper.ProcessState.Sys().(syscall.WaitStatus)
	if !(status.Signaled() && status.Signal() == syscall.SIGINT) && status.ExitStatus() != 130 {
		t.Fatalf("agent-secrets ended %v, saying %q; want it ended by its interrupt (SIGINT, a shell's 130)", helper.ProcessState, out.String())
	}
	child.requireGone(t)
}

// TestGcloudThatExitsLeavingAChildTakesTheChildWithIt: a gcloud that exits by itself and leaves
// a child holding its output, which gcloudWaitDelay stops the wait for but does not kill, has
// that child killed once runGcloud has waited for gcloud, whatever the call's outcome. The stub
// exits only once the test has seen its child running (callEnds' cue, a file it waits for).
func TestGcloudThatExitsLeavingAChildTakesTheChildWithIt(t *testing.T) {
	dir := t.TempDir()
	goAhead := filepath.Join(dir, "go")
	gcloud, child := gcloudStubIn(t, dir, "until [ -e "+shellWord(goAhead)+" ]; do sleep 0.01; done; exit 0")
	cue := sync.OnceFunc(func() {
		if err := os.WriteFile(goAhead, nil, 0o600); err != nil {
			t.Error(err)
		}
	})
	_ = child.callEnds(t, "a gcloud that exited leaving its child", cue, func() error {
		_, err := runGcloud(context.Background(), gcloud, "auth", "list", "--format=json")
		return err
	})
	child.requireGone(t)
}

// TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild: `gcloud config list`
// (readGcloudSettings) runs under gcloudSettingsDeadline, so a gcloud that never answers ends at
// that deadline as a failed read, and the child that held gcloud's output is gone with it, not
// left running. The deadline passes once the child runs (awaitCue).
func TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	deadline := awaitCue(t)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	err := child.callEnds(t, "`gcloud config list`", deadline.pass, func() error {
		_, err := readGcloudSettings(deadline, gcloud)
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
