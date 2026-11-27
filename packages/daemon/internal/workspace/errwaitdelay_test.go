package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// procgroup.Err is execRunner's one classification of a command's error: nil for an ordinary exit
// or for exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay), either way read
// from cmd.ProcessState.ExitCode(). A fake git that exits 0 while a child it started keeps its
// stdout pipe open proves that wiring end to end: RunChecked must succeed with ExitCode 0, not
// report the command's own clean exit as a failure. The child runs until the test's cleanup
// releases it (heldChild), so every run holds the pipe past WaitDelay.
//
// The runner's budget is the rest of the test binary's deadline, so a loaded machine that slows
// the fake git does not cut it off. Main's form gave the runner 5 s from before the git started,
// and that bound stays (runBound), on the runner's own time and no stand-in's. The fake git
// records when the kernel made its process (testexec.MarkStart) and the moment just before it
// exits (testexec.MarkNow), and the span between the two is the git's own run. What is left of the
// call is the runner's: from the call to that start, and from the git's exit to the return. Of the
// second, procgroup.WaitDelay is not the runner's to spend: os/exec waits that long after the
// git's reap for the child to close the pipe, and main's budget, which ended at that reap, never
// counted it either. A runner that spends main's 5 s before it starts the git, or after the git
// has exited, fails here.
func TestRunCheckedSucceedsWhenACommandExitsZeroWithAChildStillHoldingItsOutputOpen(t *testing.T) {
	const runBound = 5 * time.Second
	dir := t.TempDir()
	fakeGit, mark, exited := filepath.Join(dir, "git"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
	script := "#!/bin/sh\n" + testexec.MarkStart(shellprefix.Literal(mark)) + "\necho done\n" + heldChild(t, false) + "\n" + testexec.MarkNow(shellprefix.Literal(exited)) + "\nexit 0\n"
	if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	run := NewRunner(testwait.Remaining(t), map[string]string{"git": fakeGit})

	called := testexec.SinceBoot(t)
	started := time.Now()
	result, err := RunChecked(context.Background(), run, []string{"git"}, nil, dir)
	returned := testexec.SinceBoot(t)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("RunChecked = %v, want no error: the command exited 0 on its own", err)
	}
	if elapsed < procgroup.WaitDelay {
		t.Fatalf("RunChecked returned after %s, inside WaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	began, after, held := testexec.StartedAfter(t, mark, called), returned-exitMark(t, exited), procgroup.WaitDelayNow()
	if own := began + after - held; own > runBound {
		t.Errorf("RunChecked spent %s of its own, past main's %s: %s from the call to the git's start, and %s from the git's exit to the return, less the %s os/exec holds the child's pipe (procgroup.WaitDelay)",
			own, runBound, began, after, held)
	}
}

// A command that exits on its own inside its Timeout did not time out, however long
// procgroup.WaitDelay then holds Run open for a child it left in its process group still holding
// its output pipe: its exit is its answer. This is main's form: the child stays in the git's
// group, and the Timeout is main's, 100 ms short of WaitDelay, so a run whose git exits inside it
// returns past it. A Run that read TimedOut from the deadline having passed, or that, once its
// Timeout had passed, read a group that still answers after the wait, or a stop that found a
// member of it, as a kill, would report this git's own exit 128 as a timeout. The row whose child
// leaves the group (TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline)
// cannot see the last two: its group is gone by then.
//
// Main's form judged every run, and a loaded machine can bring the git's reap, or Wait's read of
// its context after the reap, past 900 ms. The Timeout then passes inside Wait,
// procgroup.Configure's Cancel stops the group, the child takes its SIGTERM, and a correct Run
// reports the kill it made. The git's exit mark (testexec.MarkNow) cannot order that window,
// however far before the deadline it is, since the reap and Wait's read come after the exit. The
// child in the group orders it (groupChild): a run in which no SIGTERM reached it by the run's
// return is judged as main judged it. Its deadline had passed by then: the runner sets it before
// it starts the git, and the run returned more than the Timeout after the git's start
// (testexec.MarkStart). A stop whose trap found the pipe closed came after the run, and one whose
// trap ran before the Timeout could pass was no stop of the Timeout's: either fails the row
// (judgeable). A stop that found the pipe open once the Timeout could have passed was the
// Timeout's own, inside Wait. That run, a child the grace's SIGKILL ended before its trap wrote,
// and a git stopped before its child was ready are not judged, and the row runs the git again
// until a run is judged, within a minute.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildInItsGroupHoldsItsOutputPastTheDeadline(t *testing.T) {
	const timeout = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		fakeGit, mark, exited := filepath.Join(dir, "git"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		start, child := startGroupChild(t)
		script := "#!/bin/sh\n" + testexec.MarkStart(shellprefix.Literal(mark)) + "\necho 'fatal: unable to access' >&2\n" + start + "\n" + testexec.MarkNow(shellprefix.Literal(exited)) + "\nexit 128\n"
		if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		run := NewRunner(timeout, map[string]string{"git": fakeGit})

		called := testexec.SinceBoot(t)
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		returned := testexec.SinceBoot(t)

		seen := child.stopBy(t, returned, called+timeout-2*testexec.ClockTick, until)
		child.end(t)
		answer := fmt.Sprintf("Run = exit %d, TimedOut %t, error %v", result.ExitCode, result.TimedOut, err)
		if !judgeable(t, runs, seen, called, returned, timeout, within, until, "Run", answer) {
			continue
		}
		if err != nil {
			t.Fatalf("run %d: Run = %v, want the command's own exit", runs, err)
		}
		began, exit := testexec.StartedAfter(t, mark, called), exitMark(t, exited)-called
		if returned-called <= began+timeout+2*testexec.ClockTick {
			t.Fatalf("run %d: Run returned %s after the call and %s after the git's start, before its %s Timeout, which the runner sets before that start, could have passed: nothing held the pipe past the deadline, so this run proves nothing (%s)",
				runs, returned-called, returned-called-began, timeout, answer)
		}
		if result.TimedOut || result.ExitCode != 128 {
			t.Errorf("run %d: %s; want exit 128, not timed out: git exited on its own %s after the call, its %s Timeout passed %s to %s after the call while the child the git left in its group held its output pipe, and no stop reached that child by the run's return, %s after the call",
				runs, answer, exit, timeout, timeout, began+timeout, returned-called)
		}
		return
	}
}

// A command that exits on its own inside its Timeout did not time out, however long
// procgroup.WaitDelay then holds Run open for a child still holding its output pipe. The Timeout
// is 100 ms short of WaitDelay, so a run whose git exits inside it returns past it: a Run that
// read TimedOut from the deadline having passed, or from the time the run took, would report this
// git's own exit 128 as a timeout, and a clone that failed transiently inside the last WaitDelay
// of its budget, git-remote-https still holding the pipe, would read as timed out, which
// transientCloneFailure never retries.
//
// The child leaves the git's process group (setsid) and writes its pid from inside its new session
// before the git exits (heldChild, escaped), so the Timeout's stop, where os/exec still runs it
// between Go's reap of the git and Wait's read of its context, finds no group left to signal.
//
// Only a run whose git was seen reaped before its Timeout could pass shows this: a run reaped
// past it meets the Timeout's stop while Go has not reaped the git, and reports the timeout, as it
// should. Main's form judged every run, and two things bring the reap past the Timeout: the git's
// own run, which a loaded machine slows, and Run's own time, from the call to the git's start and
// from the git's exit to its reap (watchReaped). The row tells them apart by facts of the run:
// the git records when the kernel made its process (testexec.MarkStart) and the moment just before
// it exits (testexec.MarkNow), and between the two is the git's own run, which nothing Run does
// moves (ownBeforeReap). A run reaped inside the Timeout is judged as main judged it. A run reaped
// past it holds Run's own time to main's Timeout: a Run that spent that long starting the git or
// reaping it fails, as main's form failed it. Only where Run's own time stays inside the Timeout
// did the git's own run carry the reap past it: that run is not judged, and the row runs the git
// again until a run is judged, within a minute.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	const timeout = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader, fakeGit, mark, exited := filepath.Join(dir, "leader"), filepath.Join(dir, "git"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		script := "#!/bin/sh\n" + testexec.MarkStart(shellprefix.Literal(mark)) + "\necho $$ >" + shellprefix.Literal(leader) + "\necho 'fatal: unable to access' >&2\n" + heldChild(t, true) + "\n" + testexec.MarkNow(shellprefix.Literal(exited)) + "\nexit 128\n"
		if err := testexec.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		reaped := watchReaped(leader, within)
		run := NewRunner(timeout, map[string]string{"git": fakeGit})

		calledBoot := testexec.SinceBoot(t)
		started := time.Now()
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		elapsed := time.Since(started)
		at := <-reaped

		if at.IsZero() {
			t.Fatalf("run %d: the git was not seen reaped within %s of the run's start (Run = exit %d, TimedOut %t, error %v after %s)",
				runs, within, result.ExitCode, result.TimedOut, err, elapsed)
		}
		if !at.Before(started.Add(timeout)) {
			own, began, ran := ownBeforeReap(t, calledBoot, started, at, mark, exited)
			if own >= timeout {
				t.Fatalf("run %d: Run spent %s of its own before the git's reap, at or past main's %s Timeout: %s from the call to the git's start, and %s from the git's exit to its reap, which the row saw %s after the call (Run = exit %d, TimedOut %t, error %v after %s)",
					runs, own, timeout, began, own-began, at.Sub(started), result.ExitCode, result.TimedOut, err, elapsed)
			}
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the git was seen reaped %s after the call, past its %s Timeout, with Run's own time %s inside it: the git's own run %s", runs, at.Sub(started), timeout, own, ran)
				continue
			}
			t.Fatalf("no run judged in %d runs over %s: in each the git was seen reaped past its %s Timeout with Run's own time inside it; in the last, Run's own time was %s, and the git's own run %s", runs, within, timeout, own, ran)
		}
		if err != nil {
			t.Fatalf("run %d: Run = %v, want the command's own exit", runs, err)
		}
		if elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d: Run returned after %s, inside WaitDelay: nothing held the pipe past the git's exit, so this run proves nothing", runs, elapsed)
		}
		if result.TimedOut || result.ExitCode != 128 {
			t.Errorf("run %d: Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own inside its %s Timeout", runs, result.ExitCode, result.TimedOut, elapsed, timeout)
		}
		return
	}
}

// A command its caller's context ended was not cut off by its own Timeout: TimedOut is the
// Timeout's kill alone, and the run's error is the signal the stop sent (procgroup.Err), since the
// command has no exit of its own. A Run that read a timeout from the kill alone would call a clone
// its caller stopped a timeout, which transientCloneFailure never retries.
func TestRunDoesNotReportACommandItsCallerStoppedAsTimedOut(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\n: >" + started + "\nexec sleep 30\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	run := NewRunner(time.Minute, map[string]string{"git": fakeGit})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelOnceExists(ctx, cancel, started)

	result, err := run.Run(ctx, Command{Dir: dir, Argv: []string{"git"}, Timeout: time.Minute})

	if err == nil || err.Error() != "signal: terminated" {
		t.Fatalf("Run = %v, want the signal that ended the killed command (signal: terminated): it has no exit of its own", err)
	}
	if result.TimedOut {
		t.Errorf("Run = exit %d, TimedOut true; want not timed out: the caller's context ended the command inside its minute", result.ExitCode)
	}
}

// A RunCommand command that exits on its own did not time out while a child it left in its
// process group holds its output pipes past procgroup.WaitDelay: its exit is its answer. With exit
// 3 the run must say jj's own exit and no timeout; with exit 0, a run that took exec.ErrWaitDelay
// for a failure would report that success as an error. (Ported with RunCommand's own semantics
// from the shim's runAdoptionCommand, which the adoption now reaches through RunCommand.)
//
// The budget is the rest of the test binary's deadline, so a loaded machine that slows the command
// does not cut it off. Main's form gave the exit 0 a minute from before the command started, and
// that bound stays (commandBound), on RunCommand's own time and no stand-in's, for both exits: the
// command records when the kernel made its process (testexec.MarkStart) and the moment just before
// it exits (testexec.MarkNow), and what is left of the call is RunCommand's, from the call to that
// start and from that exit to the return, less the procgroup.WaitDelay os/exec holds the child's
// pipes after the command's reap, which main's budget, ending at that reap, never counted.
func TestRunCommandReportsTheCommandsOwnExitWhenAChildInItsGroupHoldsItsPipes(t *testing.T) {
	const commandBound = time.Minute
	for name, testCase := range map[string]struct {
		exit    int
		wantErr string
	}{
		"an exit 3": {3, "Command failed (exit 3)"},
		"an exit 0": {0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			budget := testwait.Remaining(t)
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			dir := t.TempDir()
			mark, exited := filepath.Join(dir, "started"), filepath.Join(dir, "exited")
			script := testexec.MarkStart(`"$1"`) + "\necho answered; echo refused >&2\n" + heldChild(t, false) + "\n" + testexec.MarkNow(`"$2"`) + "\nexit " + strconv.Itoa(testCase.exit)

			called := testexec.SinceBoot(t)
			started := time.Now()
			out, err := RunCommand(ctx, procgroup.NoReaper, budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", mark, exited})
			returned := testexec.SinceBoot(t)
			elapsed := time.Since(started)

			if elapsed < procgroup.WaitDelay {
				t.Fatalf("the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			began, after, held := testexec.StartedAfter(t, mark, called), returned-exitMark(t, exited), procgroup.WaitDelayNow()
			if own := began + after - held; own > commandBound {
				t.Errorf("RunCommand spent %s of its own, past main's %s: %s from the call to the command's start, and %s from the command's exit to the return, less the %s os/exec holds the child's pipes (procgroup.WaitDelay) (RunCommand = %q, %v)",
					own, commandBound, began, after, held, out, err)
			}
			if testCase.wantErr == "" {
				if err != nil || out != "answered" {
					t.Errorf("RunCommand = %q, %v after %s; want \"answered\" and no error: the command exited 0 on its own", out, err, elapsed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) || strings.Contains(err.Error(), "timed out") {
				t.Errorf("RunCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own, its budget far away",
					out, err, elapsed, testCase.wantErr)
			}
		})
	}
}

// A RunCommand command that exits on its own inside its budget did not time out, however long
// procgroup.WaitDelay then holds the run open for a child still holding its output pipes. The
// budget is 100 ms short of procgroup.WaitDelay, so a run whose command exits inside it returns
// past it: a run that read a timeout from the deadline having passed, or from the time the run
// took, would report jj's own exit 3 as a timeout. The child leaves the command's process group
// before the command exits. A run whose command was seen reaped past its budget is judged as in
// TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline:
// RunCommand's own time, from the call to the command's start and from the command's exit to its
// reap, is held to main's budget, and only where it stays inside it did the command's own run
// carry the reap past the budget, so that run alone is not judged.
func TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	const budget = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader, mark, exited := filepath.Join(dir, "leader"), filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		reaped := watchReaped(leader, within)
		script := testexec.MarkStart(`"$2"`) + "\n" + `echo $$ >"$1"; echo answered; echo refused >&2` + "\n" + heldChild(t, true) + "\n" + testexec.MarkNow(`"$3"`) + "\nexit 3"

		calledBoot := testexec.SinceBoot(t)
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		out, err := RunCommand(ctx, procgroup.NoReaper, budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", leader, mark, exited})
		elapsed := time.Since(started)
		cancel()
		at := <-reaped

		if at.IsZero() {
			t.Fatalf("run %d: the command was not seen reaped within %s of the run's start (RunCommand = %q, %v after %s)", runs, within, out, err, elapsed)
		}
		if !at.Before(started.Add(budget)) {
			own, began, ran := ownBeforeReap(t, calledBoot, started, at, mark, exited)
			if own >= budget {
				t.Fatalf("run %d: RunCommand spent %s of its own before the command's reap, at or past main's %s budget: %s from the call to the command's start, and %s from the command's exit to its reap, which the row saw %s after the call (RunCommand = %q, %v after %s)",
					runs, own, budget, began, own-began, at.Sub(started), out, err, elapsed)
			}
			if time.Now().Before(until) {
				t.Logf("run %d is not judged: the command was seen reaped %s after the call, past its %s budget, with RunCommand's own time %s inside it: the command's own run %s", runs, at.Sub(started), budget, own, ran)
				continue
			}
			t.Fatalf("no run judged in %d runs over %s: in each the command was seen reaped past its %s budget with RunCommand's own time inside it; in the last, RunCommand's own time was %s, and the command's own run %s", runs, within, budget, own, ran)
		}
		if elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d: the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe, so this run proves nothing", runs, elapsed)
		}
		if err == nil || !strings.Contains(err.Error(), "Command failed (exit 3)") || strings.Contains(err.Error(), "timed out") {
			t.Errorf("run %d: RunCommand = %q, %v after %s; want an error naming \"Command failed (exit 3)\" and no timeout: the command exited on its own inside its %s budget",
				runs, out, err, elapsed, budget)
		}
		return
	}
}

// A RunCommand command that exits on its own inside its budget did not time out, however long
// procgroup.WaitDelay then holds the run open for a child it left in its process group still
// holding its output pipes: jj's own exit 3 is its answer. This is main's form: the child stays in
// the command's group, and the budget is main's, 100 ms short of procgroup.WaitDelay. A run that
// read a timeout from the deadline having passed, or that, once its budget had passed, read a
// group that still answers after the wait, or a stop that found a member of it, as the budget's
// kill, would report jj's own exit 3 as a timeout. The row whose child leaves the group
// (TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt) cannot see the last two.
// A run is judged, or not, by what the child in the group saw of a stop by the run's return, as in
// TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildInItsGroupHoldsItsOutputPastTheDeadline;
// the row makes the budget's deadline itself, before RunCommand starts the command.
func TestRunCommandReportsTheCommandsOwnExitWhenAChildInItsGroupHoldsItsPipesPastIt(t *testing.T) {
	const budget = procgroup.WaitDelay - 100*time.Millisecond
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		mark, exited := filepath.Join(dir, "started"), filepath.Join(dir, "exited")
		start, child := startGroupChild(t)
		script := testexec.MarkStart(`"$1"`) + "\necho answered; echo refused >&2\n" + start + "\n" + testexec.MarkNow(`"$2"`) + "\nexit 3"

		called := testexec.SinceBoot(t)
		started := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		out, err := RunCommand(ctx, procgroup.NoReaper, budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", mark, exited})
		returned := testexec.SinceBoot(t)
		cancel()

		seen := child.stopBy(t, returned, called+budget-2*testexec.ClockTick, until)
		child.end(t)
		answer := fmt.Sprintf("RunCommand = %q, %v", out, err)
		if !judgeable(t, runs, seen, called, returned, budget, within, until, "RunCommand", answer) {
			continue
		}
		began, exit := testexec.StartedAfter(t, mark, called), exitMark(t, exited)-called
		if returned-called <= began+budget+2*testexec.ClockTick {
			t.Fatalf("run %d: RunCommand returned %s after the call and %s after the command's start, before its %s budget, which the row set before that start, could have passed: nothing held the pipes past the deadline, so this run proves nothing (%s)",
				runs, returned-called, returned-called-began, budget, answer)
		}
		if err == nil || !strings.Contains(err.Error(), "Command failed (exit 3)") || strings.Contains(err.Error(), "timed out") {
			t.Errorf("run %d: %s; want an error naming \"Command failed (exit 3)\" and no timeout: the command exited on its own %s after the call, its %s budget passed %s to %s after the call while the child the command left in its group held its output pipes, and no stop reached that child by the run's return, %s after the call",
				runs, answer, exit, budget, budget, began+budget, returned-called)
		}
		return
	}
}

// The commands of one save, one restore, or one adoption share a single budget, so a command can
// find it already spent before it starts: that is the budget running out, reported as a timeout,
// never a failure to run.
func TestRunCommandReportsABudgetSpentBeforeItStartedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := RunCommand(ctx, procgroup.NoReaper, time.Second, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, t.TempDir(), nil, []string{"sh", "-c", "exit 0"})

	if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
		t.Errorf("RunCommand under a spent budget = %v, want \"Command timed out after 1 s …\"", err)
	}
}

// A command RunCommand's caller stopped was not cut off by the budget: the restore runs under the
// launcher's own context (internal/launcher), so a launcher stopped mid-restore must read the
// signal its stop sent the command (procgroup.Err), never a timeout naming a budget that had not
// run out. The cancel lands once the command runs, never on one not yet started.
func TestRunCommandDoesNotReportACommandItsCallerStoppedAsATimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelOnceExists(ctx, cancel, marker)

	_, err := RunCommand(ctx, procgroup.NoReaper, time.Minute, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", `: >"$1"; exec sleep 30`, "sh", marker})

	if err == nil || strings.Contains(err.Error(), "timed out") || !strings.HasSuffix(err.Error(), ": signal: terminated") {
		t.Errorf("RunCommand = %v, want \"run …: signal: terminated\": the caller's context ended the command inside its minute", err)
	}
}

// A git or jj a signal ended after it said why it was failing (the memory guard's kill, another
// drain's SIGTERM) keeps that report: RunCommand's error names the signal and carries the stderr
// and the stale-lock note, as a failed exit's does, and so does a checked run's (runFailure). The
// runner's own error does not carry it: a transfer scrubs its token from the Result's stderr
// before checkedResult reads it (TestFetchScrubsTheTokenFromATransferASignalEnded). Without the
// report, the save's and the restore's notices would say only `signal: killed`.
func TestACommandASignalEndedKeepsItsReport(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "index.lock")
	if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	said := "fatal: Unable to create '" + lock + "': File exists."
	script := "echo \"fatal: Unable to create '$1': File exists.\" >&2; kill -KILL $$"
	t.Run("RunCommand", func(t *testing.T) {
		_, err := RunCommand(context.Background(), procgroup.NoReaper, time.Minute, time.Now(), []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", lock})
		if err == nil || !strings.Contains(err.Error(), ": signal: killed\n"+said+"\ngit lock "+lock+" was last written ") {
			t.Errorf("RunCommand = %v, want the signal, then what the command said, then the stale-lock note", err)
		}
	})
	t.Run("RunChecked", func(t *testing.T) {
		fakeGit := filepath.Join(t.TempDir(), "git")
		if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n"+strings.ReplaceAll(script, "$1", lock)+"\n"), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		run := NewRunner(time.Minute, map[string]string{"git": fakeGit})
		if _, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: time.Minute}); err == nil || err.Error() != "signal: killed" {
			t.Errorf("Run = %v, want the signal alone: the runner's error carries no stderr a caller has not scrubbed", err)
		}
		if _, err := RunChecked(context.Background(), run, []string{"git"}, nil, dir); err == nil || err.Error() != "run git: signal: killed\n"+said {
			t.Errorf("RunChecked = %v, want the signal and what the command said beneath it", err)
		}
	})
}

// timesOut answers a command argv starts with as one its budget ended (TimedOut, and the -1 exit a
// kill leaves), a GitHub ls-remote with a HEAD naming refs/heads/main, and every other command
// with an empty success.
type timesOut struct{ argv []string }

func (r timesOut) Timeout() time.Duration { return time.Minute }

func (r timesOut) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case commandWith(command.Argv, r.argv...):
		return Result{ExitCode: -1, TimedOut: true}, nil
	case slices.Contains(command.Argv, "ls-remote"):
		return Result{Stdout: "ref: refs/heads/main\tHEAD\n"}, nil
	}
	return Result{}, nil
}

// A read its budget ended answered nothing, so its -1 exit is no answer: the shared clone's
// rev-parse of the default branch was read as "the feed lacks the branch", which left the branch
// untracked, the credential read as a stale key, which rewrote the clone's helpers, and the feed's
// read of its HEAD as a HEAD that names nothing, which rewrote HEAD. Each is now refused as the
// timeout it is.
func TestAReadItsBudgetEndedIsRefusedNotReadAsAnAnswer(t *testing.T) {
	t.Run("the feed clone's rev-parse of the default branch", func(t *testing.T) {
		dir := t.TempDir()
		steps := cloneSteps{clock: clock.Real(), deadline: time.Now().Add(time.Hour), dir: dir}
		err := feedClone(context.Background(), timesOut{argv: []string{"git", "--git-dir=" + dir + "/clone/.git", "rev-parse"}}, steps, dir+"/clone", "https://github.com/acme/widgets", "main", remote{feed: dir + "/feed"})
		if err == nil || !strings.HasPrefix(err.Error(), "command timed out: git --git-dir="+dir+"/clone/.git rev-parse --verify --quiet refs/remotes/origin/main") {
			t.Errorf("feedClone with its rev-parse timed out = %v, want the timeout refused", err)
		}
	})
	t.Run("the shared clone's credential read", func(t *testing.T) {
		dir := t.TempDir()
		rewrote, err := ConfigureRepositoryCredential(context.Background(), timesOut{argv: []string{"git", "--git-dir=" + dir + "/.git", "config", "--get-all"}}, dir)
		if err == nil || rewrote || !strings.HasPrefix(err.Error(), "read the shared clone's credential.helper: command timed out: ") {
			t.Errorf("ConfigureRepositoryCredential with its read timed out = %t, %v; want no rewrite and the timeout refused", rewrote, err)
		}
	})
	t.Run("the feed's read of its HEAD", func(t *testing.T) {
		gitDir := t.TempDir()
		credential, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
		if err != nil {
			t.Fatal(err)
		}
		held := &fetchCredential{remote: credential, env: credential.env, read: staticToken(testInstallationToken), held: []string{testInstallationToken}}
		run := timesOut{argv: []string{"git", "--git-dir=" + gitDir, "symbolic-ref", "--quiet"}}
		err = followHead(context.Background(), run, fetchRequest(t), gitDir, map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, held, time.Now().Add(time.Minute))
		if err == nil || !strings.HasPrefix(err.Error(), "command timed out: "+strings.Join(feedGit, " ")+" --git-dir="+gitDir+" symbolic-ref --quiet HEAD") {
			t.Errorf("followHead with its symbolic-ref timed out = %v, want the timeout refused", err)
		}
	})
}

// killsOne answers a command argv starts with as one a signal ended after it printed said, a
// GitHub ls-remote with a HEAD naming refs/heads/main, and every other command with an empty
// success.
type killsOne struct {
	argv []string
	said string
	end  error
}

func (r killsOne) Timeout() time.Duration { return time.Minute }

func (r killsOne) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case commandWith(command.Argv, r.argv...):
		return Result{ExitCode: -1, Stderr: r.said}, r.end
	case slices.Contains(command.Argv, "ls-remote"):
		return Result{Stdout: "ref: refs/heads/main\tHEAD\n"}, nil
	}
	return Result{}, nil
}

// A read a signal ended is refused naming its command, the signal and what it printed, as
// checkedResult's runs are (runFailure), where these three wrapped the bare signal: the feed
// clone's rev-parse of the default branch, the shared clone's credential read, and the feed's read
// of its HEAD before it follows GitHub's (followHead), which was `signal: killed` alone.
func TestAReadASignalEndedNamesItsCommandAndWhatItPrinted(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	const said = "fatal: could not read .git/packed-refs: Input/output error"
	t.Run("the feed clone's rev-parse of the default branch", func(t *testing.T) {
		dir := t.TempDir()
		steps := cloneSteps{clock: clock.Real(), deadline: time.Now().Add(time.Hour), dir: dir}
		err := feedClone(context.Background(), killsOne{argv: []string{"git", "--git-dir=" + dir + "/clone/.git", "rev-parse"}, said: said, end: killed}, steps, dir+"/clone", "https://github.com/acme/widgets", "main", remote{feed: dir + "/feed"})
		if want := "run git --git-dir=" + dir + "/clone/.git rev-parse --verify --quiet refs/remotes/origin/main: signal: killed\n" + said; err == nil || err.Error() != want {
			t.Errorf("feedClone = %v, want %q", err, want)
		}
	})
	t.Run("the shared clone's credential read", func(t *testing.T) {
		dir := t.TempDir()
		_, err := ConfigureRepositoryCredential(context.Background(), killsOne{argv: []string{"git", "--git-dir=" + dir + "/.git", "config", "--get-all"}, said: said, end: killed}, dir)
		if want := "read the shared clone's credential.helper: run git --git-dir=" + dir + "/.git config --get-all credential.helper: signal: killed\n" + said; err == nil || err.Error() != want {
			t.Errorf("ConfigureRepositoryCredential = %v, want %q", err, want)
		}
	})
	t.Run("the feed's read of its HEAD", func(t *testing.T) {
		gitDir := t.TempDir()
		credential, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
		if err != nil {
			t.Fatal(err)
		}
		held := &fetchCredential{remote: credential, env: credential.env, read: staticToken(testInstallationToken), held: []string{testInstallationToken}}
		run := killsOne{argv: []string{"git", "--git-dir=" + gitDir, "symbolic-ref"}, said: said, end: killed}
		err = followHead(context.Background(), run, fetchRequest(t), gitDir, map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, held, time.Now().Add(time.Minute))
		if want := "run " + strings.Join(feedGit, " ") + " --git-dir=" + gitDir + " symbolic-ref --quiet HEAD: signal: killed\n" + said; err == nil || err.Error() != want {
			t.Errorf("followHead = %v, want %q", err, want)
		}
	})
}

// cancelOnceExists calls cancel once path exists, which the command under test creates once it
// runs, so the cancel lands on a running command and never on one not yet started.
func cancelOnceExists(ctx context.Context, cancel context.CancelFunc, path string) {
	for ctx.Err() == nil {
		if _, err := os.Stat(path); err == nil {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a child standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	if pid := pidIn(pidFile); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// heldChild is lines of a fake command's shell script that start a child holding the command's
// output pipes, as git-remote-https can hold git's past git's exit, and go on once the child has
// written its pid. The child runs until the test's cleanup writes its release, and kills it, or
// until this test process is gone: never for a span of its own, so no run ends, and no assertion
// holds, by the child's own exit. escaped starts the child under setsid, out of the command's
// process group, so the group is gone once the command is; the child writes its pid from inside
// its new session, so the command goes on only once the child has left the group. The child's
// paths reach it as arguments, quoted once (shellprefix.Literal).
func heldChild(t *testing.T, escaped bool) string {
	t.Helper()
	dir := t.TempDir()
	pidFile, release, holder := filepath.Join(dir, "pid"), filepath.Join(dir, "release"), filepath.Join(dir, "hold")
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			t.Errorf("release the held child: %v", err)
		}
		killPIDFile(t, pidFile)
	})
	script := fmt.Sprintf("echo $$ >\"$1.tmp\" && mv \"$1.tmp\" \"$1\"\nwhile [ ! -e \"$2\" ] && kill -0 %d 2>/dev/null; do sleep 0.05; done\n", os.Getpid())
	if err := os.WriteFile(holder, []byte(script), 0o600); err != nil {
		t.Fatalf("write the held child's script: %v", err)
	}
	start := "sh " + shellprefix.Literal(holder) + " " + shellprefix.Literal(pidFile) + " " + shellprefix.Literal(release) + " &"
	if escaped {
		start = "setsid " + start
	}
	return start + "\nwhile [ ! -s " + shellprefix.Literal(pidFile) + " ]; do sleep 0.01; done"
}

// groupChild is a child a fake command leaves in its own process group holding its output pipes,
// as git-remote-https can hold git's past git's exit, started by the lines of shell that
// startGroupChild returns, and the files in which it records what a stop sent to that group found.
//
// It ignores SIGPIPE, and its SIGTERM trap writes one byte to its standard output, the command's
// output pipe, records when the trap ran (termed.at, testexec.MarkNow) and what the write found
// (termed: open where it went through, closed where it answered EPIPE), and exits. os/exec closes
// that pipe's read end once WaitDelay has run out after the command's reap, before the run returns
// (awaitGoroutines). procgroup.Configure's Cancel, the budget's own stop, runs only before that
// wait has begun, and waits for the group to leave on its SIGTERM, or ends it by the grace's
// SIGKILL, while the read end is still open. So open is a stop inside Wait, and closed is one sent
// after the run.
//
// The child writes its pid once its traps are set, and the command goes on only then, so a command
// that reached its exit left a child ready to record a stop. At each turn of its loop the child
// writes a beat, the time since boot (testexec.MarkNow). dash runs a pending trap at the end of
// each command, and the trap exits, so a signal sent before the child reads the time is taken on
// that read's return: a beat read past the run's return, and one more after it, mean no SIGTERM had
// reached the child by then. /proc/<pid>/status cannot say it: gVisor's has no SigPnd or ShdPnd.
// The child runs until the row ends it once the run is read (end), or until this test process is
// gone, never on a timer of its own, so its own end lets no run's pipe go early.
type groupChild struct {
	pidFile, termed, beat, release string
	ended                          bool
}

// startGroupChild is lines of a fake command's shell script that start a groupChild, and go on
// once it has set its traps and written its pid, and the child they start. The child's paths reach
// it as arguments, quoted once (shellprefix.Literal). The test's cleanup ends a child its row did
// not end.
func startGroupChild(t *testing.T) (string, *groupChild) {
	t.Helper()
	dir := t.TempDir()
	child := &groupChild{pidFile: filepath.Join(dir, "pid"), termed: filepath.Join(dir, "termed"), beat: filepath.Join(dir, "beat"), release: filepath.Join(dir, "release")}
	t.Cleanup(func() { child.end(t) })
	script := `pidfile=$1 termed=$2 beat=$3 release=$4 owner=$5
onterm() { if printf x; then w=open; else w=closed; fi; ` + testexec.MarkNow(`"$termed.at"`) + `; echo "$w" >"$termed"; exit 0; }
trap '' PIPE
trap onterm TERM
echo $$ >"$pidfile"
while [ ! -e "$release" ] && kill -0 "$owner" 2>/dev/null; do ` + testexec.MarkNow(`"$beat"`) + `; sleep 0.01; done`
	words := []string{"sh", "-c", shellprefix.Literal(script), "child"}
	for _, path := range []string{child.pidFile, child.termed, child.beat, child.release} {
		words = append(words, shellprefix.Literal(path))
	}
	words = append(words, strconv.Itoa(os.Getpid()))
	return strings.Join(words, " ") + " &\nwhile [ ! -s " + shellprefix.Literal(child.pidFile) + " ]; do sleep 0.01; done", child
}

// end releases the child, and kills it in case the release does not reach it. It does so once.
func (c *groupChild) end(t *testing.T) {
	t.Helper()
	if c.ended {
		return
	}
	c.ended = true
	if err := os.WriteFile(c.release, nil, 0o600); err != nil {
		t.Errorf("release the child in the command's group: %v", err)
	}
	killPIDFile(t, c.pidFile)
}

// childStop is what the child a fake command left in its group saw of a stop by its run's return
// (groupChild.stopBy): no SIGTERM (none); one whose trap's write found the command's output pipe
// closed (afterRun), or whose trap ran before the run's budget could pass (early), with at the time
// since boot that trap ran; or, for a run the row cannot judge, why (unjudged).
type childStop struct {
	none, afterRun, early bool
	at                    time.Duration
	unjudged              string
}

// stopBy reads what the child of a run that returned at returned, a time since boot, saw of a stop,
// from its pid, the record its SIGTERM trap writes and its beat, as groupChild's doc says.
// budgetFrom is the earliest moment, as a time since boot, the run's budget could pass: a stop whose
// trap ran before it was no stop of the budget's. stopBy waits until the child has recorded a
// SIGTERM, has ended, or has written a beat past returned and one more after it, or until until
// passes, which it reports as a run it cannot judge.
func (c *groupChild) stopBy(t *testing.T, returned, budgetFrom time.Duration, until time.Time) childStop {
	t.Helper()
	pid := pidIn(c.pidFile)
	if pid == 0 {
		return childStop{unjudged: "the command was stopped before its child had set its traps, which the command waits for before it exits: its budget passed while the command still ran"}
	}
	var seen childStop
	recorded := func() bool {
		raw, err := os.ReadFile(c.termed)
		if err != nil || !strings.HasSuffix(string(raw), "\n") {
			return false
		}
		at, err := testexec.ReadNowMark(c.termed + ".at")
		if err != nil {
			t.Fatalf("read when the child's SIGTERM trap ran, which it records before what its write found: %v", err)
		}
		switch found := strings.TrimSpace(string(raw)); {
		case found == "closed":
			seen = childStop{afterRun: true, at: at}
		case found != "open":
			t.Fatalf("the child's SIGTERM trap recorded %q, want open or closed", found)
		case at < budgetFrom:
			seen = childStop{early: true, at: at}
		default:
			seen = childStop{unjudged: "the child the command left in its group took a SIGTERM while os/exec still read the command's output pipe (its write went through), once the budget could have passed, so the stop was the budget's own, inside Wait: the machine held the command's reap, or Wait's read of its context after the reap, past the budget"}
		}
		return true
	}
	var past time.Duration
	if !testwait.Holds(time.Until(until), testwait.Poll, func() bool {
		if recorded() {
			return true
		}
		if processEnded(t, pid) {
			// It may have recorded a SIGTERM and left between the two reads.
			if !recorded() {
				seen = childStop{unjudged: "the child the command left in its group ended before its SIGTERM trap recorded anything: the grace's SIGKILL, which only the budget's own stop sends, ended it"}
			}
			return true
		}
		at, err := testexec.ReadNowMark(c.beat)
		if err != nil || at <= returned {
			return false
		}
		if past == 0 {
			past = at
			return false
		}
		seen.none = at > past
		return seen.none
	}) {
		return childStop{unjudged: "by the end of the row's bound the child the command left in its group had neither recorded a SIGTERM, nor ended, nor run on past the run's return, so the row cannot tell whether a stop reached it"}
	}
	return seen
}

// judgeable fails the row on a stop that no budget made, which the child the command of run runs
// left in its group saw (seen): one sent after the run, or one before the budget could pass, the
// run having been called at called and returned at returned (times since boot) under budget. It
// reports whether the run is judged: true where no stop reached the child by the run's return, and
// false, logged, for a run that is not judged while until, the end of the row's bound within, has
// not passed, so the row runs again. Once until has passed, such a run fails the row. runner names
// what ran the command, and answer what it answered.
func judgeable(t *testing.T, runs int, seen childStop, called, returned, budget, within time.Duration, until time.Time, runner, answer string) bool {
	t.Helper()
	switch {
	case seen.afterRun:
		t.Fatalf("run %d: %s stopped the command's process group after its run: the child the command left in its group took a SIGTERM (its trap ran %s from the run's return), and its write to the command's output pipe answered EPIPE: os/exec had already closed the pipe, which it does once WaitDelay runs out, and the budget's own stop comes only before that (%s)",
			runs, runner, seen.at-returned, answer)
	case seen.early:
		t.Fatalf("run %d: %s stopped the command's process group before its %s budget could pass: the child the command left in its group took a SIGTERM whose trap ran %s after the call (%s)",
			runs, runner, budget, seen.at-called, answer)
	case seen.none:
		return true
	case time.Now().Before(until):
		t.Logf("run %d is not judged: %s (%s after %s)", runs, seen.unjudged, answer, returned-called)
		return false
	}
	t.Fatalf("no run of %d over %s was judged: in the last, %s (%s after %s)", runs, within, seen.unjudged, answer, returned-called)
	return false
}

// processEnded is whether the process pid is gone, or has exited and waits only to be reaped: its
// state, the third field of /proc/<pid>/stat after the command in parentheses, reads Z. A child a
// command left behind is reaped by whatever it was reparented to, not by the run, and kill(pid, 0)
// answers nil for it until then. A process kill still finds is never read as ended from a /proc
// entry that cannot be read: the row fails there, naming /proc, since it has no other way to ask.
// kill is asked again after such a read, because a process that went between the two leaves no
// entry, and has ended. It calls t.Fatalf, so it must be called from the goroutine running the
// test.
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

// cutRunMargin is what the rows bounded by a minute leave of the test binary's deadline
// (testwait.Bounded): the time a run the bound cuts takes to return, its stop's grace and
// WaitDelay, and a second for the row to fail in.
const cutRunMargin = procgroup.StopGrace + procgroup.WaitDelay + time.Second

// watchReaped watches, from before the command starts, for the command that writes its own pid to
// leader before anything of its own that can block, and answers once when it first saw that pid
// answer ESRCH, which it does once Go has reaped the command (a zombie still answers), or the zero
// time where it never did within bound. Each moment is read after the fact it stands for.
func watchReaped(leader string, bound time.Duration) <-chan time.Time {
	seen := make(chan time.Time, 1)
	go func() {
		var at time.Time
		if testwait.Holds(bound, testwait.Poll, func() bool {
			pid := pidIn(leader)
			return pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		}) {
			at = time.Now()
		}
		seen <- at
	}()
	return seen
}

// exitMark is the moment, as a time since boot, a stand-in recorded at path just before it exited
// (testexec.MarkNow). A stand-in that wrote none never reached its exit, and the row fails saying
// so.
func exitMark(t *testing.T, path string) time.Duration {
	t.Helper()
	at, err := testexec.ReadNowMark(path)
	if err != nil {
		t.Fatalf("read the moment the command exited, which it records just before its exit: %v", err)
	}
	return at
}

// ownBeforeReap is the runner's own time before the reap, at (on the wall clock), of a command it
// was called on at called (on the wall clock; calledBoot is the same moment as a time since boot),
// and the stand-in's own run, for a row's log. The runner's time is from the call to the command's
// start, the kernel's record of its process that the command wrote to mark as its first act
// (testexec.MarkStart), and from the command's exit, the moment it wrote to exited just before it
// (testexec.MarkNow), to the reap. Between the two is the command's own run, which a loaded machine
// slows and which nothing the runner does moves. A command that wrote no exit mark was ended in its
// own run by its budget's stop, and only the time to its start is the runner's.
func ownBeforeReap(t *testing.T, calledBoot time.Duration, called, at time.Time, mark, exited string) (own, began time.Duration, ran string) {
	t.Helper()
	began = testexec.StartedAfter(t, mark, calledBoot)
	own, ran = began, "never reached its exit mark: its budget's stop ended it in its own run"
	if exit, unread := testexec.ReadNowMark(exited); unread == nil {
		own += calledBoot + at.Sub(called) - exit
		ran = fmt.Sprintf("took %s from its start to its exit", exit-calledBoot-began)
	}
	return own, began, ran
}

// pidIn is the pid a command's shell wrote to path, once the whole line is there, and 0 before.
func pidIn(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(raw), "\n") {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
