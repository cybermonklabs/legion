package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The two rows of this file drive the stop procgroup.Configure gives every git and jj of this
// package (SIGTERM to the command's own process group, a grace, SIGKILL to whatever outlived it)
// with the real tools of the machine instead of a shell script standing in for one, and between
// them they cover both command runners: execRunner.Run, which provisioning's git and jj go
// through (config.go), and RunCommand, which the shim's and the launcher's do (jjgit.go). The
// scripted rows of stopsignal_test.go hold what each half of that stop is; these two hold that a
// real git and a real jj, holding the lock files a real one holds, meet it.

// realTool is the git or jj of this machine, resolved from PATH as the daemon resolves its tools
// at boot. A machine without it fails these rows rather than skipping them: they are the only two
// that drive this stop with the real tools, and a skip would let a tree whose stop no longer
// removes a git lock pass a run that reported nothing. It resolves the binary itself, rather than
// taking testTools's wrapper (workspace_test.go), because these rows reach no remote and so need
// nothing added to the runner's transport allow-list, and a wrapper would put a shell between the
// runner and the tool whose own signal handling each row reads.
func realTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("this row drives a real %s: %v", name, err)
	}
	return path
}

// lockPoll is how often these rows ask whether a lock file is there. jj holds the git index lock
// for about 6 ms of a 3.5 s `jj status` over 5,000 new files (measured in an issue pod on
// 2026-11-12 with jj 0.45.1), so a row that asked at the 10 ms pace of cancelOnceExists
// (errwaitdelay_test.go), whose command holds its marker for the rest of the run, would usually
// step over the window it waits for.
const lockPoll = 200 * time.Microsecond

// whenLockAppears answers, on the channel it returns, the moment lock first exists. It stops at
// that first sight or once done is closed, so its goroutine never outlives the run it watches,
// and a run that never took the lock leaves the channel empty rather than reporting a time.
func whenLockAppears(lock string, done <-chan struct{}) <-chan time.Time {
	appeared := make(chan time.Time, 1)
	go func() {
		ticker := time.NewTicker(lockPoll)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(lock); err == nil {
				appeared <- time.Now()
				return
			}
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return appeared
}

// locksUnder is every lock file left under dir, as paths relative to it: git and jj's git backend
// both take what they lock by writing `<name>.lock` beside it, and a stopped command that left
// one stops every later git and jj of that repository (staleGitLockNote, jjgit.go). A directory
// that is gone by the time the walk reaches it is no lock: the walk runs once the run has
// returned, so what it finds is what the stop left.
func locksUnder(t *testing.T, dir string) []string {
	t.Helper()
	var locks []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		locks = append(locks, relative)
		return nil
	})
	if err != nil {
		t.Fatalf("read the lock files left under %s: %v", dir, err)
	}
	return locks
}

// slowCleanFilter is the clean filter the repository of TestARealGitStoppedAtItsBudgetLeavesNoLock
// runs on every path it adds. git runs a filter through a shell, so this is a command line and
// not an argv, and it sleeps far past that row's budget: the budget's end, never the filter's own
// exit, is what ends that git. It is what holds .git/index.lock open for as long as the row
// needs, because neither runner gives a command a pipe that stays open — execRunner.Run passes no
// standard input at all and RunCommand passes a byte slice that ends at once — so a git waiting
// on its own standard input could not serve.
const slowCleanFilter = "sleep 300; cat"

// A real git the budget stops leaves no lock behind. git holds .git/index.lock for the whole of
// an `git add` and removes it from the signal handler of its own lockfile API; SIGKILL runs no
// handler, so a git killed outright leaves that lock and every later git and jj of the repository
// fails on it (dispatch://INFRA-3098, procgroup.stopGrace). The repository is a real one whose
// `filter.slow.clean` sleeps past the budget and whose .gitattributes points every path at that
// filter, and the add runs through execRunner.Run, the runner provisioning's own git goes
// through, under a budget shorter than the sleep: what the row reads is the production stop's
// own SIGTERM reaching a real git that holds a real lock.
func TestARealGitStoppedAtItsBudgetLeavesNoLock(t *testing.T) {
	git := realTool(t, "git")
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	runSetup(t, root, fixturegit.InitWorkTree(repository, "main")...)
	runSetup(t, repository, "git", "config", "filter.slow.clean", slowCleanFilter)
	for name, content := range map[string]string{
		".gitattributes": "* filter=slow\n",
		"work.txt":       "the one file this git adds\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	budget := time.Second
	done := make(chan struct{})
	defer close(done)
	appeared := whenLockAppears(filepath.Join(repository, ".git", "index.lock"), done)
	run := NewRunner(budget, map[string]string{"git": git})

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Dir: repository, Argv: []string{"git", "add", "work.txt"}, Timeout: budget})
	elapsed := time.Since(started)

	select {
	case took := <-appeared:
		t.Logf("the git had taken .git/index.lock %s into its %s budget, and the run returned after %s", took.Sub(started), budget, elapsed)
	default:
		t.Fatalf("the git never took .git/index.lock inside its %s budget, so this run proves nothing about the lock its stop removes (Run = exit %d, timed out %t, error %v, stderr %q)",
			budget, result.ExitCode, result.TimedOut, err, result.Stderr)
	}
	// exec reports a command whose Cancel succeeded as the context's error, so a Run that read
	// that error before its own kill would report this git as one it could not run.
	if err != nil {
		t.Fatalf("Run = %v after %s, want no error: a git its own budget stopped is a timeout, not a command that could not be run", err, elapsed)
	}
	if !result.TimedOut {
		t.Errorf("Run = exit %d, TimedOut false after %s; want timed out: the budget, not the filter, ended this git", result.ExitCode, elapsed)
	}
	if locks := locksUnder(t, filepath.Join(repository, ".git")); len(locks) > 0 {
		t.Errorf("the git left %v under .git after %s: the budget's end gave it no SIGTERM to remove its lock on, and every later git and jj of this repository fails on it", locks, elapsed)
	}
	if elapsed > budget+commandStopGrace/2 {
		t.Errorf("the run took %s, past its %s budget plus half the %s grace: a git that leaves on its SIGTERM must end the grace, not sit out its whole span", elapsed, budget, commandStopGrace)
	}
}

// jjWorkingCopyFiles is how large the working copy of TestARealJJStoppedAtItsBudgetIsRecorded is.
// jj holds the git index lock for a few milliseconds of a snapshot however large the working copy
// (about 5.4 ms of a 1.55 s `jj status` over 2,000 files and about 6 ms of a 3.5 s one over 5,000,
// measured in an issue pod on 2026-11-12), so what the size buys is the span the row has to find
// the lock in, not a wider window: over 5,000 files the lock is taken about 1.3 s in, which is
// time enough for a poll at lockPoll to see it. 5,000 files cost about 4 s of command in an issue
// pod of two cores, and less on a CI runner.
const jjWorkingCopyFiles = 5000

// jjSnapshotAttempts is how many snapshots TestARealJJStoppedAtItsBudgetIsRecorded may start to
// catch the git index lock. jj holds that lock for about 6 ms of a snapshot, and a test goroutine
// the scheduler keeps off a core for that span misses it while the subject of the row — the stop
// a cancel runs — is unchanged, so a row of one attempt would redden a pull request that broke
// nothing on a CI runner running this package beside others. Three, because a miss is already
// unlikely (the poll at lockPoll saw the lock in every measured run) and because the attempts
// are not free: each adds a batch of jjWorkingCopyFiles files of its own to the working copy, so
// a snapshot that runs to the end takes 3.3 s, then 10.5 s, then 21.1 s as the working copy grows
// 5,000, 10,000 and 15,000 files (measured in an issue pod of two cores on 2026-11-12). An
// attempt that sees the lock stops its snapshot where it stands, so the row normally costs the
// first of those alone.
const jjSnapshotAttempts = 3

// A real jj stopped mid-command is recorded, not asserted. The row builds a colocated jj
// workspace large enough that `jj status` holds .git/index.lock for a measurable time, starts it
// through RunCommand — the runner the shim's and the launcher's jj go through — under a context
// of its own, polls for that lock, and cancels the moment it is there, which runs the same Cancel
// the budget's end runs (procgroup.Configure). What it asserts is that the run ends inside the
// grace, procgroup.WaitDelay and a margin: a jj the stop reaches is a jj that leaves.
//
// It does not assert that no lock is left. jj removed the git index lock in 13 of 14 measured
// SIGTERM runs and left it in the fourteenth (dispatch://INFRA-3098, measured in an issue pod on
// 2026-11-12), and a rate like that is no assertion a row can hold: it would fail about one run
// in fourteen whatever the tree does. So the row writes what it found — which lock files remain,
// and how long the stop took — into the test's own output, where a reader of a `-v` run sees it,
// and the sweep that removes such a lock when the pod's workspace is provisioned is what the tree
// relies on (`workspace-init provision`, cmd/legion/workspace_init.go).
func TestARealJJStoppedAtItsBudgetIsRecorded(t *testing.T) {
	jj := realTool(t, "jj")
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runSetup(t, root, fixturegit.JJInit(work, true)...)
	content := []byte(strings.Repeat("the working copy this jj snapshots\n", 2))

	var missed []time.Duration
	for attempt := range jjSnapshotAttempts {
		// Every attempt makes the working copy dirty again, under a directory of its own: an
		// attempt missed the lock because its snapshot ran to the end, which leaves the working
		// copy clean, and a `jj status` over a clean working copy writes no index and so takes
		// no lock at all.
		batch := filepath.Join(work, fmt.Sprintf("batch-%d", attempt))
		if err := os.MkdirAll(batch, 0o755); err != nil {
			t.Fatal(err)
		}
		for file := range jjWorkingCopyFiles {
			if err := os.WriteFile(filepath.Join(batch, fmt.Sprintf("file-%04d.txt", file)), content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		elapsed, seen := stoppedAtTheIndexLock(t, jj, work, (attempt+1)*jjWorkingCopyFiles)
		if seen {
			return
		}
		missed = append(missed, elapsed)
	}
	t.Fatalf("%d snapshots of this workspace, each over %d files more than the one before it, ended without this row seeing .git/index.lock (they took %v): every one was over before the poll at %s saw the lock, so nothing was stopped while jj held it",
		jjSnapshotAttempts, jjWorkingCopyFiles, missed, lockPoll)
}

// stoppedAtTheIndexLock is one attempt of TestARealJJStoppedAtItsBudgetIsRecorded: it starts
// `jj status` over the workspace work through RunCommand under a context of its own, polls for
// the git index lock at lockPoll, and cancels at the first sight of it. files is how many files
// that working copy holds by this attempt, which its record names. It answers how long the run
// took and whether it saw the lock; an attempt that never saw it leaves the snapshot it ran to
// the end for its caller to count, and everything the row asserts and records of the attempt
// that did see it is here.
func stoppedAtTheIndexLock(t *testing.T, jj, work string, files int) (time.Duration, bool) {
	// No t.Helper: what this function logs is the row's whole record of the attempt, and a
	// failure of its bound is this function's own, so both read better named by the line here
	// that wrote them than by the caller's loop.

	// The budget is far longer than the command needs, so the cancel below, never the budget, is
	// what ends this jj; the stop both run is the same one.
	budget := time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	appeared := whenLockAppears(filepath.Join(work, ".git", "index.lock"), done)
	type outcome struct {
		out     string
		err     error
		elapsed time.Duration
	}
	returned := make(chan outcome, 1)

	started := time.Now()
	go func() {
		out, err := RunCommand(ctx, budget, started, setupEnvironment(), work, nil, []string{jj, "status"})
		returned <- outcome{out: out, err: err, elapsed: time.Since(started)}
	}()
	var took time.Time
	select {
	case took = <-appeared:
	case ended := <-returned:
		return ended.elapsed, false
	}
	cancel()
	cancelled := time.Now()

	// The stop is SIGTERM to the group, procgroup's grace, and SIGKILL to whatever outlived it,
	// and procgroup.WaitDelay behind that for anything still holding the command's pipes; the
	// margin is for a loaded machine.
	bound := commandStopGrace + procgroup.WaitDelay + time.Second
	var ended outcome
	select {
	case ended = <-returned:
	case <-time.After(bound):
		t.Fatalf("`jj status` was still running %s after the cancel, past the %s grace, the %s wait delay and a second of margin: the stop never ended it", time.Since(cancelled), commandStopGrace, procgroup.WaitDelay)
	}
	if stopping := ended.elapsed - took.Sub(started); stopping > bound {
		t.Errorf("`jj status` took %s from the cancel to its return, past the %s this stop may take", stopping, bound)
	}
	t.Logf("`jj status` over %d files took .git/index.lock %s in, the cancel landed there, and the run returned %s later, %s in all",
		files, took.Sub(started), ended.elapsed-took.Sub(started), ended.elapsed)
	t.Logf("RunCommand = %q, %v", ended.out, ended.err)
	// The git index lock is read apart from the rest: it is the one this issue is about, the one
	// a later git or jj of the repository fails on. jj's own `.jj` locks are not that — a
	// workspace holding an empty .jj/git_import_export.lock and .jj/working_copy/working_copy.lock
	// ran `jj status` to a clean exit and removed both (measured in an issue pod on 2026-11-12
	// with jj 0.45.1) — and a clean `jj status` leaves none of the three, so what this names is
	// what the stop left.
	indexLock := filepath.Join(".git", "index.lock")
	if _, err := os.Stat(filepath.Join(work, indexLock)); err == nil {
		t.Logf("this stop left %s: a later git or jj of this workspace fails on it until the sweep removes it", indexLock)
	} else {
		t.Logf("this stop left no %s: the jj it reached removed the lock it held", indexLock)
	}
	t.Logf("lock files left in the workspace: %v", locksUnder(t, work))
	return ended.elapsed, true
}
