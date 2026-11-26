package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stopMoment is where the fake clock of a row that drives a command's stop stands until the row
// moves it: the budget's end and the grace after its SIGTERM are then steps of the row, so a
// loaded machine neither ends a budget before the command it bounds is running nor spends a grace
// the row means to keep.
var stopMoment = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// inFlight runs f on a goroutine of its own and answers the channel its answer arrives on, for a
// row that moves a fake clock while the run is in flight.
func inFlight[T any](f func() T) <-chan T {
	answered := make(chan T, 1)
	go func() { answered <- f() }()
	return answered
}

// exists waits until the command under test creates path, which it does once it is running.
func exists(t *testing.T, what, path string) {
	t.Helper()
	testwait.Eventually(t, what, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

// runAnswer is what execRunner.Run answered.
type runAnswer struct {
	result Result
	err    error
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
// It gives up after a minute, which no row here reaches: the stop under test is what ends each of
// these commands. It is what a run whose stop never came leaves behind — a command that ignores
// SIGTERM, and the helper it forked, stay until something kills them, and a failing row has
// nothing left to kill them with.
const commandLives = `ticks=0; while [ "$ticks" -lt 1200 ]; do sleep 0.05; ticks=$((ticks+1)); done`

// RunCommand's jj and git are stopped the way execRunner's are: the budget's end sends SIGTERM to
// the command's own process group, which is the group procgroup.Configure gives it and which
// RunCommand never had before dispatch://INFRA-3098, waits the grace, and sends SIGKILL only
// after it. The three cases are the three things that stop is: a command that handles SIGTERM
// removes the lock it holds and the run ends without the grace running out; a helper the command
// forked is stopped with it, which only a group can do; and a command that handles SIGTERM and
// exits 0 at the end of its budget is still reported as a timeout rather than as a command that
// could not be run, which is execRunner.Run reading its kill before it propagates the run's own
// error. Each budget and grace is on a fake clock (stopMoment), which moves only once the command
// reports that it is running.
func TestRunCommandStopsItsCommandTheSameWay(t *testing.T) {
	environment := []string{"PATH=" + os.Getenv("PATH")}

	t.Run("a command that handles SIGTERM removes its lock", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready")
		letGo := filepath.Join(dir, "let-go")
		budget := time.Second
		clock := clocktest.New(stopMoment)
		ctx, cancel := clock.WithTimeout(context.Background(), budget)
		defer cancel()
		// The lock is taken before the command reports that it is running, so a lock that is gone
		// at the end is one the command's own SIGTERM handler removed. The handler removes it and
		// leaves only once the row has written letGo ($3): while it waits, the group outlives its
		// SIGTERM, so the stop's grace is pending on the fake clock and the row can read it there.
		// A handler that left at once would let a grace that left the injected clock — a real
		// timer, or a real sleep of procgroup.StopGrace — still end the run, at the group's exit or
		// a real second later, and the row would pass on it. The handler's wait gives up after a
		// minute, as commandLives does and for its reason: a failing row of this file has nothing
		// left to kill its command.
		script := `: >"$1"; trap 'waited=0; while [ ! -e "$3" ] && [ "$waited" -lt 1200 ]; do sleep 0.05; waited=$((waited+1)); done; rm -f "$1"; exit 0' TERM; : >"$2"; ` + commandLives

		ran := inFlight(func() error {
			_, err := runCommandOn(ctx, clock, procgroup.NoReaper, budget, clock.Now(), environment, dir, nil, []string{"sh", "-c", script, "sh", lock, ready, letGo})
			return err
		})
		exists(t, "the command to report that it is running", ready)
		clock.Advance(budget)
		// The handler holds the group past its SIGTERM, so the grace is armed on the fake clock
		// from the moment the stop began. A grace that left the injected clock is never armed
		// there, so this wait fails and names the grace.
		if armed := clock.AwaitArmed(t, "the grace after the group's SIGTERM", 1); !armed[0].Equal(clock.Now().Add(procgroup.StopGrace)) {
			t.Fatalf("the stop armed %v, want its grace of procgroup.StopGrace %v from %v", armed, procgroup.StopGrace, clock.Now())
		}
		if err := os.WriteFile(letGo, nil, 0o600); err != nil {
			t.Fatalf("let the command's SIGTERM handler go: %v", err)
		}
		// The grace is on the same clock, which never moves again: a stop that sat out its grace
		// rather than ending it once the command left would never return.
		err := testwait.Receive(t, "the run to end once the command left on its SIGTERM, inside a grace the clock never spends", ran)

		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the command held is still there (%v): the budget's end gave it no SIGTERM to remove it on (RunCommand = %v)", statErr, err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v, want \"Command timed out after 1 s …\": the budget ended the command, however cleanly it then left", err)
		}
	})

	t.Run("a helper the command forked is stopped with it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper-of-this-row")
		trapped := filepath.Join(dir, "helper-trapped")
		budget := time.Second
		clock := clocktest.New(stopMoment)
		ctx, cancel := clock.WithTimeout(context.Background(), budget)
		defer cancel()
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
		// installed and the leader waits for it before it reports that it is running: the budget's
		// SIGTERM lands after the ready file, so it finds the helper under the disposition its
		// script asks for. A stop that raced that line would end the helper with the group's
		// SIGTERM rather than leave it to the SIGKILL, and this row — which reads only that the
		// helper is gone — would pass on the opposite of what it claims.
		helper := `trap '' TERM; : >"$2"; ` + commandLives
		script := `sh -c "$2" sh "$3" "$4" & trap '' TERM; until [ -e "$4" ]; do sleep 0.01; done; : >"$1"; ` + commandLives

		ran := inFlight(func() error {
			_, err := runCommandOn(ctx, clock, procgroup.NoReaper, budget, clock.Now(), environment, dir, nil, []string{"sh", "-c", script, "sh", ready, helper, marker, trapped})
			return err
		})
		exists(t, "the command to report that it is running", ready)
		clock.Advance(budget)
		// Both ignore the SIGTERM, so the grace runs until the row moves the clock past it.
		if armed := clock.AwaitArmed(t, "the grace after the group's SIGTERM", 1); !armed[0].Equal(clock.Now().Add(procgroup.StopGrace)) {
			t.Fatalf("the stop armed %v, want its grace of procgroup.StopGrace %v from %v", armed, procgroup.StopGrace, clock.Now())
		}
		clock.Advance(procgroup.StopGrace)
		err := testwait.Receive(t, "the run to end at the grace's SIGKILL", ran)
		testwait.Until(t, "the helper the command forked to be stopped with it once the run ended", func() (string, bool) {
			left := gitProcessesUnder(t, marker)
			return fmt.Sprintf("still running: %v (RunCommand = %v)", left, err), len(left) == 0
		})
	})

	t.Run("execRunner reports a command that handled SIGTERM and exited 0 as timed out", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready")
		fakeGit := filepath.Join(dir, "git")
		script := "#!/bin/sh\n: >\"" + lock + "\"\ntrap 'rm -f \"" + lock + "\"; exit 0' TERM\n: >\"" + ready + "\"\n" + commandLives + "\n"
		if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		timeout := time.Second
		clock := clocktest.New(stopMoment)
		run := execRunner{timeout: timeout, tools: map[string]string{"git": fakeGit}, clock: clock}

		ran := inFlight(func() runAnswer {
			result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
			return runAnswer{result, err}
		})
		exists(t, "the git to report that it is running", ready)
		if armed := clock.AwaitArmed(t, "the git's Timeout", 1); !armed[0].Equal(stopMoment.Add(timeout)) {
			t.Fatalf("the run armed %v, want the git's Timeout of %v from %v", armed, timeout, stopMoment)
		}
		clock.Advance(timeout)
		got := testwait.Receive(t, "the run to end once the git left on its SIGTERM", ran)
		// exec reports a command whose Cancel succeeded and which then exited cleanly as the
		// context's error, so a Run that propagated that error before reading its own kill would
		// report this git as one it could not run, and transientCloneFailure would read the next
		// such clone as a retryable failure rather than as the timeout it is.
		if got.err != nil {
			t.Fatalf("Run = %v, want no error: the git exited 0 on the timeout's SIGTERM, which is a timeout and not a failure to run", got.err)
		}
		if !got.result.TimedOut || got.result.ExitCode != 0 {
			t.Errorf("Run = exit %d, TimedOut %t; want exit 0 and timed out: the timeout stopped a git that handled its SIGTERM and left", got.result.ExitCode, got.result.TimedOut)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the git held is still there (%v): the timeout's end gave it no SIGTERM to remove it on", statErr)
		}
	})
}
