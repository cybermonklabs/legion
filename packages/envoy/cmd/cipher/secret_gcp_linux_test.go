//go:build linux && (amd64 || arm64)

// packages/envoy/cmd/cipher/secret_gcp_linux_test.go
//
// A gcloud sign-in ended by an interrupt kills every process left in gcloud's process group
// (runChild), a child that holds gcloud's output among them. These tests read /proc to see a stub
// gcloud's child running while the call runs and gone once the call has ended
// (gcptest.GcloudChild), so they build where it does: Linux on amd64 or arm64.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/gcp/gcptest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// gcloudSignalAt is the variable that makes TestGcloudSignInSignalHelper interrupt itself at one
// moment of its gcloud call (onChildStep): childStarted or childWaited.
const gcloudSignalAt = "AGENT_SECRETS_GCLOUD_SIGNAL_AT"

// gcloudSignInDeadline is the variable that sets TestGcloudSignInSignalHelper's deadline on each
// store call (secretstore.CallDeadline), 60s where it is unset.
const gcloudSignInDeadline = "AGENT_SECRETS_GCLOUD_DEADLINE"

// gcloudExecs is the variable that makes TestGcloudSignInSignalHelper's stub gcloud, once it has
// started its child, write its own pid beside the child's (gcloudPidName) and replace itself with
// a process that never answers (exec), as gcloud's own script replaces itself with its Python:
// nothing of that stub process then watches anything, so only its parent-death signal ends it
// (childProcAttr, TestGcloudEndsWhenAgentSecretsIsKilled).
const gcloudExecs = "AGENT_SECRETS_GCLOUD_EXECS"

// gcloudPidName is the file, beside the child's pid file, the stub of gcloudExecs writes its own pid to.
const gcloudPidName = "gcloud.pid"

// TestGcloudSignInSignalHelper is the process the gcloud sign-in signal rows signal: `secret list`
// against a fake broker serving the GCP store, with no other source of a Google credential, so
// its sign-in runs `gcloud auth list` through the stub gcloud of gcptest.GcloudStubIn, written beside the
// pid file AGENT_SECRETS_GCLOUD_SIGNAL_HELPER names. It prints RETURNED <exit code> should the
// form return. Without gcloudSignalAt the stub never answers, so the call ends only at the deadline
// on each store call (gcloudSignInDeadline) or by a signal, and
// TestGcloudSignInEndedByASignalKillsGcloudsChild's signal comes while the call runs. With
// gcloudSignalAt set, it sends SIGINT to the thread that runs onChildStep at that
// step, once the stub's child has named itself, so the signal is handled on that thread before
// runChild goes on; at childWaited the stub exits at once, leaving its child holding its
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
	fake := gcptest.NewFakeSecretManager()
	gcptest.UseFake(t, fake)
	gcptest.UseNoADC(t)
	at := os.Getenv(gcloudSignalAt)
	last := "wait"
	if at == childWaited {
		last = "exit 0"
	}
	if os.Getenv(gcloudExecs) != "" {
		last = "echo $$ >" + secretstore.ShellWord(filepath.Join(filepath.Dir(pidFile), gcloudPidName)) + "\nexec sleep 3600"
	}
	gcloud, child := gcptest.GcloudStubIn(t, filepath.Dir(pidFile), last)
	if at != "" {
		restore := onChildStep
		t.Cleanup(func() { onChildStep = restore })
		onChildStep = func(step string) {
			if step != at {
				return
			}
			for child.Pid() == 0 {
				time.Sleep(testwait.Poll)
			}
			runtime.LockOSThread()
			_ = syscall.Tgkill(os.Getpid(), syscall.Gettid(), syscall.SIGINT)
			runtime.UnlockOSThread()
		}
	}
	t.Setenv("PATH", filepath.Dir(gcloud)+":/usr/bin:/bin")
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
// (childProcAttr), so nothing the row started outlives the test binary. The signal follows the
// thread that started the helper, so one goroutine holds that thread from Start until the helper
// has been waited for.
func startSignInHelper(t *testing.T, child gcptest.GcloudChild, env ...string) *signInHelper {
	t.Helper()
	cmd := exec.Command("env", "--default-signal=INT", os.Args[0], "-test.run=^TestGcloudSignInSignalHelper$", "-test.count=1")
	cmd.Env = append(append(os.Environ(), "AGENT_SECRETS_GCLOUD_SIGNAL_HELPER="+child.PidFile), env...)
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
// gcloud's group (exitBySignal, killRunningChildren), which a SIGKILL would leave running in a group
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
// test waits: a signal path that waited behind the call would never end. The end is timed from the
// interrupt against no deadline of its own (requireCommandOnTime), so a path that waits for
// anything first fails at once.
func TestGcloudSignInEndedByASignalKillsGcloudsChild(t *testing.T) {
	// Not parallel: it holds code to an upper bound on a measured time, which the load of the package's parallel tests could push past (main.requireCommandOnTime, onTimeWithin at waits_test.go:245).
	child := gcptest.GcloudChild{PidFile: filepath.Join(t.TempDir(), "child.pid")}
	t.Cleanup(func() { child.CleanUp(t) })
	helper := startSignInHelper(t, child, gcloudSignInDeadline+"=1h")
	seen := heldBeforeGiveUp(t, func() bool {
		pid := child.Pid()
		if pid == 0 {
			return false
		}
		running, err := child.Runs(pid)
		return err == nil && running
	})
	if !seen {
		helper.stop()
		t.Fatalf("the stub gcloud's child was never seen running, %s before the test binary's deadline; the helper said %q", waitMargin, helper.out.String())
	}
	meter := startStallMeter(t)
	sent := time.Now()
	if err := helper.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if _, ok := received(t, helper.waited); !ok {
		helper.stop()
		t.Fatalf("agent-secrets was still running after its interrupt, %s before the test binary's deadline; it said %q", waitMargin, helper.out.String())
	}
	requireCommandOnTime(t, meter, "agent-secrets after its interrupt, which ends it at once", sent, 0)
	helper.requireEndedByInterrupt(t)
	child.RequireGone(t)
}

// TestGcloudEndsWhenAgentSecretsIsKilled: a gcloud still running when agent-secrets ends by a
// SIGKILL, which runs none of agent-secrets' code, ends with it (childProcAttr's parent-death
// signal), and is not left running in its group of its own until it ends by itself. The stub
// gcloud replaces itself with a process that never answers (gcloudExecs), as gcloud's own script
// replaces itself with its Python, so nothing of it watches agent-secrets: only that signal can
// end it. The wait for its end is bounded only by the moment the waits of these tests give up.
func TestGcloudEndsWhenAgentSecretsIsKilled(t *testing.T) {
	t.Parallel()
	child := gcptest.GcloudChild{PidFile: filepath.Join(t.TempDir(), "child.pid")}
	gcloud := gcptest.GcloudChild{PidFile: filepath.Join(filepath.Dir(child.PidFile), gcloudPidName), EnvFile: child.PidFile}
	t.Cleanup(func() { child.CleanUp(t) })
	helper := startSignInHelper(t, child, gcloudSignInDeadline+"=1h", gcloudExecs+"=1")
	var pid int
	seen := heldBeforeGiveUp(t, func() bool {
		if pid = gcloud.Pid(); pid == 0 {
			return false
		}
		running, err := gcloud.Runs(pid)
		return err == nil && running
	})
	if !seen {
		helper.stop()
		t.Fatalf("the stub gcloud never named itself running in %s, %s before the test binary's deadline; the helper said %q", gcloud.PidFile, waitMargin, helper.out.String())
	}
	if err := helper.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-helper.waited
	var readErr error
	gone := heldBeforeGiveUp(t, func() bool {
		running, err := gcloud.Runs(pid)
		readErr = err
		return err != nil || !running
	})
	if readErr != nil {
		t.Fatalf("reading the stub gcloud, pid %d: %v", pid, readErr)
	}
	if !gone {
		gcloud.Kill(t, pid)
		t.Fatalf("the stub gcloud, pid %d, still ran after agent-secrets was killed, %s before the test binary's deadline", pid, waitMargin)
	}
}

// TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild: an interrupt that comes at
// either edge of a gcloud call still ends agent-secrets by SIGINT and kills gcloud's group, the
// child that holds gcloud's output among them: the moment gcloud has started, before
// agent-secrets has recorded it (childStarted), and the moment a gcloud that exited leaving that
// child has been waited for, before the child is killed (childWaited). The helper sends the
// interrupt to itself from onChildStep once the stub's child has named itself, so it lands in
// that moment on every run, whatever the machine's load.
func TestGcloudSignInSignalledAtGcloudsStartOrEndKillsGcloudsChild(t *testing.T) {
	t.Parallel()
	for _, step := range []string{childStarted, childWaited} {
		t.Run(step, func(t *testing.T) {
			child := gcptest.GcloudChild{PidFile: filepath.Join(t.TempDir(), "child.pid")}
			t.Cleanup(func() { child.CleanUp(t) })
			helper := startSignInHelper(t, child, gcloudSignalAt+"="+step)
			testwait.Receive(t, "agent-secrets' end by the interrupt it sent itself once gcloud had "+step, helper.waited)
			helper.requireEndedByInterrupt(t)
			if child.Pid() == 0 {
				t.Fatalf("the stub gcloud's child never named itself, so no interrupt came from onChildStep; the helper said %q", helper.out.String())
			}
			child.RequireGone(t)
		})
	}
}

// TestChildCancelOfAGroupGoneAlreadyAnswersProcessDone: the kill at the end of a child's
// context (runChild) answers os.ErrProcessDone where the kernel says the group is gone
// already (ESRCH), as exec.Cmd's own kill does for a process that is done, so a gcloud that ended
// by itself keeps its own exit status; any other answer is returned as it came. signalGroup is
// replaced, so no signal reaches any process: a real kill of a reaped gcloud's pid could reach
// another process given that pid since. The live tests above drive the real kill.
func TestChildCancelOfAGroupGoneAlreadyAnswersProcessDone(t *testing.T) {
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
