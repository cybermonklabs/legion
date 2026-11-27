package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// procgroup.Err is execRunner's one classification of a command's error: nil for an ordinary exit
// or for exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay), either way read
// from cmd.ProcessState.ExitCode(). A fake git that exits 0 while a backgrounded child keeps its
// stdout pipe open proves that wiring end to end: RunChecked must succeed with ExitCode 0, not
// report the command's own clean exit as a failure.
func TestRunCheckedSucceedsWhenACommandExitsZeroWithAChildStillHoldingItsOutputOpen(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho done\nsleep 3 & echo $! >" + pidFile + "\nexit 0\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })

	run := NewRunner(5*time.Second, map[string]string{"git": fakeGit})

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
// here is 100 ms short of WaitDelay, so every such run returns past its deadline, while git still
// has 900 ms to exit under load: a Run that read TimedOut from the deadline having passed would
// report this git's own exit 128 as a timeout, and a clone that failed transiently inside the
// last WaitDelay of its budget, git-remote-https still holding the pipe, would read as timed out,
// which transientCloneFailure never retries.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: unable to access' >&2\nsleep 30 & echo $! >" + pidFile + "\nexit 128\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })
	timeout := procgroup.WaitDelay - 100*time.Millisecond
	run := NewRunner(timeout, map[string]string{"git": fakeGit})

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run = %v, want the command's own exit", err)
	}
	if elapsed < timeout {
		t.Fatalf("Run returned after %s, inside its %s Timeout: nothing held the pipe past the deadline, so this run proves nothing", elapsed, timeout)
	}
	if result.TimedOut || result.ExitCode != 128 {
		t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own inside its %s Timeout", result.ExitCode, result.TimedOut, elapsed, timeout)
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

// A RunCommand command that exits on its own did not time out, however long procgroup.WaitDelay
// then holds the run open for a child still holding its output pipes: its exit is its answer. In
// the first case the budget is 100 ms short of procgroup.WaitDelay, so the run always returns past
// it, while the command still has 900 ms to exit under load; a run that read a timeout from the
// deadline having passed would report jj's own exit 3 as a timeout. In the second the command
// exits 0 well inside its budget; a run that took exec.ErrWaitDelay for a failure would report
// that success as an error. (Ported with RunCommand's own semantics from the shim's
// runAdoptionCommand, which the adoption now reaches through RunCommand.)
func TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		exit    int
		budget  time.Duration
		wantErr string
	}{
		"an exit 3, the pipes held past the budget": {3, procgroup.WaitDelay - 100*time.Millisecond, "Command failed (exit 3)"},
		"an exit 0 inside the budget":               {0, time.Minute, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "lingering.pid")
			t.Cleanup(func() { killPIDFile(t, pidFile) })
			ctx, cancel := context.WithTimeout(context.Background(), testCase.budget)
			defer cancel()
			script := `echo answered; echo refused >&2; sleep 30 & echo $! >"$1"; exit ` + strconv.Itoa(testCase.exit)

			started := time.Now()
			out, err := RunCommand(ctx, procgroup.NoReaper, testCase.budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", pidFile})
			elapsed := time.Since(started)

			if elapsed < procgroup.WaitDelay {
				t.Fatalf("the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
			}
			if testCase.wantErr == "" {
				if err != nil || out != "answered" {
					t.Errorf("RunCommand = %q, %v after %s; want \"answered\" and no error: the command exited 0 on its own", out, err, elapsed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) || strings.Contains(err.Error(), "timed out") {
				t.Errorf("RunCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own inside its %s budget",
					out, err, elapsed, testCase.wantErr, testCase.budget)
			}
		})
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

// A read whose budget ended answered nothing, so its -1 exit is no answer: the credential read was
// read as a stale key, which rewrote the clone's helpers, and the repository cache's read of its
// HEAD as a HEAD that names nothing, which rewrote HEAD. Each is now refused as the timeout it is.
func TestAReadItsBudgetEndedIsRefusedNotReadAsAnAnswer(t *testing.T) {
	t.Run("the shared clone's credential read", func(t *testing.T) {
		dir := t.TempDir()
		rewrote, err := ConfigureRepositoryCredential(context.Background(), timesOut{argv: []string{"git", "--git-dir=" + dir + "/.git", "config", "--get-all"}}, dir)
		if err == nil || rewrote || !strings.HasPrefix(err.Error(), "read the shared clone's credential.helper: command timed out: ") {
			t.Errorf("ConfigureRepositoryCredential with its read timed out = %t, %v; want no rewrite and the timeout refused", rewrote, err)
		}
	})
	t.Run("the repository cache's read of its HEAD", func(t *testing.T) {
		gitDir := t.TempDir()
		credential, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
		if err != nil {
			t.Fatal(err)
		}
		held := &fetchCredential{remote: credential, env: credential.env, read: staticToken(testInstallationToken), held: []string{testInstallationToken}}
		run := timesOut{argv: []string{"git", "--git-dir=" + gitDir, "symbolic-ref", "--quiet"}}
		err = followHead(context.Background(), run, fetchRequest(t), gitDir, map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, held, time.Now().Add(time.Minute))
		if err == nil || !strings.HasPrefix(err.Error(), "command timed out: "+strings.Join(isolatedGit, " ")+" --git-dir="+gitDir+" symbolic-ref --quiet HEAD") {
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
// checkedResult's runs are (runFailure), where these two wrapped the bare signal: the shared clone's
// credential read, and the repository cache's read of its HEAD before it follows GitHub's
// (followHead), which was `signal: killed` alone.
func TestAReadASignalEndedNamesItsCommandAndWhatItPrinted(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	const said = "fatal: could not read .git/packed-refs: Input/output error"
	t.Run("the shared clone's credential read", func(t *testing.T) {
		dir := t.TempDir()
		_, err := ConfigureRepositoryCredential(context.Background(), killsOne{argv: []string{"git", "--git-dir=" + dir + "/.git", "config", "--get-all"}, said: said, end: killed}, dir)
		if want := "read the shared clone's credential.helper: run git --git-dir=" + dir + "/.git config --get-all credential.helper: signal: killed\n" + said; err == nil || err.Error() != want {
			t.Errorf("ConfigureRepositoryCredential = %v, want %q", err, want)
		}
	})
	t.Run("the repository cache's read of its HEAD", func(t *testing.T) {
		gitDir := t.TempDir()
		credential, err := newProvisioningCredential(t.TempDir(), testInstallationToken)
		if err != nil {
			t.Fatal(err)
		}
		held := &fetchCredential{remote: credential, env: credential.env, read: staticToken(testInstallationToken), held: []string{testInstallationToken}}
		run := killsOne{argv: []string{"git", "--git-dir=" + gitDir, "symbolic-ref"}, said: said, end: killed}
		err = followHead(context.Background(), run, fetchRequest(t), gitDir, map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, held, time.Now().Add(time.Minute))
		if want := "run " + strings.Join(isolatedGit, " ") + " --git-dir=" + gitDir + " symbolic-ref --quiet HEAD: signal: killed\n" + said; err == nil || err.Error() != want {
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
// assertions by design (a backgrounded sleep standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
