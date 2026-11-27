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
	"runtime"
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

// gcloudHeldDeadline is what these tests make the deadline on each store call (storeCallDeadline)
// and gcloudSettingsDeadline: longer than any run, so no timer ends a call. The test ends each
// call itself, once it has seen the stub gcloud's child running (callEnds), by firing the call's
// context as a deadline (firedDeadline), the way a timer's expiry ends it. So the child is always
// seen before the kill, whatever the machine's load. That the timer itself ends the call at the
// deadline, naming it, is TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's and
// TestGCPSecretGcloudLoginListThatNeverAnswersEndsAtTheStoreDeadline's.
const gcloudHeldDeadline = time.Hour

// firedDeadline is a context that ends when the test fires it (fire): Done closes then and Err
// answers context.DeadlineExceeded, as an expired deadline's context does. A context the code
// derives from it (context.WithTimeout) ends with it, with the same error, so the code takes the
// path its own deadline takes: the kill of gcloud's group (killGroupOnCancel) and its "did not
// answer" error.
type firedDeadline struct {
	done chan struct{}
	once sync.Once
}

// newFiredDeadline is a firedDeadline that t's end fires, so a test that failed before firing it
// still lets its call end.
func newFiredDeadline(t *testing.T) *firedDeadline {
	d := &firedDeadline{done: make(chan struct{})}
	t.Cleanup(d.fire)
	return d
}

func (d *firedDeadline) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d *firedDeadline) Done() <-chan struct{}       { return d.done }
func (d *firedDeadline) Value(any) any               { return nil }
func (d *firedDeadline) fire()                       { d.once.Do(func() { close(d.done) }) }

func (d *firedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// gcloudChildFile is the variable that names the file the stub gcloud's child writes its pid to
// (useGcloudWhoseChildHoldsItsOutput). The child inherits it, so it also marks the child's
// environment as this test's (ours).
const gcloudChildFile = "AGENT_SECRETS_TEST_GCLOUD_CHILD"

// gcloudChild is the child of the stub gcloud useGcloudWhoseChildHoldsItsOutput writes, by the
// file it writes its pid to; or, with envFile set, another process of that stub (the stub's own,
// in TestGcloudEndsWhenAgentSecretsIsKilled), by its pid file and the child's, which its
// environment names (ours).
type gcloudChild struct {
	pidFile string
	envFile string
}

// useGcloudWhoseChildHoldsItsOutput is a stub gcloud that never answers, whatever it is asked: it
// starts a child that holds its standard output open, as gcloud's Python does under a shell
// script that does not exec it, and waits on it. The child writes its own pid to a file before it
// waits, so it names itself even where the stub is killed first. It waits for as long as the
// process that wrote the stub runs, which is no bound of the test's: it only keeps a child the
// kill missed from outliving a test binary that was killed itself. A child still running when t
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
		`/bin/sh -c 'echo $$ >"$` + gcloudChildFile + `"; while kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; do sleep 1; done' &` + "\n" +
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
	if pid == 0 && !heldBeforeGiveUp(t, func() bool { pid = c.pid(); return pid != 0 }) {
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

// ours reports whether pid's environment holds this child's pid file (envFile where it is set), so
// that no process the kernel has given the pid since is killed for the child. A process that is
// exiting can refuse a read of its environment for a moment (permission denied, seen once in
// twenty runs), so ours asks again until the waits of these tests give up (waitsGaveUp) before it
// answers that error.
func (c gcloudChild) ours(t *testing.T, pid int) (bool, error) {
	marker := c.pidFile
	if c.envFile != "" {
		marker = c.envFile
	}
	var answer bool
	var readErr error
	testwait.Holds(untilGiveUp(t), testwait.Poll, func() bool {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH):
			answer, readErr = false, nil
		case err != nil:
			readErr = err
			return false
		default:
			answer, readErr = slices.Contains(strings.Split(string(environ), "\x00"), gcloudChildFile+"="+marker), nil
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

// callEnds is call's error. It starts call, waits until the stub gcloud's child has named itself
// and /proc shows it running, then runs release, which ends the call the way the row's code ends
// it: a fired deadline (firedDeadline.fire), or a stub that may now exit. Nothing else ends the
// call before release (gcloudHeldDeadline), so the child is always seen running first, and its
// absence afterwards (requireGone) is read by the same /proc that saw it. The call is timed
// against after, how long the code may take once released (0 for a deadline's kill,
// gcloudWaitDelay for a gcloud that exited), less the time this process was held up
// (endsByItsDeadline). It fails t where call ends before release (the stub did not hold it), and
// where the child is not seen or call has not ended when the waits of these tests give up. Where
// it fails t before call has returned, t ends by running release, so nothing the row set up still
// holds the call, then killing the child (cleanUp), which lets a call held by the child's open
// output return, and then waits for call, failing where it has not returned when the waits give up.
func (c gcloudChild) callEnds(t *testing.T, what string, after time.Duration, release func(), call func() error) error {
	t.Helper()
	done, finished := make(chan error, 1), make(chan struct{})
	meter := startStallMeter(t)
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
		release()
		c.cleanUp(t)
		// Past the give-up moment received would wait no time at all, so the released call is
		// given a share of the margin kept for this cleanup to end, and kill its stub, instead.
		select {
		case <-finished:
		case <-time.After(cleanupShare):
			t.Errorf("%s was still waiting on gcloud %s after its release, in its test's cleanup", what, cleanupShare)
		}
	})
	giveUp := time.NewTimer(untilGiveUp(t))
	defer giveUp.Stop()
	poll := time.NewTicker(testwait.Poll)
	defer poll.Stop()
	for seen := false; !seen; {
		select {
		case err := <-done:
			t.Fatalf("%s ended (%v) before it was released, with the stub gcloud's child unseen", what, err)
		case <-giveUp.C:
			t.Fatalf("the stub gcloud's child was never seen running while %s ran, %s before the test binary's deadline; its pid file %s holds %d", what, waitMargin, c.pidFile, c.pid())
		case <-poll.C:
			if pid := c.pid(); pid != 0 {
				running, err := c.runs(pid)
				if err != nil {
					t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, err)
				}
				seen = running
			}
		}
	}
	start := time.Now()
	release()
	return endsByItsDeadline(t, meter, what, start, after, done)
}

// requireGone fails t unless the child, seen running while the call ran (callEnds), is gone once
// the call has ended: the kill at the deadline reached it, not the stub gcloud alone. The wait's
// one bound is the moment the waits of these tests give up (waitsGaveUp): a child the kill missed
// sleeps for an hour.
func (c gcloudChild) requireGone(t *testing.T) {
	t.Helper()
	pid := c.pid()
	var readErr error
	gone := heldBeforeGiveUp(t, func() bool {
		running, err := c.runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud's child, pid %d: %v", pid, readErr)
	}
	if !gone {
		t.Fatalf("the stub gcloud's child, pid %d, still runs after the call that started it ended, %s before the test binary's deadline", pid, waitMargin)
	}
}

// TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild: a refresh of an expired gcloud token
// (gcloudToken.Token, `gcloud auth print-access-token`) that gcloud never answers ends at the
// deadline on each store call (storeCallDeadline), naming the account and the deadline, and the
// child that held gcloud's output is gone with it, not left running. The test fires the deadline
// once it has seen the child running (callEnds); the timer's own expiry and its error alone, on
// every platform, are TestGcloudTokenRefreshThatNeverAnswersEndsAtTheStoreDeadline's.
func TestGcloudTokenRefreshAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useGcloudWhoseChildHoldsItsOutput, main.gcloudStubIn, t.Setenv at secret_gcp_linux_test.go:108); it signals the test process or changes its signal handling ((gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStoreCallDeadline, storeCallDeadline set at secret_create_test.go:36); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((gcloudChild).callEnds, main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	useStoreCallDeadline(t, gcloudHeldDeadline)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	expired := &oauth2.Token{AccessToken: "old", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)}
	source := oauth2.ReuseTokenSource(expired, gcloudToken{ctx: deadline, gcloud: gcloud, account: "ada@example.com"})
	err := child.callEnds(t, "the refresh of an expired token", 0, deadline.fire, func() error {
		_, err := source.Token()
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "ada@example.com") || !strings.Contains(err.Error(), "did not answer within 3600s") {
		t.Fatalf("Token() = %v; want a failure naming the account and the deadline", err)
	}
	child.requireGone(t)
}

// TestGcloudLoginListAtItsDeadlineKillsGcloudsChild: `gcloud auth list` (gcloudLogins) runs under
// storeCallDeadline as the token's refresh does, so a gcloud that never answers ends at that
// deadline, naming it, and the child that held gcloud's output is gone with it, not left running.
// The test fires the deadline once it has seen the child running (callEnds).
func TestGcloudLoginListAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useGcloudWhoseChildHoldsItsOutput, main.gcloudStubIn, t.Setenv at secret_gcp_linux_test.go:108); it signals the test process or changes its signal handling (main.gcloudLogins, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.useStoreCallDeadline, storeCallDeadline set at secret_create_test.go:36); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((gcloudChild).callEnds, main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	useStoreCallDeadline(t, gcloudHeldDeadline)
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	err := child.callEnds(t, "`gcloud auth list`", 0, deadline.fire, func() error {
		_, _, err := gcloudLogins(deadline, gcloud)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "`gcloud auth list` did not answer within 3600s") {
		t.Fatalf("gcloudLogins() = %v; want a failure naming the deadline", err)
	}
	child.requireGone(t)
}

// gcloudSignalAt is the variable that makes TestGcloudSignInSignalHelper interrupt itself at one
// moment of its gcloud call (onGcloudStep): gcloudStarted or gcloudWaited.
const gcloudSignalAt = "AGENT_SECRETS_GCLOUD_SIGNAL_AT"

// gcloudSignInDeadline is the variable that sets TestGcloudSignInSignalHelper's deadline on each
// store call (storeCallDeadline), 60s where it is unset.
const gcloudSignInDeadline = "AGENT_SECRETS_GCLOUD_DEADLINE"

// gcloudExecs is the variable that makes TestGcloudSignInSignalHelper's stub gcloud, once it has
// started its child, write its own pid beside the child's (gcloudPidName) and replace itself with
// a process that never answers (exec), as gcloud's own script replaces itself with its Python:
// nothing of that stub process then watches anything, so only its parent-death signal ends it
// (gcloudProcAttr, TestGcloudEndsWhenAgentSecretsIsKilled).
const gcloudExecs = "AGENT_SECRETS_GCLOUD_EXECS"

// gcloudPidName is the file, beside the child's pid file, the stub of gcloudExecs writes its own pid to.
const gcloudPidName = "gcloud.pid"

// TestGcloudSignInSignalHelper is the process the gcloud sign-in signal rows signal: `secret list`
// against a fake broker serving the GCP store, with no other source of a Google credential, so
// its sign-in runs `gcloud auth list` through the stub gcloud of gcloudStubIn, written beside the
// pid file AGENT_SECRETS_GCLOUD_SIGNAL_HELPER names. It prints RETURNED <exit code> should the
// form return. Without gcloudSignalAt the stub never answers, so the call ends only at the deadline
// on each store call (gcloudSignInDeadline) or by a signal, and
// TestGcloudSignInEndedByASignalKillsGcloudsChild's signal comes while the call runs. With
// gcloudSignalAt set, it sends SIGINT to the thread that runs onGcloudStep at that
// step, once the stub's child has named itself, so the signal is handled on that thread before
// runRecorded goes on; at gcloudWaited the stub exits at once, leaving its child holding its
// output (TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild).
func TestGcloudSignInSignalHelper(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv at secret_gcp_linux_test.go:408); it signals the test process or changes its signal handling (main.useNoADC, main.userinfoOf, (gcloudToken).Token, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (onGcloudStep set at secret_gcp_linux_test.go:395).
	pidFile := os.Getenv("AGENT_SECRETS_GCLOUD_SIGNAL_HELPER")
	if pidFile == "" {
		t.Skip("subprocess helper")
	}
	if d := os.Getenv(gcloudSignInDeadline); d != "" {
		deadline, err := time.ParseDuration(d)
		if err != nil {
			t.Fatal(err)
		}
		useStoreCallDeadline(t, deadline)
	}
	fake := newFakeSecretManager()
	startFakeSecretManager(t, fake)
	useNoADC(t)
	at := os.Getenv(gcloudSignalAt)
	last := "wait"
	if at == gcloudWaited {
		last = "exit 0"
	}
	if os.Getenv(gcloudExecs) != "" {
		last = "echo $$ >" + shellWord(filepath.Join(filepath.Dir(pidFile), gcloudPidName)) + "\nexec sleep 3600"
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
			runtime.LockOSThread()
			_ = syscall.Tgkill(os.Getpid(), syscall.Gettid(), syscall.SIGINT)
			runtime.UnlockOSThread()
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
// ends is stopped (stop). Where this test process is itself killed and runs no cleanup, the
// helper is killed with it (its parent-death signal), and its gcloud with the helper
// (gcloudProcAttr), so nothing the row started outlives the test binary. The signal follows the
// thread that started the helper, so one goroutine holds that thread from Start until the helper
// has been waited for.
func startSignInHelper(t *testing.T, child gcloudChild, env ...string) *signInHelper {
	t.Helper()
	cmd := exec.Command("env", "--default-signal=INT", os.Args[0], "-test.run=^TestGcloudSignInSignalHelper$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_GCLOUD_SIGNAL_HELPER="+child.pidFile), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	h := &signInHelper{cmd: cmd, out: &bytes.Buffer{}, waited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = h.out, h.out
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		_ = cmd.Wait()
		close(h.waited)
	}()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.stop)
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

// stop ends a helper still running as an interrupt ends it, so that on its way out it kills
// gcloud's group (exitBySignal, killRunningGcloud), which a SIGKILL would leave running in a group
// of its own, its call held for up to an hour; it is killed only where it has not ended within
// cleanupShare.
func (h *signInHelper) stop() {
	select {
	case <-h.waited:
		return
	default:
	}
	_ = h.cmd.Process.Signal(syscall.SIGINT)
	if !h.ended(cleanupShare) {
		_ = h.cmd.Process.Kill()
		<-h.waited
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
// a shell's status 130 - at once, and the child that held gcloud's output is gone with it. The
// call's deadline is an hour (gcloudSignInDeadline), so only the interrupt can end it while the
// test waits: a signal path that waited behind the call would never end. The end is read from that
// cause and its status, not timed: agent-secrets runs in a process of its own, which a machine can
// starve alone.
func TestGcloudSignInEndedByASignalKillsGcloudsChild(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireCommandOnTime, onTimeWithin at waits_test.go:245).
	child := gcloudChild{pidFile: filepath.Join(t.TempDir(), "child.pid")}
	t.Cleanup(func() { child.cleanUp(t) })
	helper := startSignInHelper(t, child, gcloudSignInDeadline+"=1h")
	seen := heldBeforeGiveUp(t, func() bool {
		pid := child.pid()
		if pid == 0 {
			return false
		}
		running, err := child.runs(pid)
		return err == nil && running
	})
	if !seen {
		helper.stop()
		t.Fatalf("the stub gcloud's child was never seen running, %s before the test binary's deadline; the helper said %q", waitMargin, helper.out.String())
	}
	if err := helper.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if _, ok := received(t, helper.waited); !ok {
		helper.stop()
		t.Fatalf("agent-secrets was still running after its interrupt, %s before the test binary's deadline; it said %q", waitMargin, helper.out.String())
	}
	helper.requireEndedByInterrupt(t)
	child.requireGone(t)
}

// TestGcloudEndsWhenAgentSecretsIsKilled: a gcloud still running when agent-secrets ends by a
// SIGKILL, which runs none of agent-secrets' code, ends with it (gcloudProcAttr's parent-death
// signal), and is not left running in its group of its own until it ends by itself. The stub
// gcloud replaces itself with a process that never answers (gcloudExecs), as gcloud's own script
// replaces itself with its Python, so nothing of it watches agent-secrets: only that signal can
// end it. The wait for its end is bounded only by the moment the waits of these tests give up.
func TestGcloudEndsWhenAgentSecretsIsKilled(t *testing.T) {
	t.Parallel()
	child := gcloudChild{pidFile: filepath.Join(t.TempDir(), "child.pid")}
	gcloud := gcloudChild{pidFile: filepath.Join(filepath.Dir(child.pidFile), gcloudPidName), envFile: child.pidFile}
	t.Cleanup(func() { child.cleanUp(t) })
	helper := startSignInHelper(t, child, gcloudSignInDeadline+"=1h", gcloudExecs+"=1")
	var pid int
	seen := heldBeforeGiveUp(t, func() bool {
		if pid = gcloud.pid(); pid == 0 {
			return false
		}
		running, err := gcloud.runs(pid)
		return err == nil && running
	})
	if !seen {
		helper.stop()
		t.Fatalf("the stub gcloud never named itself running in %s, %s before the test binary's deadline; the helper said %q", gcloud.pidFile, waitMargin, helper.out.String())
	}
	if err := helper.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-helper.waited
	var readErr error
	gone := heldBeforeGiveUp(t, func() bool {
		running, err := gcloud.runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud, pid %d: %v", pid, readErr)
	}
	if !gone {
		gcloud.kill(t, pid)
		t.Fatalf("the stub gcloud, pid %d, still ran after agent-secrets was killed, %s before the test binary's deadline", pid, waitMargin)
	}
}

// TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild: an interrupt that comes at
// either edge of a gcloud call still ends agent-secrets by SIGINT and kills gcloud's group, the
// child that holds gcloud's output among them: the moment gcloud has started, before
// agent-secrets has recorded it (gcloudStarted), and the moment a gcloud that exited leaving that
// child has been waited for, before the child is killed (gcloudWaited). The helper sends the
// interrupt to itself from onGcloudStep once the stub's child has named itself, so it lands in
// that moment on every run, whatever the machine's load.
func TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild(t *testing.T) {
	t.Parallel()
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
// that child killed once runGcloud has waited for gcloud, whatever the call's outcome. The stub
// exits once the test, having seen the child running, writes its go-ahead file (callEnds).
func TestGcloudThatExitsLeavingAChildTakesTheChildWithIt(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.gcloudStubIn, t.Setenv at secret_gcp_linux_test.go:108); it signals the test process or changes its signal handling (main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (main.runGcloud, main.runRecorded, runningGcloud set at secret_gcp.go:516); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((gcloudChild).callEnds, main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	dir := t.TempDir()
	goAhead := filepath.Join(dir, "go")
	// The stub waits for the go-ahead, and gives up when this test's process is gone, so a test
	// that failed before its release leaves no stub behind.
	wait := "while [ ! -e " + shellWord(goAhead) + " ]; do kill -0 " + strconv.Itoa(os.Getpid()) + " 2>/dev/null || exit 1; sleep 0.01; done\nexit 0"
	gcloud, child := gcloudStubIn(t, dir, wait)
	release := func() {
		if err := os.WriteFile(goAhead, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = child.callEnds(t, "a gcloud that exited leaving its child", gcloudWaitDelay, release, func() error {
		_, err := runGcloud(context.Background(), gcloud, "auth", "list", "--format=json")
		return err
	})
	child.requireGone(t)
}

// TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild: `gcloud config list`
// (readGcloudSettings) runs under gcloudSettingsDeadline, so a gcloud that never answers ends at
// that deadline as a failed read, and the child that held gcloud's output is gone with it, not
// left running. The test fires the deadline once it has seen the child running (callEnds); the
// timer's own expiry is TestGCPSecretGcloudSettingsReadIsBestEffort's.
func TestGcloudSettingsReadAtItsDeadlineKillsGcloudsChild(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (main.useGcloudWhoseChildHoldsItsOutput, main.gcloudStubIn, t.Setenv at secret_gcp_linux_test.go:108); it signals the test process or changes its signal handling (main.readGcloudSettings, main.runGcloud, main.runRecorded, main.watchSignalsWhileGcloudRuns, signal.Notify at secret_gcp.go:558); it sets a package variable the package's other tests read (gcloudSettingsDeadline set at secret_gcp_linux_test.go:633); it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past ((gcloudChild).callEnds, main.endsByItsDeadline, main.requireOnTime, onTimeWithin at waits_test.go:239).
	restore := gcloudSettingsDeadline
	gcloudSettingsDeadline = gcloudHeldDeadline
	t.Cleanup(func() { gcloudSettingsDeadline = restore })
	gcloud, child := useGcloudWhoseChildHoldsItsOutput(t)
	deadline := newFiredDeadline(t)
	err := child.callEnds(t, "`gcloud config list`", 0, deadline.fire, func() error {
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
	// Not parallel: it sets a package variable the package's other tests read (signalGroup set at secret_gcp_linux_test.go:656).
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
