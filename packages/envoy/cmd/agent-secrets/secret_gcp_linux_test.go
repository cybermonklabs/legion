//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/agent-secrets/secret_gcp_linux_test.go
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
	"sync/atomic"
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

// gcloudSignalAt is the variable that names where TestGcloudSignInSignalHelper sends itself its
// interrupt (gcloudRaise); unset, the test that runs the helper sends it.
const gcloudSignalAt = "AGENT_SECRETS_GCLOUD_SIGNAL_AT"

// gcloudRaise is a place in runRecorded where TestGcloudSignInSignalHelper sends itself SIGINT,
// once the stub gcloud's child runs, or, before the start, at once.
type gcloudRaise string

const (
	// raiseBeforeStart is the gcloudStarting step: runningGcloud is held by the start, and gcloud
	// is not yet started, so there is no child to wait for and the helper sends the signal at once.
	// A watch armed before the start holds it, and its kill (killRunningGcloud) waits on
	// runningGcloud, so the start goes on: at the gcloudStarted step that follows, the helper waits
	// for the stub's child to run, so that the kill after the record finds it running, and writes
	// "started" to the file "started" beside its pid file. A watch armed only once gcloud runs, at
	// any statement after the fork, is not armed at gcloudStarting: the signal's default action
	// ends agent-secrets there, before gcloud starts, and "started" is never written. Such a watch
	// leaves a window from the fork to its arming in which a signal leaves gcloud's group running,
	// which no row signalling after the fork can hold.
	raiseBeforeStart gcloudRaise = "before start"
	// raiseAtStart is the gcloudStarted step: gcloud has started and its pid is not yet recorded.
	// The helper first reads whether runningGcloud is free there, which a signal's kill
	// (killRunningGcloud) would then take at once and find no gcloud in, and writes "held" or
	// "free" to the file "start-lock" beside its pid file. The step returns once the signal is
	// sent, so the start goes on.
	raiseAtStart gcloudRaise = "start"
	// raiseAtRelease is the first gcloudReleased step after gcloudStarted: the start has given
	// runningGcloud up, and runRecorded has done nothing since. The helper writes whether gcloud's
	// pid is recorded there, "recorded" or "unrecorded", to the file "release-record" beside its pid
	// file, sends the signal, and holds runRecorded at that step for good. So a lock given up before
	// the pid is recorded, to be taken again to record it, is not taken again before the signal's
	// kill (killRunningGcloud), which takes the free lock and finds no gcloud to kill.
	raiseAtRelease gcloudRaise = "release"
	// raiseAtLastKill is the kill of what is left of gcloud's group once gcloud has been waited for
	// (killGroupLeft): the first signal sent to a group (signalGroup), which is never sent, so only
	// the end the interrupt makes can kill the group.
	raiseAtLastKill gcloudRaise = "last kill"
)

// gcloudSignalLast is the variable that gives TestGcloudSignInSignalHelper's stub gcloud its last
// line (gcloudStubIn): "wait" where it is unset, or "exit 0".
const gcloudSignalLast = "AGENT_SECRETS_GCLOUD_SIGNAL_LAST"

// TestGcloudSignInSignalHelper is the process TestGcloudSignInEndedByASignalKillsGcloudsChild
// signals: `secret list` against a fake broker serving the GCP store, with no other source of a
// Google credential, so its sign-in runs `gcloud auth list` through the stub gcloud of
// gcloudStubIn, written beside the pid file AGENT_SECRETS_GCLOUD_SIGNAL_HELPER names, ending with
// the line gcloudSignalLast gives. The deadline on each store call stays 60s, so the signal comes
// while the call runs. Where gcloudSignalAt names a place (gcloudRaise), the helper sends itself
// SIGINT there, once the stub's child runs, or at once at the place before the start, having
// written the place's name to the file "raised" beside its pid file. It prints RETURNED <exit
// code> should the form return.
func TestGcloudSignInSignalHelper(t *testing.T) {
	pidFile := os.Getenv("AGENT_SECRETS_GCLOUD_SIGNAL_HELPER")
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	child := gcloudChild{pidFile: pidFile}
	childRuns := func() {
		if !testwait.Holds(30*time.Second, testwait.Poll, func() bool {
			pid := child.pid()
			running, err := child.runs(pid)
			return pid != 0 && err == nil && running
		}) {
			fmt.Println("RAISE: the stub gcloud's child was never seen running")
		}
	}
	send := func(at gcloudRaise) {
		if err := os.WriteFile(filepath.Join(filepath.Dir(pidFile), "raised"), []byte(at), 0o600); err != nil {
			fmt.Println("RAISE", err)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			fmt.Println("RAISE", err)
		}
	}
	raise := func(at gcloudRaise) {
		childRuns()
		send(at)
	}
	switch at := gcloudRaise(os.Getenv(gcloudSignalAt)); at {
	case raiseBeforeStart:
		var raised atomic.Bool
		atGcloudStep = func(step gcloudStep) {
			switch {
			case step == gcloudStarting && raised.CompareAndSwap(false, true):
				send(at)
			case step == gcloudStarted:
				childRuns()
				if err := os.WriteFile(filepath.Join(filepath.Dir(pidFile), "started"), []byte("started"), 0o600); err != nil {
					fmt.Println("RAISE", err)
				}
			}
		}
	case raiseAtStart:
		atGcloudStep = func(step gcloudStep) {
			if step != gcloudStarted {
				return
			}
			lock := "held"
			if runningGcloud.TryLock() {
				runningGcloud.Unlock()
				lock = "free"
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(pidFile), "start-lock"), []byte(lock), 0o600); err != nil {
				fmt.Println("RAISE", err)
			}
			raise(at)
		}
	case raiseAtRelease:
		started := false
		atGcloudStep = func(step gcloudStep) {
			switch {
			case step == gcloudStarted:
				started = true
			case step == gcloudReleased && started:
				runningGcloud.Mutex.Lock()
				record := "unrecorded"
				if len(runningGcloud.pids) > 0 {
					record = "recorded"
				}
				runningGcloud.Mutex.Unlock()
				if err := os.WriteFile(filepath.Join(filepath.Dir(pidFile), "release-record"), []byte(record), 0o600); err != nil {
					fmt.Println("RAISE", err)
				}
				raise(at)
				select {}
			}
		}
	case raiseAtLastKill:
		// The kill that is held is the first of the call; the end the interrupt makes sends its
		// own (killRunningGcloud), from the watch's goroutine, through the second.
		send := signalGroup
		var held atomic.Bool
		signalGroup = func(pgid int, sig syscall.Signal) error {
			if !held.CompareAndSwap(false, true) {
				return send(pgid, sig)
			}
			raise(at)
			select {}
		}
	}
	last := os.Getenv(gcloudSignalLast)
	if last == "" {
		last = "wait"
	}
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	gcloud, _ := gcloudStubIn(t, filepath.Dir(pidFile), last)
	t.Setenv("PATH", filepath.Dir(gcloud.path)+":/usr/bin:/bin")
	t.Setenv("CLOUDSDK_CORE_ACCOUNT", "")
	startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
	_, stderr, code := runSecret("list")
	fmt.Println("RETURNED", code, stderr)
}

// TestGcloudSignInEndedByASignalKillsGcloudsChild: a sign-in whose gcloud never answers, ended by
// an interrupt sent to agent-secrets alone (gcloud, in a group of its own, gets no signal sent to
// agent-secrets' group, a terminal's Ctrl-C included), ends as the interrupt ends it - by SIGINT,
// a shell's status 130 - and the child that held gcloud's output is gone with it. That holds
// wherever the interrupt comes in runRecorded: while the call waits on gcloud; the moment before
// gcloud starts, with runningGcloud held by the start (raiseBeforeStart); the moment gcloud has
// started, while the start still holds runningGcloud and before gcloud's pid is recorded
// (raiseAtStart); the moment the start gives runningGcloud up (raiseAtRelease); and once a gcloud
// that exited leaving its child has been waited for, at the kill of what is left of its group,
// before that kill is sent (raiseAtLastKill).
//
// The last four rows have the helper send itself the interrupt at that place, rather than this
// test from outside, so it comes there and at no other moment: a watch not yet armed at that step,
// or stopped before that kill, meets it unarmed, the interrupt's default action ends agent-secrets
// with gcloud's group running, and the row fails every time. At the start the row also requires
// what that signal's kill finds there (killRunningGcloud): runningGcloud still held by the start,
// so the kill waits for gcloud's pid to be recorded and then kills its group. A lock taken only
// once the start has returned would be free there, and the kill would find no gcloud recorded.
// At the release the row requires gcloud's pid recorded there, and the helper holds runRecorded at
// that step, so the signal's kill takes the free lock with nothing more done: a lock given up
// before the record, to be taken again for it, leaves the kill no gcloud to find. Without that
// hold a row would see such a retake only by chance: the goroutine that gives a sync.Mutex up
// almost always takes it again before one parked on it runs. Before the start the row requires
// that gcloud was started all the same: the watch, armed before the fork, held the interrupt, so
// the start went on and the kill then found gcloud recorded. A watch armed at any statement after
// the fork meets that interrupt unarmed, and agent-secrets ends there, before gcloud starts; no
// row that signals after the fork can see such a watch, since it is armed by then.
func TestGcloudSignInEndedByASignalKillsGcloudsChild(t *testing.T) {
	for _, row := range []struct {
		name, last string
		at         gcloudRaise
	}{
		{name: "while the call waits on gcloud", last: "wait"},
		{name: "the moment before gcloud starts", last: "wait", at: raiseBeforeStart},
		{name: "the moment gcloud starts", last: "wait", at: raiseAtStart},
		{name: "the moment the start gives runningGcloud up", last: "wait", at: raiseAtRelease},
		{name: "at the last kill of a gcloud that left its child", last: "exit 0", at: raiseAtLastKill},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			child := gcloudChild{pidFile: filepath.Join(dir, "child.pid")}
			t.Cleanup(func() { child.cleanUp(t) })
			helper := exec.Command(os.Args[0], "-test.run=^TestGcloudSignInSignalHelper$", "-test.count=1")
			helper.Env = append(os.Environ(),
				"AGENT_SECRETS_GCLOUD_SIGNAL_HELPER="+child.pidFile,
				gcloudSignalLast+"="+row.last,
				gcloudSignalAt+"="+string(row.at))
			var out bytes.Buffer
			helper.Stdout, helper.Stderr = &out, &out
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan struct{})
			go func() {
				_ = helper.Wait()
				close(waited)
			}()
			ended := func(within time.Duration) bool {
				select {
				case <-waited:
					return true
				case <-time.After(within):
					return false
				}
			}
			t.Cleanup(func() {
				_ = helper.Process.Kill()
				ended(10 * time.Second)
			})
			// The helper that sends itself the interrupt first waits for the stub's child, up to
			// 30s; the bound on the end after it is the 10s this test gives an interrupt it sends.
			bound := 40 * time.Second
			if row.at == "" {
				bound = 10 * time.Second
				seen := testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
					pid := child.pid()
					if pid == 0 {
						return false
					}
					running, err := child.runs(pid)
					return err == nil && running
				})
				if !seen {
					_ = helper.Process.Kill()
					ended(10 * time.Second)
					t.Fatalf("the stub gcloud's child was never seen running; the helper said %q", out.String())
				}
				if err := helper.Process.Signal(syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
			}
			if !ended(bound) {
				_ = helper.Process.Kill()
				ended(10 * time.Second)
				t.Fatalf("agent-secrets was still running %s after its interrupt was due; it said %q", bound, out.String())
			}
			if row.at != "" {
				if raised, err := os.ReadFile(filepath.Join(dir, "raised")); err != nil || gcloudRaise(raised) != row.at {
					t.Fatalf("the helper did not send its interrupt at %q (%q, %v); it said %q", row.at, raised, err, out.String())
				}
			}
			if row.at == raiseBeforeStart {
				if started, err := os.ReadFile(filepath.Join(dir, "started")); err != nil || string(started) != "started" {
					t.Fatalf("agent-secrets ended at an interrupt sent the moment before gcloud's start without starting gcloud (%q, %v): no watch held that interrupt, and a watch armed only once gcloud runs leaves a window from gcloud's start to its arming in which a signal leaves gcloud's group running: %q", started, err, out.String())
				}
			}
			if row.at == raiseAtStart {
				if lock, err := os.ReadFile(filepath.Join(dir, "start-lock")); err != nil || string(lock) != "held" {
					t.Fatalf("runningGcloud was %q the moment gcloud had started (%v); want it held by the start, so a signal then waits for gcloud's pid to be recorded rather than finding no gcloud to kill: %q", lock, err, out.String())
				}
			}
			if row.at == raiseAtRelease {
				if record, err := os.ReadFile(filepath.Join(dir, "release-record")); err != nil || string(record) != "recorded" {
					t.Errorf("gcloud's pid was %q the moment the start gave runningGcloud up (%v); want it recorded before the lock is first given up, so a signal then finds gcloud to kill: %q", record, err, out.String())
				}
			}
			status := helper.ProcessState.Sys().(syscall.WaitStatus)
			if !(status.Signaled() && status.Signal() == syscall.SIGINT) && status.ExitStatus() != 130 {
				t.Fatalf("agent-secrets ended %v, saying %q; want it ended by its interrupt (SIGINT, a shell's 130)", helper.ProcessState, out.String())
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
