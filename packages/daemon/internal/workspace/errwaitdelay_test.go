package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// procgroup.Err is execRunner's one classification of a command's error: nil for an ordinary exit
// or for exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay), either way read
// from cmd.ProcessState.ExitCode(). A fake git that exits 0 while a backgrounded child keeps its
// stdout pipe open proves that wiring end to end: RunChecked must succeed with ExitCode 0, not
// report the command's own clean exit as a failure. The runner's Timeout is on a fake clock that
// never moves, so the command's own exit is the only end this run has.
func TestRunCheckedSucceedsWhenACommandExitsZeroWithAChildStillHoldingItsOutputOpen(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho done\nsleep 3 & echo $! >" + pidFile + "\nexit 0\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })

	run := execRunner{timeout: 5 * time.Second, tools: map[string]string{"git": fakeGit}, clock: clocktest.New(stopMoment)}

	result, err := RunChecked(context.Background(), run, []string{"git"}, nil, dir)
	if err != nil {
		t.Fatalf("RunChecked = %v, want no error: the command exited 0 on its own", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

// A command that exits on its own inside its Timeout did not time out, however long
// procgroup.WaitDelay then holds Run open for a child still holding its output pipe. The Timeout
// is on a fake clock, which the row moves past it once the git has exited and while that child
// still holds Run open: a Run that read TimedOut from the deadline having passed would report this
// git's own exit 128 as a timeout, and a clone that failed transiently inside the last WaitDelay of
// its budget, git-remote-https still holding the pipe, would read as timed out, which
// transientCloneFailure never retries.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	dir := t.TempDir()
	pidFile, leaderFile := filepath.Join(dir, "sleep.pid"), filepath.Join(dir, "git.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: unable to access' >&2\nsleep 30 & echo $! >" + pidFile + "\n" +
		"echo $$ >" + leaderFile + ".part && mv " + leaderFile + ".part " + leaderFile + "\nexit 128\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	timeout := time.Second
	clock := clocktest.New(stopMoment)
	run := execRunner{timeout: timeout, tools: map[string]string{"git": fakeGit}, clock: clock}

	ran := inFlight(func() runAnswer {
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		return runAnswer{result, err}
	})
	reaped(t, "the git", leaderFile)
	heldOpen(t, ran)
	clock.Advance(timeout)
	got := testwait.Receive(t, "the run to end once WaitDelay gave up on the pipe the git's child holds", ran)

	if got.err != nil {
		t.Fatalf("Run = %v, want the command's own exit", got.err)
	}
	if got.result.TimedOut || got.result.ExitCode != 128 {
		t.Errorf("Run = exit %d, TimedOut %t; want exit 128, not timed out: git failed on its own inside its %s Timeout, which ran out only after", got.result.ExitCode, got.result.TimedOut, timeout)
	}
}

// A command its caller's context ended was not cut off by its own Timeout: TimedOut is the
// Timeout's kill alone. A Run that read a timeout from the kill alone would call a clone its
// caller stopped a timeout, which transientCloneFailure never retries. The Timeout is on a fake
// clock that never moves, so the caller's cancel is the only stop this run has.
func TestRunDoesNotReportACommandItsCallerStoppedAsTimedOut(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n: >" + started + "\nexec sleep 30\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	run := execRunner{timeout: time.Minute, tools: map[string]string{"git": fakeGit}, clock: clocktest.New(stopMoment)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := inFlight(func() runAnswer {
		result, err := run.Run(ctx, Command{Dir: dir, Argv: []string{"git"}, Timeout: time.Minute})
		return runAnswer{result, err}
	})
	// The cancel lands once the command runs, never on one not yet started.
	exists(t, "the git to report that it is running", started)
	cancel()
	got := testwait.Receive(t, "the run to end once its caller's context ended", ran)

	if got.err != nil {
		t.Fatalf("Run = %v, want the killed command's own exit", got.err)
	}
	if got.result.TimedOut {
		t.Errorf("Run = exit %d, TimedOut true; want not timed out: the caller's context ended the command inside its minute", got.result.ExitCode)
	}
}

// commandAnswer is what RunCommand answered.
type commandAnswer struct {
	out string
	err error
}

// A RunCommand command that exits on its own did not time out, however long procgroup.WaitDelay
// then holds the run open for a child still holding its output pipes: its exit is its answer. In
// the first case the budget, on a fake clock, runs out once the command has exited and while that
// child holds the run open; a run that read a timeout from the deadline having passed would report
// jj's own exit 3 as a timeout. In the second the budget never runs out; a run that took
// exec.ErrWaitDelay for a failure would report that success as an error. In both the child is
// still running once the run returned, so WaitDelay, not the child's end, let the run return.
// (Ported with RunCommand's own semantics from the shim's runAdoptionCommand, which the adoption
// now reaches through RunCommand.)
func TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit       int
		pastBudget bool
		wantErr    string
	}{
		"an exit 3, the pipes held past the budget": {3, true, "Command failed (exit 3)"},
		"an exit 0 inside the budget":               {0, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile, leaderFile := filepath.Join(dir, "lingering.pid"), filepath.Join(dir, "leader.pid")
			t.Cleanup(func() { killPIDFile(t, pidFile) })
			budget := time.Second
			clock := clocktest.New(stopMoment)
			ctx, cancel := clock.WithTimeout(context.Background(), budget)
			defer cancel()
			script := `echo answered; echo refused >&2; sleep 30 & echo $! >"$1"; echo $$ >"$2.part" && mv "$2.part" "$2"; exit ` + strconv.Itoa(testCase.exit)

			ran := inFlight(func() commandAnswer {
				out, err := runCommandOn(ctx, clock, procgroup.NoReaper, budget, clock.Now(), []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", pidFile, leaderFile})
				return commandAnswer{out, err}
			})
			reaped(t, "the command", leaderFile)
			if testCase.pastBudget {
				heldOpen(t, ran)
				clock.Advance(budget)
			}
			got := testwait.Receive(t, "the run to end once WaitDelay gave up on the pipes the command's child holds", ran)
			if lingering, ok := readPID(pidFile); !ok || syscall.Kill(lingering, 0) != nil {
				t.Fatalf("the command's child had exited by the time the run returned (%q, %v): nothing held the pipe, so this run proves nothing", got.out, got.err)
			}
			if testCase.wantErr == "" {
				if got.err != nil || got.out != "answered" {
					t.Errorf("RunCommand = %q, %v; want \"answered\" and no error: the command exited 0 on its own", got.out, got.err)
				}
				return
			}
			if got.err == nil || !strings.Contains(got.err.Error(), testCase.wantErr) || strings.Contains(got.err.Error(), "timed out") {
				t.Errorf("RunCommand = %q, %v; want an error naming %q and no timeout: the command exited on its own inside its %s budget",
					got.out, got.err, testCase.wantErr, budget)
			}
		})
	}
}

// The commands of one save, one restore, or one adoption share a single budget, so a command can
// find it already spent before it starts: that is the budget running out, reported as a timeout,
// never a failure to run.
func TestRunCommandReportsABudgetSpentBeforeItStartedAsATimeout(t *testing.T) {
	clock := clocktest.New(stopMoment)
	ctx, cancel := clock.WithDeadline(context.Background(), stopMoment.Add(-time.Second))
	defer cancel()

	_, err := runCommandOn(ctx, clock, procgroup.NoReaper, time.Second, clock.Now(), []string{"PATH=" + os.Getenv("PATH")}, t.TempDir(), nil, []string{"sh", "-c", "exit 0"})

	if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
		t.Errorf("RunCommand under a spent budget = %v, want \"Command timed out after 1 s …\"", err)
	}
}

// A command RunCommand's caller stopped was not cut off by the budget: the restore runs under the
// launcher's own context (internal/launcher), so a launcher stopped mid-restore must read the
// command's own exit, never a timeout naming a budget that had not run out. The cancel lands once
// the command runs, never on one not yet started.
func TestRunCommandDoesNotReportACommandItsCallerStoppedAsATimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := inFlight(func() error {
		_, err := RunCommand(ctx, procgroup.NoReaper, time.Minute, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", `: >"$1"; exec sleep 30`, "sh", marker})
		return err
	})
	exists(t, "the command to report that it is running", marker)
	cancel()
	err := testwait.Receive(t, "the run to end once its caller's context ended", ran)

	if err == nil || strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "Command failed (exit -1)") {
		t.Errorf("RunCommand = %v, want \"Command failed (exit -1)\": the caller's context ended the command inside its minute", err)
	}
}

// reaped waits until the command whose pid the script wrote to pidFile has exited and its run's
// own wait has reaped it, after which kill answers ESRCH for that pid.
func reaped(t *testing.T, what, pidFile string) {
	t.Helper()
	pid := testwait.Until(t, what+" to write its pid", func() (int, bool) { return readPID(pidFile) })
	testwait.Eventually(t, what+" to exit and be reaped", func() bool {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	})
}

// heldOpen fails the row when the run has already returned: once its command exited, only the
// child still holding the command's pipes keeps the run open (procgroup.WaitDelay), and a run that
// nothing held there leaves no moment for the row's deadline to pass in, so it proves nothing.
func heldOpen[T any](t *testing.T, ran <-chan T) {
	t.Helper()
	select {
	case got := <-ran:
		t.Fatalf("the run returned %+v as its command exited: nothing held the pipe past that exit, so this run proves nothing", got)
	default:
	}
}

// readPID is the pid pidFile holds, and whether it holds one.
func readPID(pidFile string) (int, bool) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	if pid, ok := readPID(pidFile); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
