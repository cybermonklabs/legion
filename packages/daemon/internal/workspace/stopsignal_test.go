package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// commandLives keeps a test's command running until a signal ends it. A shell runs a trap when the
// command it is waiting for ends, so the command waits in a loop of short sleeps rather than in
// one long one: a SIGTERM it handles is then acted on within one sleep of its arrival, well inside
// the grace. A trap of the empty action on TERM ignores SIGTERM outright, and a process started
// under that disposition inherits it, so an ignoring command's own sleeps and helpers ignore
// SIGTERM too.
const commandLives = `while :; do sleep 0.05; done`

// RunCommand's jj and git are stopped the way execRunner's are: the budget's end sends SIGTERM to
// the command's own process group, which is the group procgroup.Configure gives it and which
// RunCommand never had before dispatch://INFRA-3098, waits the grace, and sends SIGKILL only
// after it. The three cases are the three things that stop is: a command that handles SIGTERM
// removes the lock it holds and the run ends well inside the grace; a helper the command forked is
// stopped with it, which only a group can do; and a command that handles SIGTERM and exits 0 at
// the end of its budget is still reported as a timeout rather than as a command that could not be
// run, which is execRunner.Run reading its kill before it propagates the run's own error.
func TestRunCommandStopsItsCommandTheSameWay(t *testing.T) {
	environment := []string{"PATH=" + os.Getenv("PATH")}

	t.Run("a command that handles SIGTERM removes its lock", func(t *testing.T) {
		dir := t.TempDir()
		lock, ready := filepath.Join(dir, "index.lock"), filepath.Join(dir, "ready")
		budget := time.Second
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		// The lock is taken before the command reports that it is running, so a lock that is gone
		// at the end is one the command's own SIGTERM handler removed.
		script := `: >"$1"; trap 'rm -f "$1"; exit 0' TERM; : >"$2"; ` + commandLives

		started := time.Now()
		_, err := RunCommand(ctx, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", lock, ready})
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): the budget ended before it ran, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		if _, statErr := os.Stat(lock); !os.IsNotExist(statErr) {
			t.Errorf("the lock the command held is still there (%v) after %s: the budget's end gave it no SIGTERM to remove it on (RunCommand = %v)", statErr, elapsed, err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "Command timed out after 1 s") {
			t.Errorf("RunCommand = %v after %s, want \"Command timed out after 1 s …\": the budget ended the command, however cleanly it then left", err, elapsed)
		}
		if elapsed > budget+procgroup.StopGrace/2 {
			t.Errorf("the run took %s, which is past its %s budget plus half the %s grace: a command that leaves on its SIGTERM must end the grace, not sit out its whole span", elapsed, budget, procgroup.StopGrace)
		}
	})

	t.Run("a helper the command forked is stopped with it", func(t *testing.T) {
		dir := t.TempDir()
		ready, marker := filepath.Join(dir, "ready"), filepath.Join(dir, "deaf-helper-of-this-row")
		budget := time.Second
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		// Both ignore SIGTERM, so the group's SIGKILL after the grace is what ends them, and the
		// helper carries a path of this row's own temporary directory, which is what names it in
		// /proc. Without a group of its own the command's own kill reaches the leader alone and
		// this helper — a git-remote-https, in a real fetch — keeps running with the pipes open.
		// The helper is forked before the command ignores SIGTERM: the ignore is inherited across
		// fork and exec, and a shell cannot trap a signal that was already ignored when it
		// started, so a helper forked after that line would ignore SIGTERM whatever its own script
		// says, and this row would prove nothing about the helper's own disposition.
		helper := `trap '' TERM; ` + commandLives
		script := `sh -c "$2" sh "$3" & trap '' TERM; : >"$1"; ` + commandLives

		started := time.Now()
		_, err := RunCommand(ctx, budget, started, environment, dir, nil, []string{"sh", "-c", script, "sh", ready, helper, marker})
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the command never reported that it was running (%v): the budget ended before it ran, so this run proves nothing (RunCommand = %v)", statErr, err)
		}
		if !testwait.Holds(5*time.Second, testwait.Poll, func() bool { return len(gitProcessesUnder(t, marker)) == 0 }) {
			t.Errorf("the helper the command forked was still running after the run ended in %s: %v (RunCommand = %v)", elapsed, gitProcessesUnder(t, marker), err)
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
		timeout := time.Second
		run := NewRunner(timeout, map[string]string{"git": fakeGit})

		started := time.Now()
		result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		elapsed := time.Since(started)

		if _, statErr := os.Stat(ready); statErr != nil {
			t.Fatalf("the git never reported that it was running (%v): the timeout ended before it ran, so this run proves nothing (Run = %v)", statErr, err)
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
