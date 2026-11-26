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
	"sync/atomic"
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
// for a command whose own process exited first, though WaitDelay then held Run open for a child
// still holding the command's output pipe. The first case's command is a loop no timer ends
// (childLives), which handles neither signal of Cancel's stop, so its group is gone as soon as the
// SIGTERM lands and the grace adds nothing to the run; its deadline passes once the command has
// started (deadlineOnceStarted). The second case's leader exits at once and leaves the same loop
// in its group, holding the pipe until the row's cleanup kills the group. Its deadline is
// answerBudget away, so it cannot pass while the leader is still exiting, however loaded the
// machine: a deadline that passed between the leader's reap and Wait's read of its context would
// still meet Cancel, whose stop the child in the group takes, and killed would be true
// (Configure's doc). No fact outside os/exec orders that window, so no row here asserts either
// answer there.
func TestKilledIsWhetherTheContextsEndSignaledTheGroup(t *testing.T) {
	t.Run("cut off by its deadline", func(t *testing.T) {
		started := filepath.Join(t.TempDir(), "started")
		cmd := exec.CommandContext(deadlineOnceStarted(t, started), "sh", "-c", `: >"$1"; `+childLives, "sh", started)
		killed := Configure(cmd)
		t.Cleanup(func() { killGroupOf(cmd) })

		err := cmd.Run()

		if _, statErr := os.Stat(started); cmd.Process == nil || statErr != nil {
			t.Fatalf("Run = %v, and the command never reported that it started (%v), so its deadline passed at no fact of the run", err, statErr)
		}
		if !killed() {
			t.Errorf("killed() = false for a command its deadline cut off (Run = %v)", err)
		}
	})
	t.Run("exited before its deadline, a child holding its pipe past its exit", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), answerBudget)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", childLives+" & exit 0")
		var out bytes.Buffer
		cmd.Stdout = &out
		killed := Configure(cmd)
		// The loop is still in the command's own process group, which outlives the reaped leader
		// while the loop does; nothing but this kill ends it.
		t.Cleanup(func() { killGroupOf(cmd) })

		started := time.Now()
		err := cmd.Run()
		elapsed := time.Since(started)
		if cmd.Process == nil {
			t.Fatalf("Run = %v before the command started, so this run proves nothing", err)
		}

		if killed() {
			t.Errorf("killed() = true for a command that exited on its own while its %s deadline was far away (Run = %v after %s)", answerBudget, err, elapsed)
		}
		if elapsed < WaitDelay {
			t.Fatalf("Run = %v returned after %s, inside WaitDelay: nothing held the pipe past the leader's exit, so this run proves nothing", err, elapsed)
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Errorf("Run = %v, want exec.ErrWaitDelay: the leader exited 0 and the loop held the pipe past WaitDelay", err)
		}
	})
}

// answerBudget is a deadline no command of these rows that exits on its own comes near, however
// loaded the machine, as the gate rigs' budget of the same name is (internal/daemon): a row that
// asserts no kill gives its command this deadline, so the deadline cannot pass while the command
// is still exiting. Such a row returns at WaitDelay, long before it.
const answerBudget = 20 * time.Second

// killGroupOf kills cmd's process group, once cmd has started: the end of a row's own child, left
// running past its assertions by design or by a failure.
func killGroupOf(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// deadlineOnceStarted is a context whose deadline passes, as a real deadline does
// (testwait.StandInDeadline), once the file marker names exists, which a row's command writes as
// its first act: a deadline that passed at a time could pass before Start on a loaded machine, and
// the row would cut no running command. Where the command never writes marker, the deadline passes
// at the end of a far bound (answerBudget), so the row's run still returns and fails by its own
// checks. The context ends with the row's cleanup.
func deadlineOnceStarted(t *testing.T, marker string) context.Context {
	t.Helper()
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	bound := testwait.Bounded(t, answerBudget, time.Second)
	go func() {
		testwait.Holds(bound, testwait.Poll, func() bool {
			_, err := os.Stat(marker)
			return err == nil || ctx.Err() != nil
		})
		pass(nil)
	}()
	return ctx
}

// A Cancel whose signal fails with anything but "already gone" still counts as the context's end
// killing the command: exec then kills the process itself once WaitDelay has passed (watchCtx).
// A killed that counted only a Cancel that succeeded would call a command its deadline ended one a
// signal ended (`signal: killed`, procgroup.Err), where the old reading of the deadline called it a
// timeout. The deadline passes once the command has started (deadlineOnceStarted).
func TestKilledCountsACancelWhoseSignalFailed(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	cmd := exec.CommandContext(deadlineOnceStarted(t, started), "sh", "-c", `: >"$1"; `+childLives, "sh", started)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	cmd.Cancel = func() error { return syscall.EPERM }
	cmd.WaitDelay = WaitDelay
	killed := KilledByContext(cmd)

	err := cmd.Run()

	if _, statErr := os.Stat(started); cmd.Process == nil || statErr != nil {
		t.Fatalf("Run = %v, and the command never reported that it started (%v), so its deadline passed at no fact of the run", err, statErr)
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
// A command a signal ended is the exception: it has no exit of its own, so its *exec.ExitError is
// returned unchanged, naming the signal. Anything else — the command never started, or Run failed
// some other way — is unchanged.
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
	killed := exec.Command("sh", "-c", "kill -TERM $$").Run()
	var signalled *exec.ExitError
	if !errors.As(killed, &signalled) || signalled.ProcessState.Exited() {
		t.Fatalf("a shell that sent itself SIGTERM ran to %v, want an *exec.ExitError for a signal to test against", killed)
	}
	if err := Err(killed); err != killed || err.Error() != "signal: terminated" {
		t.Errorf("Err(%v) = %v, want it unchanged (signal: terminated): a killed command has no exit to read", killed, err)
	}
	if err := Err(fmt.Errorf("run widget: %w", killed)); !errors.Is(err, killed) {
		t.Errorf("Err on a wrapped signal death = %v, want it unchanged", err)
	}
}

// Ended answers an exit of the command's own as its *exec.ExitError, with the status it chose, and
// a signal's death as the signal's words and no ExitError, wrapped or not; anything else is
// neither. Its String is the slot a failure's message fills: `exit 3`, or the signal's words. An
// Ended that answered a signal's ExitError would hand its caller the status -1, which every such
// caller once printed as `exit -1` or took for an answer.
func TestEndedTellsAnExitOfTheCommandsOwnFromASignalsDeath(t *testing.T) {
	exitedThree := exec.Command("sh", "-c", "exit 3").Run()
	terminated := exec.Command("sh", "-c", "kill -TERM $$").Run()
	for _, row := range []struct {
		name       string
		err        error
		wantStatus int // -2: no *exec.ExitError
		wantSignal string
		wantString string
	}{
		{"no error", nil, -2, "", ""},
		{"an exit of its own", exitedThree, 3, "", "exit 3"},
		{"a wrapped exit of its own", fmt.Errorf("run widget: %w", exitedThree), 3, "", "exit 3"},
		{"a signal's death", terminated, -2, "signal: terminated", "signal: terminated"},
		{"a wrapped signal's death", fmt.Errorf("run widget: %w", terminated), -2, "signal: terminated", "signal: terminated"},
		{"a start that failed", exec.Command("/nonexistent/widget").Run(), -2, "", ""},
		{"exec.ErrWaitDelay", exec.ErrWaitDelay, -2, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			end := Ended(row.err)
			status := -2
			if end.Exited != nil {
				status = end.Exited.ExitCode()
			}
			if status != row.wantStatus || end.Signal != row.wantSignal || end.String() != row.wantString {
				t.Errorf("Ended(%v) = status %d, signal %q, String %q; want status %d (-2: no ExitError), signal %q, String %q",
					row.err, status, end.Signal, end.String(), row.wantStatus, row.wantSignal, row.wantString)
			}
		})
	}
}

// Said puts what a command a signal ended printed beneath the signal, trimmed and wrapped or not,
// and leaves every other error as it is: an exit (its caller reads the output beside the status),
// a start that failed, nil, and a signal's death that printed nothing or only space.
func TestSaidPutsWhatACommandASignalEndedPrintedBeneathTheSignal(t *testing.T) {
	terminated := exec.Command("sh", "-c", "kill -TERM $$").Run()
	exitedThree := exec.Command("sh", "-c", "exit 3").Run()
	notStarted := exec.Command("/nonexistent/widget").Run()
	for _, row := range []struct {
		name   string
		err    error
		output string
		want   string // "" for err unchanged
	}{
		{"a signal's death that printed", terminated, "  refused: no server running\n", "signal: terminated\nrefused: no server running"},
		{"a wrapped signal's death that printed", fmt.Errorf("run widget: %w", terminated), "refused", "run widget: signal: terminated\nrefused"},
		{"a signal's death that printed only space", terminated, " \n", ""},
		{"an exit that printed", exitedThree, "refused", ""},
		{"a start that failed", notStarted, "refused", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := Said(row.err, row.output)
			switch {
			case row.want == "" && got != row.err:
				t.Errorf("Said(%v, %q) = %v, want the error unchanged", row.err, row.output, got)
			case row.want != "" && (got == nil || got.Error() != row.want || !errors.Is(got, terminated)):
				t.Errorf("Said(%v, %q) = %v, want %q wrapping the signal's error", row.err, row.output, got, row.want)
			}
		})
	}
	if got := Said(nil, "refused"); got != nil {
		t.Errorf("Said(nil, …) = %v, want nil", got)
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

// childRun is one child started under Configure and still running: stop ends its context, ran
// carries what Wait returned, killed is Configure's answer, began answers when Configure's Cancel
// began and whether it did, and out holds everything the child printed, which a failure names.
type childRun struct {
	cmd    *exec.Cmd
	ran    <-chan error
	stop   context.CancelFunc
	killed func() bool
	began  func() (time.Time, bool)
	out    *bytes.Buffer
}

// startChild starts `sh -c script sh args…` under Configure and waits for the child to report
// that it is running by creating the file args[0] names. The stop is a cancel rather than a
// deadline so that no row's measurement holds the child's own start-up: Cancel does the same work
// either way, and killed answers the same. The child's output goes to a pipe, as a real command's
// does, so a helper that outlived the stop would hold Run open the way git-remote-https does.
// Configure's Cancel is wrapped once more, after Configure, to record when os/exec began it: the
// moment the stop under test starts, which a loaded machine can hold back from the row's cancel.
func startChild(t *testing.T, script string, args ...string) childRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Dir = filepath.Dir(args[0])
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	killed := Configure(cmd)
	var began atomic.Pointer[time.Time]
	stopGroup := cmd.Cancel
	cmd.Cancel = func() error {
		now := time.Now()
		began.Store(&now)
		return stopGroup()
	}
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
	beganAt := func() (time.Time, bool) {
		if at := began.Load(); at != nil {
			return *at, true
		}
		return time.Time{}, false
	}
	return childRun{cmd: cmd, ran: ran, stop: cancel, killed: killed, began: beganAt, out: &out}
}

// wait answers what the child's Wait returned, failing the row when the run has not ended within a
// far bound (answerBudget): a stop that reached nothing would otherwise hang the row until go
// test's own deadline. The bound is no span of the stop under test, which a row reads from its
// facts and from the production constants alone.
func (c childRun) wait(t *testing.T) error {
	t.Helper()
	bound := testwait.Bounded(t, answerBudget, time.Second)
	select {
	case err := <-c.ran:
		return err
	case <-time.After(bound):
		t.Fatalf("the run had not ended %s after the stop, and the child printed %q", bound, c.out.String())
		return nil
	}
}

// stopBound is how soon after the stop a run whose group outlives the grace must end: the grace,
// WaitDelay, and two seconds for the group's SIGKILL to land and the run to be reaped and answered.
// A stop whose grace ran long, or whose SIGKILL came late, ends the run past it.
const stopBound = StopGrace + WaitDelay + 2*time.Second

// rerunWithin is how long a row that runs again a run it cannot judge goes on doing so: a minute,
// less what its last run may spend of its own far bound (answerBudget) and a second for the row
// to fail in. It answers the moment that ends and the span.
func rerunWithin(t *testing.T) (until time.Time, within time.Duration) {
	within = testwait.Bounded(t, time.Minute, answerBudget+time.Second)
	return time.Now().Add(within), within
}

// endedInTime judges a run that ended took after the stop at stopped against stopBound, and
// answers whether the row should run again. A run inside the bound passes. A run past it fails
// where Cancel began within StopGrace of the stop, since more than StopGrace+WaitDelay+1s then
// passed between the stop's start and the run's end. Where Cancel began later, a loaded machine
// that held back os/exec's call of Cancel cannot be told from a late stop, so that run is not
// judged, and the row runs again until until, then fails naming the runs.
func (c childRun) endedInTime(t *testing.T, runs int, stopped time.Time, took time.Duration, err error, until time.Time, within time.Duration) (again bool) {
	t.Helper()
	if took <= stopBound {
		return false
	}
	began, ok := c.began()
	if ok && began.Sub(stopped) <= StopGrace {
		t.Errorf("run %d: the run ended %s after the stop, past %s (the grace, WaitDelay and two seconds), though Cancel began %s after the stop: the group's SIGKILL must follow the grace, not come later (Run = %v)", runs, took, stopBound, began.Sub(stopped), err)
		return false
	}
	when := "never began"
	if ok {
		when = fmt.Sprintf("began %s after the stop", began.Sub(stopped))
	}
	if time.Now().Before(until) {
		t.Logf("run %d is not judged: it ended %s after the stop, past %s, and Cancel %s, not within the %s grace", runs, took, stopBound, when, StopGrace)
		return true
	}
	t.Fatalf("no run of the right order in %d runs over %s: the last ended %s after the stop, past %s, and Cancel %s, not within the %s grace", runs, within, took, stopBound, when, StopGrace)
	return false
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

// processEnded is whether the process pid is gone, or has exited and waits only to be reaped: its
// state, the third field of /proc/<pid>/stat after the command in parentheses, reads Z. A process a
// row's leader left behind is reaped by whatever it was reparented to, not by the run, and
// kill(pid, 0) answers nil for it until then. A process kill still finds is never read as ended
// from a /proc entry that cannot be read: the row fails there, naming /proc, since it has no other
// way to ask. kill is asked again after such a read, because a process that went between the two
// leaves no entry, and has ended. It calls t.Fatalf, so it must be called from the goroutine
// running the test.
func processEnded(t *testing.T, pid int) bool {
	t.Helper()
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	body, err := os.ReadFile(stat)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		t.Fatalf("%s cannot be read (%v) for process %d, which kill still finds: this row reads from /proc whether a process that answers kill has exited", stat, err, pid)
	}
	_, after, ok := strings.Cut(string(body), ") ")
	return ok && strings.HasPrefix(after, "Z")
}

// watchGroupGone watches the process group whose leader is pid, every testwait.Poll, until the
// group answers kill with ESRCH, which it does once no member of it is left, its leader reaped (a
// zombie still answers), or until the watch is stopped. stop ends the watch and answers when the
// watch first saw the group gone, read after that fact, and whether it saw it at all.
func watchGroupGone(pid int) (stop func() (gone time.Time, seen bool)) {
	var gone time.Time
	stopped, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(testwait.Poll)
		defer ticker.Stop()
		for !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			select {
			case <-stopped:
				return
			case <-ticker.C:
			}
		}
		gone = time.Now()
	}()
	return func() (time.Time, bool) {
		close(stopped)
		<-done
		return gone, !gone.IsZero()
	}
}

// The end of a command's context stops its whole group with SIGTERM first and SIGKILL only after
// the grace. The child here records its SIGTERM and keeps running, so it proves both halves at
// once: the record is the SIGTERM, and the SIGKILL that finally ends it cannot have been sent
// before the grace was over. A stop that sent SIGKILL alone, as this package's did before
// dispatch://INFRA-3098, leaves a git no chance to remove the lock files it holds, since SIGKILL
// runs no handler. The SIGKILL must not come late either: the run ends within stopBound of the
// stop (endedInTime).
func TestAStoppedCommandGetsSIGTERMThenSIGKILLOverItsGroup(t *testing.T) {
	until, within := rerunWithin(t)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		ready, caught := filepath.Join(dir, "ready"), filepath.Join(dir, "caught")
		child := startChild(t, `trap 'echo term >"$2"' TERM; : >"$1"; `+childLives, ready, caught)

		stopped := time.Now()
		child.stop()
		err := child.wait(t)
		took := time.Since(stopped)

		if _, statErr := os.Stat(caught); statErr != nil {
			t.Errorf("run %d: the child recorded no SIGTERM (%v): the stop reached it with SIGKILL alone (Run = %v, printed %q)", runs, statErr, err, child.out.String())
		}
		if took < StopGrace {
			t.Errorf("run %d: the run ended %s after the stop, inside the %s grace: the SIGKILL must follow the grace, not the SIGTERM (Run = %v)", runs, took, StopGrace, err)
		}
		if !killedBySIGKILL(child.cmd.ProcessState) {
			t.Errorf("run %d: the child ended as %v, want killed by SIGKILL: it handled its SIGTERM and kept running, so the group's SIGKILL is what must end it", runs, child.cmd.ProcessState)
		}
		if !child.killed() {
			t.Errorf("run %d: killed() = false for a command the end of its context stopped (Run = %v)", runs, err)
		}
		if t.Failed() || !child.endedInTime(t, runs, stopped, took, err, until, within) {
			return
		}
	}
}

// This is the issue itself, through the one package that stops a command. A git holds .git/index.lock
// while it writes the index and removes it from its own signal handler; SIGKILL runs no handler, so
// a git killed outright leaves that lock behind and every later git and jj of the repository fails
// on it (dispatch://INFRA-3098). The child here stands in for that git: it takes a lock before it
// reports that it is running, removes it on SIGTERM and leaves with an exit of its own. The row
// reads the order of facts: the lock, which only the child's trap removes, is gone by the time the
// run ends, and the child ended by its own exit 0, which only that trap gives, so it left before
// any SIGKILL, which would have ended it by the signal instead.
//
// The row also reads that the grace ended once the group was gone, not at the end of its span
// (StopGrace): the run must end within half the grace after the stop. A run that ended later
// fails where the row's own watch saw the group gone within a quarter of the grace
// (watchGroupGone). That run then went on for more than a quarter of the grace with nothing left
// in its group to stop, so the grace did not end with the group. The quarter is the run's own
// path from the group's end to Wait's return (the stop's next poll, Cancel's return, the hand-off
// to the row), which starts no process. Where the group went later, the child's own marks tell
// why: it writes a beat on every turn of its loop and a mark first thing in its SIGTERM trap, and
// the kernel stamps each with the time it was written. A beat more than a quarter of the grace
// after Cancel began is a child still running with no SIGTERM taken, so Cancel sent its SIGTERM
// late, and the row fails. A trap within a quarter of the grace after Cancel began, where os/exec
// began Cancel within a quarter of the grace after the stop, leaves nothing a loaded machine held
// to account for the late end, and the row fails as well. A run is not judged only where the trap
// came later than that with the child's last beat inside the quarter (the machine held the child
// between its last beat and its trap), or where os/exec began Cancel more than a quarter of the
// grace after the stop; the row runs again until a run of the right order comes, within a minute.
func TestAChildThatHandlesSIGTERMRemovesItsLockBeforeTheRunEnds(t *testing.T) {
	until, within := rerunWithin(t)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		ready, lock := filepath.Join(dir, "ready"), filepath.Join(dir, "index.lock")
		// $3 is the mark the child's trap writes first, and $4 the beat it writes on every turn of its
		// loop: once the trap has run, no beat follows.
		termed, beat := filepath.Join(dir, "termed"), filepath.Join(dir, "beat")
		child := startChild(t, `: >"$2"; trap 'echo term >"$3"; rm -f "$2"; exit 0' TERM; : >"$1"; while :; do echo beat >"$4"; sleep 0.05; done`,
			ready, lock, termed, beat)
		group := watchGroupGone(child.cmd.Process.Pid)

		stopped := time.Now()
		child.stop()
		err := child.wait(t)
		took := time.Since(stopped)
		gone, seen := group()

		// The child took the lock before it reported that it was running, which startChild waited
		// for, so a lock that is gone here is one the child's own SIGTERM handler removed.
		if _, statErr := os.Stat(lock); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("run %d: the lock the child held is still there (%v) after the run ended: the stop gave it no SIGTERM to remove it on (Run = %v)", runs, statErr, err)
		}
		if state := child.cmd.ProcessState; !state.Exited() || state.ExitCode() != 0 {
			t.Errorf("run %d: the child ended as %v, want its own exit 0 from its SIGTERM trap: a stop whose SIGKILL came before the trap's exit ended it by that signal (Run = %v)", runs, state, err)
		}
		if !child.killed() {
			t.Errorf("run %d: killed() = false for a command the end of its context stopped (Run = %v)", runs, err)
		}
		if t.Failed() || took <= StopGrace/2 {
			return
		}
		if seen && gone.Sub(stopped) <= StopGrace/4 {
			t.Errorf("run %d: the run ended %s after the stop, past half the %s grace, though its group was gone %s after it, within a quarter of the grace: a group that leaves on its SIGTERM must end the grace, not sit out its span (Run = %v)", runs, took, StopGrace, gone.Sub(stopped), err)
			return
		}
		began, begun := child.began()
		trapped, trapErr := os.Stat(termed)
		if !begun || trapErr != nil {
			t.Errorf("run %d: the run ended %s after the stop, past half the %s grace, and the row cannot read when the stop began (Cancel began: %t) or when the child took its SIGTERM (%v) (Run = %v)", runs, took, StopGrace, begun, trapErr, err)
			return
		}
		cancelAt, trapAt := began.Sub(stopped), trapped.ModTime().Sub(began)
		last := "wrote no beat"
		if info, statErr := os.Stat(beat); statErr == nil {
			beatAt := info.ModTime().Sub(began)
			if beatAt > StopGrace/4 {
				t.Errorf("run %d: the run ended %s after the stop, past half the %s grace, and the child wrote a beat %s after Cancel began, past a quarter of the grace, with no SIGTERM taken: Cancel must send its SIGTERM as it begins, not later (Run = %v)", runs, took, StopGrace, beatAt, err)
				return
			}
			last = fmt.Sprintf("wrote its last beat %s after Cancel began", beatAt)
		}
		if cancelAt <= StopGrace/4 && trapAt <= StopGrace/4 {
			t.Errorf("run %d: the run ended %s after the stop, past half the %s grace, though os/exec began Cancel %s after the stop and the child took its SIGTERM %s after that, each within a quarter of the grace: a child that leaves on its SIGTERM must end the grace, not sit out its span (Run = %v)", runs, took, StopGrace, cancelAt, trapAt, err)
			return
		}
		if time.Now().Before(until) {
			t.Logf("run %d is not judged: it ended %s after the stop, past half the %s grace; os/exec began Cancel %s after the stop, and the child %s and took its SIGTERM %s after Cancel began", runs, took, StopGrace, cancelAt, last, trapAt)
			continue
		}
		t.Fatalf("no run of the right order in %d runs over %s: the last ended %s after the stop, past half the %s grace; os/exec began Cancel %s after the stop, and the child %s and took its SIGTERM %s after Cancel began", runs, within, took, StopGrace, cancelAt, last, trapAt)
	}
}

// A child that ignores SIGTERM is still gone when the run ends, and so is the grandchild it started
// in its group: the grace is a grace and not a reprieve, and the stop's SIGKILL, which comes only
// once the grace is over, goes to the whole group. The run ends after that SIGKILL, so it ends at
// least StopGrace after the stop, a span the production constant bounds from below and no load can
// shorten; procgroup keeps no record of when it sent the SIGKILL, and the child, which ignores the
// SIGTERM, cannot see it coming, so the run's own end is the fact the row reads. A stop that sent
// SIGKILL at once would end the run inside the grace, and one whose SIGKILL came late ends it past
// stopBound (endedInTime). A stop that never sent it leaves os/exec's own kill, once WaitDelay has
// passed, which ends the leader alone: the grandchild, which ignores SIGTERM too (it is started
// after the leader's trap, and inherits its disposition) and holds no pipe, would then run on, and
// the row reads that it has ended. Nothing but a SIGKILL ends it, the stop's or startChild's
// cleanup's, never a timer of its own.
func TestAChildThatIgnoresSIGTERMIsKilledAfterTheGrace(t *testing.T) {
	until, within := rerunWithin(t)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		ready, pidFile := filepath.Join(dir, "ready"), filepath.Join(dir, "grandchild.pid")
		child := startChild(t, `trap '' TERM; (`+childLives+`) </dev/null >/dev/null 2>&1 & echo $! >"$2"; : >"$1"; `+childLives, ready, pidFile)
		grandchild := pidOf(t, pidFile)

		stopped := time.Now()
		child.stop()
		err := child.wait(t)
		took := time.Since(stopped)

		if took < StopGrace {
			t.Errorf("run %d: the run ended %s after the stop, inside the %s grace: the child ignores SIGTERM, so a run that ended there was killed with no grace at all (Run = %v)", runs, took, StopGrace, err)
		}
		if !killedBySIGKILL(child.cmd.ProcessState) {
			t.Errorf("run %d: the child ended as %v, want killed by SIGKILL: it ignores SIGTERM, so nothing else can have ended it", runs, child.cmd.ProcessState)
		}
		if !child.killed() {
			t.Errorf("run %d: killed() = false for a command the end of its context stopped (Run = %v)", runs, err)
		}
		bound := testwait.Bounded(t, answerBudget, time.Second)
		if !testwait.Holds(bound, testwait.Poll, func() bool { return processEnded(t, grandchild) }) {
			t.Errorf("run %d: the grandchild the child started in its group (pid %d), which ignores SIGTERM too, still ran %s after the run ended: the stop's SIGKILL did not reach the group (Run = %v)", runs, grandchild, bound, err)
		}
		if t.Failed() || !child.endedInTime(t, runs, stopped, took, err, until, within) {
			return
		}
	}
}

// helperGoneWithin is how soon after the run ends a helper the stop reached must be gone. A stop
// that reaches the group signals the helper with the leader, before the run ends, so what is left
// is that signal's delivery: a helper still running past it was signalled late, or not at all.
const helperGoneWithin = 2 * time.Second

// A helper the command forked is in the command's own group, and both signals of the stop are sent
// to that group: the helper gets the SIGTERM, and where it ignores it, the group's SIGKILL ends it
// too. The leader ignores SIGTERM in both cases, so what ends the leader is that same SIGKILL at
// the end of the grace — and os/exec's own kill, at the end of WaitDelay, would reach the leader
// alone (watchCtx), leaving the helper running, which is a git-remote-https holding the command's
// pipes open forever. The helper is gone within helperGoneWithin of the run's end, and the run
// ends within stopBound of the stop (endedInTime).
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
		until, within := rerunWithin(t)
		for runs := 1; ; runs++ {
			dir := t.TempDir()
			ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper")
			pidFile, trapped := filepath.Join(dir, "helper.pid"), filepath.Join(dir, "helper-trapped")
			child := startChild(t, leader, ready, ignoresSIGTERM, marker, pidFile, trapped)
			helper := pidOf(t, pidFile)

			stopped := time.Now()
			child.stop()
			err := child.wait(t)
			took := time.Since(stopped)

			if !testwait.Holds(helperGoneWithin, testwait.Poll, func() bool { return !runningUnder(t, helper, marker) }) {
				t.Errorf("run %d: the helper (pid %d) was still running %s after the run ended: the stop reached the leader alone, or the group late (Run = %v)", runs, helper, helperGoneWithin, err)
			}
			if t.Failed() || !child.endedInTime(t, runs, stopped, took, err, until, within) {
				return
			}
		}
	})

	t.Run("a helper that handles SIGTERM gets it", func(t *testing.T) {
		until, within := rerunWithin(t)
		for runs := 1; ; runs++ {
			dir := t.TempDir()
			ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "helper-caught")
			pidFile, trapped := filepath.Join(dir, "helper.pid"), filepath.Join(dir, "helper-trapped")
			child := startChild(t, leader, ready, handlesSIGTERM, marker, pidFile, trapped)
			helper := pidOf(t, pidFile)

			stopped := time.Now()
			child.stop()
			err := child.wait(t)
			took := time.Since(stopped)

			if _, statErr := os.Stat(marker); statErr != nil {
				t.Errorf("run %d: the helper recorded no SIGTERM (%v): the group's SIGTERM must reach a forked helper too, not only the group's SIGKILL (Run = %v)", runs, statErr, err)
			}
			if !testwait.Holds(helperGoneWithin, testwait.Poll, func() bool { return !runningUnder(t, helper, marker) }) {
				t.Errorf("run %d: the helper (pid %d) was still running %s after the run ended (Run = %v)", runs, helper, helperGoneWithin, err)
			}
			if t.Failed() || !child.endedInTime(t, runs, stopped, took, err, until, within) {
				return
			}
		}
	})
}
