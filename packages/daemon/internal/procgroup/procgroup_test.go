package procgroup

import (
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

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// testMoment is where a test's fake clock stands until the test moves it.
var testMoment = time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC)

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
// the grace adds nothing to the run. In the second case the test moves the context's fake clock
// past the deadline only once the leader has exited and been reaped, and before WaitDelay lets
// Run return: a caller that read a timeout from the deadline having passed would call that
// command's own exit a timeout.
func TestKilledIsWhetherTheContextsEndSignaledTheGroup(t *testing.T) {
	t.Run("cut off by its deadline", func(t *testing.T) {
		clock := clocktest.New(testMoment)
		ctx, cancel := clock.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "30")
		killed := Configure(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start the command: %v", err)
		}

		clock.Advance(2 * time.Second)
		err := cmd.Wait()

		if !killed() {
			t.Errorf("killed() = false for a command its deadline cut off (Run = %v)", err)
		}
	})
	t.Run("exited before its deadline, a child holding its pipe past it", func(t *testing.T) {
		clock := clocktest.New(testMoment)
		budget := WaitDelay - 100*time.Millisecond
		ctx, cancel := clock.WithTimeout(context.Background(), budget)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & exit 0")
		var out testlog.Buffer
		cmd.Stdout = &out
		killed := Configure(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start the command: %v", err)
		}
		leader := cmd.Process.Pid
		// The backgrounded sleep is still in the command's own process group, which outlives the
		// reaped leader while it does.
		defer syscall.Kill(-leader, syscall.SIGKILL)
		ran := make(chan error, 1)
		go func() { ran <- cmd.Wait() }()
		// Wait reaps the leader as soon as it exits, and only then waits on the pipe the sleep
		// holds, so a leader gone from /proc is one whose own exit Wait has already seen.
		testwait.Eventually(t, "the command's leader to exit and be reaped", func() bool {
			_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(leader)))
			return errors.Is(err, os.ErrNotExist)
		})
		clock.Advance(budget)
		select {
		case err := <-ran:
			t.Fatalf("Run = %v returned before its deadline passed: nothing held the pipe past it, so this run proves nothing", err)
		default:
		}
		err := testwait.Receive(t, "Run's return once WaitDelay gave up on the pipe", ran)

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
	clock := clocktest.New(testMoment)
	ctx, cancel := clock.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.Cancel = func() error { return syscall.EPERM }
	cmd.WaitDelay = WaitDelay
	killed := KilledByContext(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}

	clock.Advance(time.Second)
	err := cmd.Wait()

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

// Run starts cmd through the caller's Starter and calls its done only once the wait has read the
// exit, which is what keeps a PID 1 reaper's hold on the child until then; a start that fails is
// answered as it is, with no wait and no done; NoReaper is cmd.Run's own start; and no Starter at
// all panics before anything starts, rather than run the command under one nobody chose.
func TestRunStartsThroughTheStarterAndIsDoneOnceTheExitIsRead(t *testing.T) {
	t.Run("a starter", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 3")
		starts := 0
		var stateAtDone *os.ProcessState
		err := Run(cmd, func(started *exec.Cmd) (func(), error) {
			starts++
			if started != cmd {
				t.Errorf("the starter was given %v, want the command Run was", started)
			}
			return func() { stateAtDone = started.ProcessState }, started.Start()
		})
		if Err(err) != nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
			t.Fatalf("Run = %v with exit %v, want the command's own exit 3", err, cmd.ProcessState)
		}
		if starts != 1 {
			t.Errorf("the starter ran %d times, want once", starts)
		}
		if stateAtDone == nil || stateAtDone.ExitCode() != 3 {
			t.Errorf("done ran with the exit %v, want it called only once the wait had read exit 3", stateAtDone)
		}
	})
	t.Run("a start that fails", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 0")
		refused := errors.New("the guard refused the start")
		doneCalled := false
		err := Run(cmd, func(*exec.Cmd) (func(), error) { return func() { doneCalled = true }, refused })
		if !errors.Is(err, refused) || cmd.Process != nil || doneCalled {
			t.Fatalf("Run = %v, process %v, done called %v; want the start's own error, no process and no done", err, cmd.Process, doneCalled)
		}
	})
	t.Run("NoReaper", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 4")
		if err := Run(cmd, NoReaper); Err(err) != nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 4 {
			t.Fatalf("Run(NoReaper) = %v with exit %v, want the command's own exit 4", err, cmd.ProcessState)
		}
	})
	t.Run("no starter", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "exit 0")
		defer func() {
			if recover() == nil || cmd.Process != nil {
				t.Fatalf("Run(nil) started %v and did not panic, want a panic before any start", cmd.Process)
			}
		}()
		_ = Run(cmd, nil)
	})
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

// childRun is one child started under ConfigureOn and still running: stop ends its context, ran
// carries what Wait returned, killed is ConfigureOn's answer, clock is the fake its grace is measured
// on, and out holds everything the child printed, which a failure names.
type childRun struct {
	cmd    *exec.Cmd
	ran    <-chan error
	stop   context.CancelFunc
	killed func() bool
	clock  *clocktest.Fake
	out    *testlog.Buffer
}

// startChild starts `sh -c script sh args…` under ConfigureOn, its grace measured on a fake clock
// that moves only when the row moves it, and waits for the child to report that it is running by
// creating the file args[0] names. The stop is a cancel rather than a deadline: Cancel does the
// same work either way, and killed answers the same. The child's output goes to a pipe, as a real
// command's does, so a helper that outlived the stop would hold Run open the way git-remote-https
// does.
func startChild(t *testing.T, script string, args ...string) childRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Dir = filepath.Dir(args[0])
	var out testlog.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	clock := clocktest.New(testMoment)
	killed := ConfigureOn(cmd, clock)
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
	return childRun{cmd: cmd, ran: ran, stop: cancel, killed: killed, clock: clock, out: &out}
}

// wait answers what the child's Wait returned; a failure prints what the child printed.
func (c childRun) wait(t *testing.T) error {
	t.Helper()
	var err error
	testwait.Until(t, "the run's end after the stop", func() (string, bool) {
		select {
		case err = <-c.ran:
			return "", true
		default:
			return "the child printed " + strconv.Quote(c.out.String()), false
		}
	})
	return err
}

// graceArmed waits for the stop's grace to be armed and requires it to end StopGrace after the
// moment the fake stands at. Only a row whose group outlives the SIGTERM may ask: a group that
// leaves on it ends the grace, which is then never pending.
func (c childRun) graceArmed(t *testing.T) {
	t.Helper()
	if armed := c.clock.AwaitArmed(t, "the stop's grace", 1); len(armed) != 1 || !armed[0].Equal(c.clock.Now().Add(StopGrace)) {
		t.Fatalf("the stop armed %v, want its one grace of %s from %v", armed, StopGrace, c.clock.Now())
	}
}

// runningInsideTheGrace fails the row if the run has already ended, which inside the grace only
// a stop that did not wait the grace out could have done to a group that outlives its SIGTERM.
func (c childRun) runningInsideTheGrace(t *testing.T) {
	t.Helper()
	select {
	case err := <-c.ran:
		t.Fatalf("the run ended inside the %s grace, though its group outlives the SIGTERM: the SIGKILL must follow the grace, not the SIGTERM (Run = %v, printed %q)", StopGrace, err, c.out.String())
	default:
	}
}

// recorded waits for the file a child's SIGTERM handler writes, the child's own report that the
// stop's SIGTERM reached it. A row moves the grace past its end only after this report: the fake's
// move is instant, so a SIGKILL sent then could otherwise reach the child before its handler ran.
func recorded(t *testing.T, who, path string) {
	t.Helper()
	testwait.Eventually(t, who+" to record the stop's SIGTERM in "+path, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
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

	child.stop()
	child.graceArmed(t)
	recorded(t, "the child", caught)
	child.runningInsideTheGrace(t)
	child.clock.Advance(StopGrace)
	err := child.wait(t)

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
// reports that it is running, and on SIGTERM it removes the lock and leaves, but only once the row
// has written the let-go file its handler waits for. While it waits, the group outlives its
// SIGTERM, so the stop's grace is pending on the fake clock and the row reads it there
// (graceArmed). A grace that left the injected clock, such as a real timer or a real sleep of
// StopGrace, is never armed there, so that wait fails and names the grace; a handler that left at
// once would let such a grace still end the run, at the group's exit or a real second later. The
// lock must be gone by the time the run ends, and the run must end while the grace's clock has not
// moved at all: a stop that sat out its grace on the injected clock never returns, which is what
// tells a grace that ends as soon as the group is gone from one slept in full on every stop.
func TestAChildThatHandlesSIGTERMRemovesItsLockBeforeTheRunEnds(t *testing.T) {
	dir := t.TempDir()
	ready, lock := filepath.Join(dir, "ready"), filepath.Join(dir, "index.lock")
	letGo := filepath.Join(dir, "let-go")
	// The handler's wait for letGo ($3) has no bound of its own: startChild kills the group once
	// the test ends, so a row that fails before it lets the child go leaves nothing running.
	child := startChild(t, `: >"$2"; trap 'until [ -e "$3" ]; do sleep 0.01; done; rm -f "$2"; exit 0' TERM; : >"$1"; `+childLives, ready, lock, letGo)

	child.stop()
	// The handler holds the group past its SIGTERM until the row lets it go, so the grace is
	// armed on the fake clock from the moment the stop began.
	child.graceArmed(t)
	if err := os.WriteFile(letGo, nil, 0o600); err != nil {
		t.Fatalf("let the child's SIGTERM handler go: %v", err)
	}
	// The grace's fake clock never moves here, so a run that ends is one the group's own exit
	// ended, not the grace running out.
	err := child.wait(t)

	// The child took the lock before it reported that it was running, which startChild waited for,
	// so a lock that is gone here is one the child's own SIGTERM handler removed.
	if _, statErr := os.Stat(lock); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the lock the child held is still there (%v) after the run ended: the stop gave it no SIGTERM to remove it on (Run = %v)", statErr, err)
	}
	if !child.killed() {
		t.Errorf("killed() = false for a command the end of its context stopped (Run = %v)", err)
	}
}

// A child that ignores SIGTERM is still gone when the run ends: the grace is a grace and not a
// reprieve. It is alive while the grace runs, which is what pins the grace's length — a stop that
// killed at once would have ended the run by then — and the group's SIGKILL at the grace's end is
// what ends it.
func TestAChildThatIgnoresSIGTERMIsKilledAfterTheGrace(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	child := startChild(t, `trap '' TERM; : >"$1"; `+childLives, ready)

	child.stop()
	child.graceArmed(t)
	child.runningInsideTheGrace(t)
	child.clock.Advance(StopGrace)
	err := child.wait(t)

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
		child.graceArmed(t)
		child.clock.Advance(StopGrace)
		err := child.wait(t)

		helperGone(t, helper, marker, err)
	})

	t.Run("a helper that handles SIGTERM gets it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "helper-caught")
		pidFile, trapped := filepath.Join(dir, "helper.pid"), filepath.Join(dir, "helper-trapped")
		child := startChild(t, leader, ready, handlesSIGTERM, marker, pidFile, trapped)
		helper := pidOf(t, pidFile)

		child.stop()
		child.graceArmed(t)
		// The group's SIGTERM must reach a forked helper too, not only the group's SIGKILL.
		recorded(t, "the helper", marker)
		child.clock.Advance(StopGrace)
		err := child.wait(t)

		helperGone(t, helper, marker, err)
	})
}

// helperGone waits for the forked helper to be gone once the run has ended: a stop that reached
// the leader alone leaves it running.
func helperGone(t *testing.T, helper int, marker string, ran error) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the helper (pid %d) to be gone after the run ended", helper), func() (string, bool) {
		return fmt.Sprintf("the helper is still running (Run = %v)", ran), !runningUnder(t, helper, marker)
	})
}
