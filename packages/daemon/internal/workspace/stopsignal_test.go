package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// inFlight runs f on a goroutine of its own and answers the channel its answer arrives on, for a
// row that waits, under a bound of its own (testwait.Receive), for a call that may never return.
func inFlight[T any](f func() T) <-chan T {
	answered := make(chan T, 1)
	go func() { answered <- f() }()
	return answered
}

// commandLives keeps a test's command running until a signal ends it. A shell runs a trap when the
// command it is waiting for ends, so the command waits in a loop of short foreground sleeps rather
// than in one long one: a SIGTERM it handles is then acted on within one sleep of its arrival,
// well inside the grace. The sleeps are foreground, and never a loop of `sleep & wait $!`: a
// `wait` returns on the group's own SIGTERM and the loop then forks its next sleep, which that
// signal has already passed by, so a trap that leaves would leave that sleep behind holding the
// command's output pipes. A trap of the empty action on TERM ignores SIGTERM outright, and a
// process started under that disposition inherits it, so an ignoring command's own sleeps and
// helpers ignore SIGTERM too.
//
// It gives up after a minute, which no row here reaches: each row's budget passes once its command
// has reported that it is running (budgetPassedOnceExists), so the stop under test is what ends
// each of these commands. It is what a run whose stop never came leaves behind — a command that
// ignores SIGTERM stays until something kills it, and a failing row has nothing left to kill it
// with. Every row reads a timeout, which a command that gave up on its own never reports, so no
// row passes by that end.
const commandLives = `ticks=0; while [ "$ticks" -lt 1200 ]; do sleep 0.05; ticks=$((ticks+1)); done`

// RunCommand's jj and git are stopped the way execRunner's are: the budget's end sends SIGTERM to
// the command's own process group, which is the group procgroup.Configure gives it and which
// RunCommand never had before dispatch://INFRA-3098, waits the grace, and sends SIGKILL only
// after it. The three cases are the three things that stop is: a command that handles SIGTERM
// removes the lock it holds and the run ends well inside the grace; a helper the command forked is
// stopped with it, which only a group can do; and a command that handles SIGTERM and exits 0 at
// the end of its budget is still reported as a timeout rather than as a command that could not be
// run, which is execRunner.Run reading its kill before it propagates the run's own error.
//
// Each row's budget passes at a fact of the run, not at a time: once the command has reported
// that it is running, its trap installed and its helper ready (budgetPassedOnceExists). A budget
// counted from before the command started could pass, on a loaded machine, before the command had
// reached that step, and the stop would then find a command with no trap yet, or a helper not yet
// ignoring SIGTERM. RunCommand's report still names the second its caller gave it as the budget.
func TestRunCommandStopsItsCommandTheSameWay(t *testing.T) {
	environment := []string{"PATH=" + os.Getenv("PATH")}

	t.Run("a command that handles SIGTERM removes its lock", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready")
		budget := time.Second
		ctx := budgetPassedOnceExists(t, ready)
		// The lock is taken before the command reports that it is running, so a lock that is gone
		// at the end is one the command's own SIGTERM handler removed.
		script := `: >"$1"; trap 'rm -f "$1"; exit 0' TERM; : >"$2"; ` + commandLives

		started := time.Now()
		_, err := RunCommand(ctx, procgroup.NoReaper, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", lock, ready})
		returned := time.Now()
		elapsed := returned.Sub(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): its budget never passed, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		passed, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("the command reported that it was running, but its budget had not passed when the run returned after %s (RunCommand = %v)", elapsed, err)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the command held is still there (%v) after %s: the budget's end gave it no SIGTERM to remove it on (RunCommand = %v)", statErr, elapsed, err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v after %s, want \"Command timed out after 1 s …\": the budget ended the command, however cleanly it then left", err, elapsed)
		}
		if stopped := returned.Sub(passed); stopped > procgroup.StopGrace/2 {
			t.Errorf("the run returned %s after its budget passed, past half the %s grace: a command that leaves on its SIGTERM must end the grace, not sit out its whole span", stopped, procgroup.StopGrace)
		}
	})

	t.Run("a helper the command forked is stopped with it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper-of-this-row")
		trapped := filepath.Join(dir, "helper-trapped")
		budget := time.Second
		ctx := budgetPassedOnceExists(t, ready)
		// Both ignore SIGTERM, so the group's SIGKILL after the grace is what ends them, and the
		// helper carries a path of this row's own temporary directory, which is what names it in
		// /proc. Without a group of its own the command's own kill reaches the leader alone and
		// this helper — a git-remote-https, in a real fetch — keeps running with the pipes open.
		// The helper is forked before the command ignores SIGTERM: the ignore is inherited across
		// fork and exec, and a shell cannot trap a signal that was already ignored when it
		// started, so a helper forked after that line would ignore SIGTERM whatever its own script
		// says, and this row would prove nothing about the helper's own disposition.
		//
		// The fork returns as soon as the helper exists, which is before that helper's own shell
		// has reached its trap line, so the helper writes a marker of its own once the ignore is
		// installed and the leader waits for it before it reports that it is running: the budget
		// passes once the ready file is there, so its SIGTERM finds the helper under the
		// disposition its script asks for. A stop that raced that line would end the helper with
		// the group's SIGTERM rather than leave it to the SIGKILL, and this row — which reads only
		// that the helper is gone — would pass on the opposite of what it claims.
		//
		// The helper holds the command's pipes until this row's directory is gone, which the
		// test's cleanup removes, or until this test process is gone: never for a span of its own,
		// so a helper nothing stopped is still there however long the row waits for it, and a run
		// that waits on its pipes for ever fails the wait below rather than ending with the helper.
		helper := `trap '' TERM; : >"$2"; while [ -d '` + dir + `' ] && kill -0 ` + strconv.Itoa(os.Getpid()) + ` 2>/dev/null; do sleep 0.05; done`
		script := `sh -c "$2" sh "$3" "$4" & trap '' TERM; until [ -e "$4" ]; do sleep 0.01; done; : >"$1"; ` + commandLives

		returned := make(chan error, 1)
		started := time.Now()
		go func() {
			_, err := RunCommand(ctx, procgroup.NoReaper, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", ready, helper, marker, trapped})
			returned <- err
		}()
		var err error
		select {
		case err = <-returned:
		case <-time.After(testwait.Bounded(t, time.Minute, cutRunMargin)):
			t.Fatalf("RunCommand had not returned %s after it started: nothing stopped the helper the command forked, which still holds the command's pipes (%v)", time.Since(started), gitProcessesUnder(t, marker))
		}
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): its budget never passed, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v after %s, want \"Command timed out after 1 s …\": the budget's stop ended a command that ignores SIGTERM", err, elapsed)
		}
		// The stop's SIGKILL reaches the helper with the leader, before the run ends, so five
		// seconds after the run's end is the signal's delivery and nothing else: a helper still
		// running then was signalled late, or not at all.
		helperGone := func() bool { return len(gitProcessesUnder(t, marker)) == 0 }
		if !testwait.Holds(5*time.Second, testwait.Poll, helperGone) {
			t.Errorf("the helper the command forked was still running 5s after the run ended in %s: %v (RunCommand = %v)", elapsed, gitProcessesUnder(t, marker), err)
		}
	})

	t.Run("execRunner reports a command that handled SIGTERM and exited 0 as timed out", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready")
		fakeGit := filepath.Join(dir, "git")
		script := "#!/bin/sh\n: >\"" + lock + "\"\ntrap 'rm -f \"" + lock + "\"; exit 0' TERM\n: >\"" + ready + "\"\n" + commandLives + "\n"
		if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		// The git's own Timeout is the rest of the test binary's deadline: the budget that ends
		// it is the caller's, whose deadline passes once the git reports that it is running, and
		// execRunner.Run bounds the git by both.
		timeout := testwait.Remaining(t)
		run := NewRunner(timeout, map[string]string{"git": fakeGit})
		ctx := budgetPassedOnceExists(t, ready)

		started := time.Now()
		result, err := run.Run(ctx, Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the git never reported that it was running (%v): its budget never passed, so this run proves nothing (Run = %v)", statErr, err)
		}
		// exec reports a command whose Cancel succeeded and which then exited cleanly as the
		// context's error, so a Run that propagated that error before reading its own kill would
		// report this git as one it could not run, and transientCloneFailure would read the next
		// such clone as a retryable failure rather than as the timeout it is.
		if err != nil {
			t.Fatalf("Run = %v after %s, want no error: the git exited 0 on the timeout's SIGTERM, which is a timeout and not a failure to run", err, elapsed)
		}
		if !result.TimedOut || result.ExitCode != 0 {
			t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 0 and timed out: the timeout stopped a git that handled its SIGTERM and left", result.ExitCode, result.TimedOut, elapsed)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the git held is still there (%v) after %s: the timeout's end gave it no SIGTERM to remove it on", statErr, elapsed)
		}
	})
}

// budgetPassedOnceExists is a caller's context whose deadline passes as a budget's does
// (testwait.StandInDeadline: its Err and its cause context.DeadlineExceeded) once path exists,
// which the command under test writes once it has reached the step its stop must find it at: a
// trap installed, a helper ready, a lock taken. A budget counted from before the command started
// could pass, on a loaded machine, before the command had reached that step. Its Deadline, once
// it has passed, is the moment it passed. A path not there within a minute ends the context as a
// cancel, so a run still going then reads no budget passed, and the test's cleanup ends it the
// same way.
func budgetPassedOnceExists(t *testing.T, path string) context.Context {
	t.Helper()
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	bound := testwait.Bounded(t, time.Minute, cutRunMargin)
	go func() {
		there := testwait.Holds(bound, testwait.Poll, func() bool {
			if ctx.Err() != nil {
				return true
			}
			_, err := os.Stat(path)
			return err == nil
		})
		if there && ctx.Err() == nil {
			pass(context.DeadlineExceeded)
			return
		}
		cancel()
	}()
	return ctx
}
