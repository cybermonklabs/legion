package procgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Configure's Cancel runs in the race window between ctx firing and the process finishing on its
// own: by the time its SIGTERM reaches the group, there may be nothing left to stop. exec's
// watchCtx goroutine treats any Cancel error other than one equivalent to os.ErrProcessDone as a
// real cancellation failure — wrapping it "exec: canceling Cmd: <err>" and returning that from
// Wait even when the command's own exit was a clean success (exec.go's watchCtx) — so a raw ESRCH
// from the kill syscall must be translated, not passed through. Nothing follows that answer: a
// Cancel whose SIGTERM found no group spends no grace and sends no SIGKILL.
func TestConfigureCancelMapsESRCHToErrProcessDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	killed := Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a trivial command: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Fatalf("wait for it to exit on its own: %v", err)
	}
	// The process, and its whole group (it is its own, and forked nothing), is already reaped:
	// Cancel's kill(-pid, SIGTERM) now finds no such process or group.
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel() on an already-finished command = %v, want an error equivalent to os.ErrProcessDone", err)
	}
	if killed() {
		t.Error("killed() = true after a Cancel that found nothing left to signal")
	}
}

// killed is true when the end of the command's context made Cancel signal its group, and false
// for a command whose own process exited first, though WaitDelay then held Run open past the
// context's deadline for a child still holding the command's output pipe. The first case's sleep
// handles neither signal of Cancel's stop, so its group is gone as soon as the SIGTERM lands and
// the grace adds nothing to the run. The second case's deadline is 100 ms short of WaitDelay, so
// its Run always returns past it, while the leader still has 900 ms to exit under load: a caller
// that read a timeout from the deadline having passed would call that command's own exit a
// timeout.
func TestKilledIsWhetherTheContextsEndSignaledTheGroup(t *testing.T) {
	t.Run("cut off by its deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "30")
		killed := Configure(cmd)

		err := cmd.Run()

		if cmd.Process == nil {
			t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
		}
		if !killed() {
			t.Errorf("killed() = false for a command its deadline cut off (Run = %v)", err)
		}
	})
	t.Run("exited before its deadline, a child holding its pipe past it", func(t *testing.T) {
		budget := WaitDelay - 100*time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & exit 0")
		var out bytes.Buffer
		cmd.Stdout = &out
		killed := Configure(cmd)

		started := time.Now()
		err := cmd.Run()
		elapsed := time.Since(started)
		if cmd.Process == nil {
			t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
		}
		// The backgrounded sleep is still in the command's own process group, which outlives the
		// reaped leader while it does.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

		// The wall clock, not ctx.Err(): under load the context's own timer can fire after Run has
		// returned, and what this run needs is that the drain outlasted the deadline.
		if elapsed < budget {
			t.Fatalf("Run = %v returned after %s, inside its %s deadline: nothing held the pipe past it, so this run proves nothing", err, elapsed, budget)
		}
		if killed() {
			t.Errorf("killed() = true for a command that exited on its own before its deadline (Run = %v)", err)
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Errorf("Run = %v, want exec.ErrWaitDelay: the leader exited 0 and the sleep held the pipe past WaitDelay", err)
		}
	})
}

// A Cancel whose signal fails with anything but "already gone" still counts as the context's end
// killing the command: exec then kills the process itself once WaitDelay has passed (watchCtx).
// A killed that counted only a Cancel that succeeded would call a command its deadline ended an
// exit of its own, exit -1, where the old reading of the deadline called it a timeout.
func TestKilledCountsACancelWhoseSignalFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.Cancel = func() error { return syscall.EPERM }
	cmd.WaitDelay = WaitDelay
	killed := KilledByContext(cmd)

	err := cmd.Run()

	if cmd.Process == nil {
		t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
	}
	if !killed() {
		t.Errorf("killed() = false for a command its deadline ended though Cancel's signal failed (Run = %v)", err)
	}
}

// Err is the one place both execRunner and the boot gate's probe runner read a command's real
// exit: nil for an ordinary exit (whatever its own code) and for exec.ErrWaitDelay (a clean exit
// whose I/O draining outlived WaitDelay), seen through a wrapped error too — a caller then reads
// cmd.ProcessState.ExitCode() for both. Had Err instead let exec.ErrWaitDelay through unchanged, a
// caller would propagate it as a real error, reporting a command that actually exited 0 as
// "run git: exec: WaitDelay expired before I/O complete" rather than reading its own exit code.
// Anything else — the command never started, or Run failed some other way — is unchanged.
func TestErrRecognizesAnOrdinaryExitAndErrWaitDelayButNothingElse(t *testing.T) {
	exited := exec.Command("false").Run()
	if exited == nil {
		t.Fatal("exec.Command(\"false\").Run() = nil, want an *exec.ExitError to test against")
	}
	if err := Err(nil); err != nil {
		t.Errorf("Err(nil) = %v, want nil", err)
	}
	if err := Err(exited); err != nil {
		t.Errorf("Err(%v) = %v, want nil: an ordinary exit is not an error to propagate", exited, err)
	}
	if err := Err(exec.ErrWaitDelay); err != nil {
		t.Errorf("Err(exec.ErrWaitDelay) = %v, want nil", err)
	}
	if err := Err(fmt.Errorf("run widget: %w", exec.ErrWaitDelay)); err != nil {
		t.Errorf("Err on a wrapped ErrWaitDelay = %v, want nil (errors.Is sees through %%w)", err)
	}
	other := errors.New("workspace command is not a tool the daemon resolved at boot")
	if err := Err(other); err != other {
		t.Errorf("Err(%v) = %v, want it unchanged", other, err)
	}
}

// The rows below write their own children, and these are the shell pieces they share. A shell
// runs a trap when the command it is waiting for ends, so a child spends its life in a loop of
// short sleeps rather than in one long one: a signal it handles is then acted on within one sleep
// of its arrival, well inside the grace. A trap of the empty action on TERM ignores SIGTERM
// outright, and a process started under that disposition inherits it, so an ignoring child's own
// sleeps ignore SIGTERM too and only SIGKILL ends such a group.
const (
	// childLives keeps a child running until a signal ends it.
	childLives = `while :; do sleep 0.05; done`
	// helperTrapped is what a forked helper writes into the file $2 names once its own trap is
	// installed, and the piece every helper script below carries: a helper is a shell of its own,
	// which reaches its trap line some time after the fork that made it returned, so a stop sent
	// before this marker appears would find the helper under the default disposition rather than
	// under the one its script asks for.
	helperTrapped = `: >"$2"; `
	// ignoresSIGTERM is a child SIGKILL alone can end. Its $1 is a path of the test's own
	// temporary directory, which it never reads and which names it in /proc (runningUnder).
	ignoresSIGTERM = `trap '' TERM; ` + helperTrapped + childLives
	// handlesSIGTERM is a child that records its SIGTERM in the file $1 names and leaves.
	handlesSIGTERM = `trap 'echo term >"$1"; exit 0' TERM; ` + helperTrapped + childLives
)

// loadSlack is what a row allows a loaded machine beyond the span it measures. It is scheduling
// noise around the grace and WaitDelay, never a span anything under test may spend: a row's own
// bound is written as the grace and the delay, with this added to it.
const loadSlack = 2 * time.Second

// childRun is one child started under Configure and still running: stop ends its context, ran
// carries what Wait returned, killed is Configure's answer, and out holds everything the child
// printed, which a failure names.
type childRun struct {
	cmd    *exec.Cmd
	ran    <-chan error
	stop   context.CancelFunc
	killed func() bool
	out    *bytes.Buffer
}

// startChild starts `sh -c script sh args…` under Configure and waits for the child to report
// that it is running by creating the file args[0] names. The stop is a cancel rather than a
// deadline so that no row's measurement holds the child's own start-up: Cancel does the same work
// either way, and killed answers the same. The child's output goes to a pipe, as a real command's
// does, so a helper that outlived the stop would hold Run open the way git-remote-https does.
func startChild(t *testing.T, script string, args ...string) childRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Dir = filepath.Dir(args[0])
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	killed := Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	// Whatever a row leaves behind when it fails: the group is killed once the test ends, so a
	// failing row costs the machine no lingering sleep loop.
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	ran := make(chan error, 1)
	go func() { ran <- cmd.Wait() }()
	testwait.Eventually(t, "the child to report that it is running", func() bool {
		_, err := os.Stat(args[0])
		return err == nil
	})
	return childRun{cmd: cmd, ran: ran, stop: cancel, killed: killed, out: &out}
}

// wait answers what the child's Wait returned, failing the row when the run has not ended within
// bound: a stop that reached nothing would otherwise hang the row until go test's own deadline.
func (c childRun) wait(t *testing.T, bound time.Duration) error {
	t.Helper()
	select {
	case err := <-c.ran:
		return err
	case <-time.After(bound):
		t.Fatalf("the run had not ended %s after the stop, and the child printed %q", bound, c.out.String())
		return nil
	}
}

// killedBySIGKILL reports whether the child's own leader process was killed by SIGKILL, which is
// what ends a child that outlives the grace.
func killedBySIGKILL(state *os.ProcessState) bool {
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// requireProc fails the row where /proc does not answer. The rows that read whether a forked
// helper is still running have no other way to ask, and a skip would leave the stop unproven.
func requireProc(t *testing.T) {
	t.Helper()
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this row reads from it whether the command's forked helper is still running", err)
	}
}

// runningUnder reports whether pid is still running as the helper marker names. /proc/<pid>/cmdline
// holds a running process's arguments and is empty for one that has exited, whether the kernel has
// freed its entry or the leader's own death left it a zombie nobody has reaped yet. The marker, a
// path of the row's own temporary directory that the helper carries as an argument, keeps the
// answer about this row's helper rather than about whatever the kernel gave that pid next.
func runningUnder(t *testing.T, pid int, marker string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), marker)
}

// pidOf reads the pid a child wrote into path.
func pidOf(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the helper's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("read the helper's pid from %q: %v", raw, err)
	}
	return pid
}

// The end of a command's context stops its whole group with SIGTERM first and SIGKILL only after
// the grace. The child here records its SIGTERM and keeps running, so it proves both halves at
// once: the record is the SIGTERM, and the SIGKILL that finally ends it cannot have been sent
// before the grace was over. A stop that sent SIGKILL alone, as this package's did before
// dispatch://INFRA-3098, leaves a git no chance to remove the lock files it holds, since SIGKILL
// runs no handler.
func TestAStoppedCommandGetsSIGTERMThenSIGKILLOverItsGroup(t *testing.T) {
	dir := t.TempDir()
	ready, caught := filepath.Join(dir, "ready"), filepath.Join(dir, "caught")
	child := startChild(t, `trap 'echo term >"$2"' TERM; : >"$1"; `+childLives, ready, caught)

	stopped := time.Now()
	child.stop()
	err := child.wait(t, StopGrace+WaitDelay+loadSlack)
	took := time.Since(stopped)

	if _, statErr := os.Stat(caught); statErr != nil {
		t.Errorf("the child recorded no SIGTERM (%v): the stop reached it with SIGKILL alone (Run = %v, printed %q)", statErr, err, child.out.String())
	}
	if took < StopGrace {
		t.Errorf("the run ended %s after the stop, inside the %s grace: the SIGKILL must follow the grace, not the SIGTERM (Run = %v)", took, StopGrace, err)
	}
	if !killedBySIGKILL(child.cmd.ProcessState) {
		t.Errorf("the child ended as %v, want killed by SIGKILL: it handled its SIGTERM and kept running, so the group's SIGKILL is what must end it", child.cmd.ProcessState)
	}
	if !child.killed() {
		t.Errorf("killed() = false for a command the end of its context stopped (Run = %v)", err)
	}
}

// This is the issue itself, through the one package that stops a command. A git holds .git/index.lock
// while it writes the index and removes it from its own signal handler; SIGKILL runs no handler, so
// a git killed outright leaves that lock behind and every later git and jj of the repository fails
// on it (dispatch://INFRA-3098). The child here stands in for that git: it takes a lock before it
// reports that it is running, removes it on SIGTERM and leaves. The lock must be gone by the time
// the run ends, and the run must end well inside the grace rather than at the end of it, which is
// what tells a grace that ends as soon as the group is gone from one slept in full on every stop.
func TestAChildThatHandlesSIGTERMRemovesItsLockBeforeTheRunEnds(t *testing.T) {
	dir := t.TempDir()
	ready, lock := filepath.Join(dir, "ready"), filepath.Join(dir, "index.lock")
	child := startChild(t, `: >"$2"; trap 'rm -f "$2"; exit 0' TERM; : >"$1"; `+childLives, ready, lock)

	stopped := time.Now()
	child.stop()
	err := child.wait(t, StopGrace+WaitDelay+loadSlack)
	took := time.Since(stopped)

	// The child took the lock before it reported that it was running, which startChild waited for,
	// so a lock that is gone here is one the child's own SIGTERM handler removed.
	if _, statErr := os.Stat(lock); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the lock the child held is still there (%v) after the run ended: the stop gave it no SIGTERM to remove it on (Run = %v)", statErr, err)
	}
	if took > StopGrace/2 {
		t.Errorf("the run ended %s after the stop, which is no sooner than half the %s grace: a child that leaves on its SIGTERM must end the grace, not sit out its whole span (Run = %v)", took, StopGrace, err)
	}
	if !child.killed() {
		t.Errorf("killed() = false for a command the end of its context stopped (Run = %v)", err)
	}
}

// A child that ignores SIGTERM is still gone when the run ends: the grace is a grace and not a
// reprieve. It is alive at half the grace, which is what pins the grace's length — a stop that
// killed at once would have ended the run by then — and gone by the grace plus WaitDelay, which is
// what keeps the whole stop inside the three seconds
// TestFetchKillsGitsWholeProcessTreeWhenItsTimeoutFires (internal/workspace) allows a killed fetch.
func TestAChildThatIgnoresSIGTERMIsKilledAfterTheGrace(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	child := startChild(t, `trap '' TERM; : >"$1"; `+childLives, ready)

	stopped := time.Now()
	child.stop()
	select {
	case err := <-child.ran:
		t.Fatalf("the run ended %s after the stop, inside half the %s grace: the child ignores SIGTERM, so a run that ended there was killed with no grace at all (Run = %v)", time.Since(stopped), StopGrace, err)
	case <-time.After(StopGrace / 2):
	}
	err := child.wait(t, StopGrace/2+WaitDelay+loadSlack)
	took := time.Since(stopped)

	if took < StopGrace {
		t.Errorf("the run ended %s after the stop, inside the %s grace (Run = %v)", took, StopGrace, err)
	}
	if !killedBySIGKILL(child.cmd.ProcessState) {
		t.Errorf("the child ended as %v, want killed by SIGKILL: it ignores SIGTERM, so nothing else can have ended it", child.cmd.ProcessState)
	}
	if !child.killed() {
		t.Errorf("killed() = false for a command the end of its context stopped (Run = %v)", err)
	}
}

// A helper the command forked is in the command's own group, and both signals of the stop are sent
// to that group: the helper gets the SIGTERM, and where it ignores it, the group's SIGKILL ends it
// too. The leader ignores SIGTERM in both cases, so what ends the leader is that same SIGKILL at
// the end of the grace — and os/exec's own kill, at the end of WaitDelay, would reach the leader
// alone (watchCtx), leaving the helper running, which is a git-remote-https holding the command's
// pipes open forever.
func TestAForkedHelperOfAStoppedCommandIsStoppedToo(t *testing.T) {
	requireProc(t)
	// $1 is the ready file, $2 the helper's own script, $3 the path the helper carries, $4 the file
	// the leader writes the helper's pid into, and $5 the marker the helper writes once its own
	// trap is installed (helperTrapped). The helper is forked before the leader ignores SIGTERM,
	// because a shell cannot trap a signal that was already ignored when it started: the
	// disposition is inherited across fork and exec, so a helper forked after that line could only
	// ignore SIGTERM too, whatever its own script says. The fork returns as soon as the helper
	// exists, which is before that helper's own shell has run its trap line, so the leader waits
	// for the helper's marker before it writes the ready file: the stop lands after the ready file,
	// which the leader writes once all three dispositions are in place. A stop that raced the
	// helper's trap would say nothing about the stop under test — the handling helper would die of
	// the group's SIGTERM with no record of it, and the ignoring helper would be ended by that same
	// SIGTERM rather than outliving it, which is the opposite error and just as silent.
	leader := `sh -c "$2" sh "$3" "$5" & echo $! >"$4"; trap '' TERM; until [ -e "$5" ]; do sleep 0.01; done; : >"$1"; ` + childLives

	t.Run("a helper that ignores SIGTERM is killed with the group", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper")
		pidFile, trapped := filepath.Join(dir, "helper.pid"), filepath.Join(dir, "helper-trapped")
		child := startChild(t, leader, ready, ignoresSIGTERM, marker, pidFile, trapped)
		helper := pidOf(t, pidFile)

		child.stop()
		err := child.wait(t, StopGrace+WaitDelay+loadSlack)

		if !testwait.Holds(loadSlack, testwait.Poll, func() bool { return !runningUnder(t, helper, marker) }) {
			t.Errorf("the helper (pid %d) was still running %s after the run ended: the stop reached the leader alone (Run = %v)", helper, loadSlack, err)
		}
	})

	t.Run("a helper that handles SIGTERM gets it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "helper-caught")
		pidFile, trapped := filepath.Join(dir, "helper.pid"), filepath.Join(dir, "helper-trapped")
		child := startChild(t, leader, ready, handlesSIGTERM, marker, pidFile, trapped)
		helper := pidOf(t, pidFile)

		child.stop()
		err := child.wait(t, StopGrace+WaitDelay+loadSlack)

		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("the helper recorded no SIGTERM (%v): the group's SIGTERM must reach a forked helper too, not only the group's SIGKILL (Run = %v)", statErr, err)
		}
		if !testwait.Holds(loadSlack, testwait.Poll, func() bool { return !runningUnder(t, helper, marker) }) {
			t.Errorf("the helper (pid %d) was still running %s after the run ended (Run = %v)", helper, loadSlack, err)
		}
	})
}
