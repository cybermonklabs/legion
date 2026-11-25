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

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// procgroup.Err is execRunner's one classification of a command's error: nil for an ordinary exit
// or for exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay), either way read
// from cmd.ProcessState.ExitCode(). A fake git that exits 0 while a child it started keeps its
// stdout pipe open proves that wiring end to end: RunChecked must succeed with ExitCode 0, not
// report the command's own clean exit as a failure. The child runs until the test's cleanup
// releases it (heldChild), so every run holds the pipe past WaitDelay, and the budget is the rest
// of the test binary's deadline, far from the git's own exit on a loaded machine.
func TestRunCheckedSucceedsWhenACommandExitsZeroWithAChildStillHoldingItsOutputOpen(t *testing.T) {
	dir := t.TempDir()
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho done\n" + heldChild(t, false) + "\nexit 0\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	run := NewRunner(testwait.Remaining(t), map[string]string{"git": fakeGit})

	started := time.Now()
	result, err := RunChecked(context.Background(), run, []string{"git"}, nil, dir)
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
}

// A command that exits on its own did not time out while a child it left in its process group
// holds its output pipe past procgroup.WaitDelay: its exit is its answer. The budget is the rest
// of the test binary's deadline, so it passes nowhere near the git's own exit on a loaded machine.
// A budget that passes between Go's reap of the git and Wait's read of its context still meets
// exec's Cancel, whose stop the child in the group takes, so that run reports a kill, as
// procgroup.Configure's doc says; no row holds either answer there.
func TestRunReportsTheCommandsOwnFailureWhenAChildInItsGroupHoldsItsOutput(t *testing.T) {
	dir := t.TempDir()
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho 'fatal: unable to access' >&2\n" + heldChild(t, false) + "\nexit 128\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	timeout := testwait.Remaining(t)
	run := NewRunner(timeout, map[string]string{"git": fakeGit})

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run = %v, want the command's own exit", err)
	}
	if elapsed < procgroup.WaitDelay {
		t.Fatalf("Run returned after %s, inside WaitDelay: nothing held the pipe, so this run proves nothing", elapsed)
	}
	if result.TimedOut || result.ExitCode != 128 {
		t.Errorf("Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own, its budget far away", result.ExitCode, result.TimedOut, elapsed)
	}
}

// A command that exits on its own did not time out, however long procgroup.WaitDelay then holds
// Run open for a child still holding its output pipe, and however much of that time passes after
// its budget: a Run that read TimedOut from the deadline having passed would report this git's own
// exit 128 as a timeout, and a clone that failed transiently inside the last WaitDelay of its
// budget, git-remote-https still holding the pipe, would read as timed out, which
// transientCloneFailure never retries.
//
// The budget passes at a fact of the run, not at a time: the caller's deadline passes once the
// git's process group is gone (budgetPassedOnceTheGroupIsGone), after the git has exited on its
// own and Go has reaped it. The child leaves the group (setsid) before the git exits, so the stop
// that end sends finds nothing to signal. A Timeout counted from before the shell started could
// pass, on a loaded machine, before the shell had exited. Only a run in which the budget passed
// before Run read it (exitedHook, which Run calls just before) is judged: a run of the other order
// shows nothing of a Run that reads a passed budget, so the row runs the git again until a run of
// the right order comes, within a minute.
func TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline(t *testing.T) {
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	t.Cleanup(func() { exitedHook.Store(nil) })
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader, fakeGit := filepath.Join(dir, "leader"), filepath.Join(dir, "git")
		script := "#!/bin/sh\necho $$ >'" + leader + "'\necho 'fatal: unable to access' >&2\n" + heldChild(t, true) + "\nexit 128\n"
		if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
			t.Fatalf("write the fake git: %v", err)
		}
		ctx, watch := budgetPassedOnceTheGroupIsGone(t, leader, within)
		passedFirst := false
		hook := func(Command, *os.ProcessState) { passedFirst = errors.Is(ctx.Err(), context.DeadlineExceeded) }
		exitedHook.Store(&hook)
		timeout := testwait.Remaining(t)
		run := NewRunner(timeout, map[string]string{"git": fakeGit})

		started := time.Now()
		result, err := run.Run(ctx, Command{Dir: dir, Argv: []string{"git"}, Timeout: timeout})
		elapsed := time.Since(started)
		exitedHook.Store(nil)
		<-watch.done

		if !watch.gone {
			t.Fatalf("run %d: the git's process group was not gone within %s of the run's start, so its budget never passed (Run = exit %d, TimedOut %t, error %v after %s)",
				runs, within, result.ExitCode, result.TimedOut, err, elapsed)
		}
		if !passedFirst {
			if time.Now().Before(until) {
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the budget passed only after Run had read it", runs, within)
		}
		if err != nil {
			t.Fatalf("run %d: Run = %v, want the command's own exit", runs, err)
		}
		if elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d: Run returned after %s, inside WaitDelay: nothing held the pipe past the git's exit, so this run proves nothing", runs, elapsed)
		}
		if result.TimedOut || result.ExitCode != 128 {
			t.Errorf("run %d: Run = exit %d, TimedOut %t after %s; want exit 128, not timed out: git failed on its own before its budget passed", runs, result.ExitCode, result.TimedOut, elapsed)
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
// for a failure would report that success as an error. The budget is the rest of the test
// binary's deadline, far from the command's own exit on a loaded machine. (Ported with
// RunCommand's own semantics from the shim's runAdoptionCommand, which the adoption now reaches
// through RunCommand.)
func TestRunCommandReportsTheCommandsOwnExitWhenAChildInItsGroupHoldsItsPipes(t *testing.T) {
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
			script := "echo answered; echo refused >&2\n" + heldChild(t, false) + "\nexit " + strconv.Itoa(testCase.exit)

			started := time.Now()
			out, err := RunCommand(ctx, procgroup.NoReaper, budget, started, []string{"PATH=" + os.Getenv("PATH")}, t.TempDir(), nil, []string{"sh", "-c", script})
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
				t.Errorf("RunCommand = %q, %v after %s; want an error naming %q and no timeout: the command exited on its own, its budget far away",
					out, err, elapsed, testCase.wantErr)
			}
		})
	}
}

// A RunCommand command that exits on its own did not time out, however long procgroup.WaitDelay
// then holds the run open for a child still holding its output pipes, and however much of that
// time passes after its budget: a run that read a timeout from the deadline having passed would
// report jj's own exit 3 as a timeout. The budget passes once the command's process group is gone
// (budgetPassedOnceTheGroupIsGone), the child having left it (setsid), as in
// TestRunReportsTheCommandsOwnFailureNotATimeoutWhenAChildHoldsItsOutputPastTheDeadline. Only a
// run in which the budget passed before RunCommand read it is judged: the Starter's done, which
// procgroup.Run calls once the wait has read the exit, records whether it had.
func TestRunCommandReportsTheCommandsOwnExitWhenAChildHoldsItsPipesPastIt(t *testing.T) {
	within := testwait.Bounded(t, time.Minute, cutRunMargin)
	until := time.Now().Add(within)
	for runs := 1; ; runs++ {
		dir := t.TempDir()
		leader := filepath.Join(dir, "leader")
		ctx, watch := budgetPassedOnceTheGroupIsGone(t, leader, within)
		passedFirst := false
		start := func(cmd *exec.Cmd) (func(), error) {
			return func() { passedFirst = errors.Is(ctx.Err(), context.DeadlineExceeded) }, cmd.Start()
		}
		script := `echo $$ >"$1"; echo answered; echo refused >&2` + "\n" + heldChild(t, true) + "\nexit 3"
		budget := testwait.Remaining(t)

		started := time.Now()
		out, err := RunCommand(ctx, start, budget, started, []string{"PATH=" + os.Getenv("PATH")}, dir, nil, []string{"sh", "-c", script, "sh", leader})
		elapsed := time.Since(started)
		<-watch.done

		if !watch.gone {
			t.Fatalf("run %d: the command's process group was not gone within %s of the run's start, so its budget never passed (RunCommand = %q, %v after %s)",
				runs, within, out, err, elapsed)
		}
		if !passedFirst {
			if time.Now().Before(until) {
				continue
			}
			t.Fatalf("no run of the right order in %d runs over %s: in each, the budget passed only after RunCommand had read it", runs, within)
		}
		if elapsed < procgroup.WaitDelay {
			t.Fatalf("run %d: the run returned after %s, inside procgroup.WaitDelay: nothing held the pipe, so this run proves nothing", runs, elapsed)
		}
		if err == nil || !strings.Contains(err.Error(), "Command failed (exit 3)") || strings.Contains(err.Error(), "timed out") {
			t.Errorf("run %d: RunCommand = %q, %v after %s; want an error naming \"Command failed (exit 3)\" and no timeout: the command exited on its own before its budget passed",
				runs, out, err, elapsed)
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
// process group, so the group is gone once the command is.
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
	script := fmt.Sprintf("echo $$ >'%[1]s.tmp' && mv '%[1]s.tmp' '%[1]s'\nwhile [ ! -e '%[2]s' ] && kill -0 %[3]d 2>/dev/null; do sleep 0.05; done\n",
		pidFile, release, os.Getpid())
	if err := os.WriteFile(holder, []byte(script), 0o600); err != nil {
		t.Fatalf("write the held child's script: %v", err)
	}
	start := "sh '" + holder + "' &"
	if escaped {
		start = "setsid " + start
	}
	return start + "\nwhile [ ! -s '" + pidFile + "' ]; do sleep 0.01; done"
}

// cutRunMargin is what the rows bounded by a minute leave of the test binary's deadline
// (testwait.Bounded): the time a run the bound cuts takes to return, its stop's grace and
// WaitDelay, and a second for the row to fail in.
const cutRunMargin = procgroup.StopGrace + procgroup.WaitDelay + time.Second

// groupWatch is what budgetPassedOnceTheGroupIsGone saw of one run: whether the group was gone
// within its bound, written before done closes.
type groupWatch struct {
	done chan struct{}
	gone bool
}

// budgetPassedOnceTheGroupIsGone is a caller's context whose deadline passes as a budget's does
// (testwait.StandInDeadline: its Err and its cause context.DeadlineExceeded) once the process
// group of the command that writes its own pid to leader as its first act is gone. kill(-pid, 0)
// answers ESRCH once no member of that group is left: after the command has exited on its own and
// Go has reaped it, where its children have left the group (heldChild, escaped). Wait reaps the
// command before it starts the WaitDelay a child holding the pipes keeps the run open for. A group
// not gone within bound ends the context as a cancel, so a run still going then reads no budget
// passed; done closes either way.
func budgetPassedOnceTheGroupIsGone(t *testing.T, leader string, bound time.Duration) (context.Context, *groupWatch) {
	t.Helper()
	ctx, pass, cancel := testwait.StandInDeadline(context.Background())
	t.Cleanup(cancel)
	watch := &groupWatch{done: make(chan struct{})}
	go func() {
		defer close(watch.done)
		if watch.gone = testwait.Holds(bound, testwait.Poll, func() bool {
			pid := pidIn(leader)
			return pid > 0 && errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
		}); watch.gone {
			pass(context.DeadlineExceeded)
			return
		}
		cancel()
	}()
	return ctx, watch
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
