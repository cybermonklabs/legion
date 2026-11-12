package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Every row here works a real clone on disk — a git directory, the issue workspace's own worktree
// entry under it and real lock files — because the walk the sweep reads (workspace.GitLocks) tells
// this workspace's locks from another workspace's by the gitdir file of each worktree entry, and a
// fixture that answered the paths itself would prove nothing about the clone a pod holds.
//
// No row here calls t.Parallel, and none may: the rows that drive a launcher use t.Chdir, which
// panics in a parallel test, and the package's sweep numbers are package variables a row shortens.

// lockClone is one issue pod's clone and the workspace in it, as the sweep reads them: the shared
// clone's git directory, that workspace's worktree entry under it, and the two jj files
// workspace.GitDir resolves the git directory through (.jj/repo and the repository's
// store/git_target).
type lockClone struct {
	t *testing.T
	// dir is the jj workspace, clone the shared clone, gitDir the clone's git directory and
	// worktree the workspace's own entry under gitDir/worktrees. repo is the jj repository
	// .jj/repo names.
	dir, clone, gitDir, worktree, repo string
}

func newLockClone(t *testing.T) *lockClone {
	t.Helper()
	root := t.TempDir()
	c := &lockClone{t: t, dir: filepath.Join(root, "ws"), clone: filepath.Join(root, "clone")}
	c.gitDir = filepath.Join(c.clone, ".git")
	c.worktree = filepath.Join(c.gitDir, "worktrees", "ws")
	c.repo = filepath.Join(c.clone, ".jj", "repo")
	for _, dir := range []string{filepath.Join(c.gitDir, "refs", "heads"), c.worktree, filepath.Join(c.repo, "store"), filepath.Join(c.dir, ".jj")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// What jj and git leave in a colocated clone with one workspace added: the workspace's
	// .jj/repo names the repository, the repository's store names the git directory, and the
	// clone's worktree entry for this workspace names the workspace's own .git.
	c.write(filepath.Join(c.dir, ".jj", "repo"), c.repo+"\n")
	c.write(filepath.Join(c.repo, "store", "git_target"), c.gitDir+"\n")
	c.write(filepath.Join(c.worktree, "gitdir"), filepath.Join(c.dir, ".git")+"\n")
	c.write(filepath.Join(c.dir, ".git"), "gitdir: "+c.worktree+"\n")
	c.write(filepath.Join(c.gitDir, "HEAD"), "ref: refs/heads/main\n")
	return c
}

func (c *lockClone) write(path, content string) {
	c.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// lock plants one lock file at name under the clone's git directory, holding content and last
// written age ago, and answers its path.
func (c *lockClone) lock(name, content string, age time.Duration) string {
	c.t.Helper()
	return c.plant(filepath.Join(c.gitDir, filepath.FromSlash(name)), content, age)
}

// worktreeLock plants one under the workspace's own worktree entry, which git locks as it locks
// the clone's own files: the entry holds this workspace's HEAD, index and refs.
func (c *lockClone) worktreeLock(name, content string, age time.Duration) string {
	c.t.Helper()
	return c.plant(filepath.Join(c.worktree, filepath.FromSlash(name)), content, age)
}

func (c *lockClone) plant(path, content string, age time.Duration) string {
	c.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.t.Fatal(err)
	}
	c.write(path, content)
	written := time.Now().Add(-age)
	if err := os.Chtimes(path, written, written); err != nil {
		c.t.Fatal(err)
	}
	return path
}

// sweep is a sweep over this clone with the launcher's own walk, git directory, removal and
// clock, and a /proc root of the row's own. The numbers are the package's, which a row changes on
// the sweep it was given rather than on the package, since nothing but its own sweep reads them
// here.
func (c *lockClone) sweep(log *lockedBuffer, proc string) *gitLockSweep {
	c.t.Helper()
	return &gitLockSweep{
		proc: proc, guard: gitLockGuard, interval: gitLockInterval, recheck: time.Millisecond,
		locks: workspace.GitLocks, gitDir: workspace.GitDir, remove: os.Remove, now: time.Now,
		log: (&roleLog{stdout: log, stderr: log}).linef, workspace: c.dir,
	}
}

// procRoot is a /proc root of the row's own: one symlink per process it cares about, named by
// that process's pid and pointing at its entry in the real /proc, so the holder read walks the
// processes the row planted and not every process of the machine.
func procRoot(t *testing.T, pids ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, pid := range pids {
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(root, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// swept says whether path is gone, which is how a row reads a lock the sweep removed.
func swept(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// requireSwept fails the row with the sweep's own log when path is still there.
func requireSwept(t *testing.T, log *lockedBuffer, path string) {
	t.Helper()
	if !swept(path) {
		t.Fatalf("the sweep left %s where it was; its log:\n%s", path, log.String())
	}
}

// requireLine fails the row with the whole log when it holds no such line.
func requireLine(t *testing.T, log *lockedBuffer, want string) {
	t.Helper()
	for _, line := range strings.Split(log.String(), "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("the log holds no line %q; it holds:\n%s", want, log.String())
}

// shortGitLockClock shortens the package's sweep numbers for a row that drives a real launcher,
// which builds its sweep from them (newGitLockSweep), and restores them at the row's end. A row
// that builds its own sweep sets its fields instead. It is called before the launcher starts, so
// the restore below runs after the launcher's own cleanup, which waits for Run to return: a
// goroutine still reading one of these after a row restored it is a data race the race detector
// fails the row for.
func shortGitLockClock(t *testing.T, interval, recheck time.Duration) {
	t.Helper()
	previousInterval, previousRecheck := gitLockInterval, gitLockRecheck
	gitLockInterval, gitLockRecheck = interval, recheck
	t.Cleanup(func() { gitLockInterval, gitLockRecheck = previousInterval, previousRecheck })
}

// roleStart is a start of role's generation that runs argv and exits, carrying env besides the
// workspace it names: the restore and the sweep both read LEGION_WORKSPACE from the environment
// the launcher merges for the generation.
func roleStart(generation uint64, ws string, env []string) shimwire.LauncherStart {
	return shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation,
		Argv: []string{"/bin/sh", "-c", "exit 0"},
		Env:  append(append([]string{}, env...), "LEGION_WORKSPACE="+ws),
	}
}

// answersNothingFor fails the row unless the launcher sent nothing on conn for d: what a role
// start held behind the sweep's first pass looks like from the daemon's end.
func answersNothingFor(t *testing.T, conn *roleConn, d time.Duration) {
	t.Helper()
	if err := conn.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	line, err := conn.reader.ReadBytes('\n')
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("the launcher answered %q (%v) while the sweep's first pass was held", line, err)
	}
	if err := conn.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

// The launcher's first pass removes every lock of the clone that no process holds open, whatever
// its age: it runs before the launcher serves any role and before it starts the pod's test
// database, so no process of the container can hold one, and a lock written a second before the
// pod started is as stale as one written a day before. The locks of the clone's own git directory
// and of the workspace's worktree entry go alike.
func TestTheFirstPassRemovesEveryLockAtLauncherStart(t *testing.T) {
	c := newLockClone(t)
	index := c.lock("index.lock", "one", time.Second)
	ref := c.lock("refs/heads/legion.lock", "two", time.Second)
	working := c.worktreeLock("index.lock", "three", time.Second)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), true)
	for _, path := range []string{index, ref, working} {
		requireSwept(t, log, path)
		if !strings.Contains(log.String(), "removed the git lock "+path+",") {
			t.Fatalf("the first pass removed %s and wrote no line for it:\n%s", path, log.String())
		}
	}

	// The same locks under the clock's rule: a pass that runs once the roles are working keeps
	// each of them, because the guard is what a lock a live writer took looks like.
	t.Run("a clock pass keeps what the first pass removed unasked", func(t *testing.T) {
		again := c.lock("index.lock", "one", time.Second)
		clock := &lockedBuffer{}
		s := c.sweep(clock, procRoot(t, os.Getpid()))
		s.log = (&roleLog{stdout: clock, stderr: clock}).linef
		s.pass(context.Background(), false)
		if swept(again) {
			t.Fatalf("a clock pass removed a lock written a second ago:\n%s", clock.String())
		}
		if !strings.Contains(clock.String(), "younger than the "+gitLockGuard.String()+" guard") {
			t.Fatalf("the clock pass kept it and said %q", clock.String())
		}
	})
}

// A lock some process still holds open is never removed, the first pass included: two writers
// would then replace one file. The first pass's rule is about the age bound and the second read,
// not about the holder read, which costs one walk of /proc and keeps a lock open in a process the
// sweep did not expect — a `kubectl exec` into the container, a role's jj that raced the
// launcher's own start.
func TestTheFirstPassKeepsALockAProcessHoldsOpen(t *testing.T) {
	c := newLockClone(t)
	held := c.lock("index.lock", "held", time.Second)
	free := c.lock("refs/heads/legion.lock", "free", time.Second)
	file, err := os.OpenFile(held, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), true)
	if swept(held) {
		t.Fatalf("the first pass removed a lock this process holds open:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "kept the git lock "+held+",") ||
		!strings.Contains(log.String(), ": a process of the container holds it open") {
		t.Fatalf("the first pass kept %s and said:\n%s", held, log.String())
	}
	requireSwept(t, log, free)

	// The descriptor is compared with the lock by device and inode, never by the path the link
	// prints: a lock renamed since it was opened is the same file, and the link still names
	// where it was.
	t.Run("the holder is read by device and inode", func(t *testing.T) {
		renamed := filepath.Join(filepath.Dir(held), "packed-refs.lock")
		if err := os.Rename(held, renamed); err != nil {
			t.Fatal(err)
		}
		moved := &lockedBuffer{}
		s := c.sweep(moved, procRoot(t, os.Getpid()))
		s.pass(context.Background(), true)
		if swept(renamed) {
			t.Fatalf("the pass removed the file the open descriptor points at:\n%s", moved.String())
		}
		if !strings.Contains(moved.String(), "kept the git lock "+renamed+",") {
			t.Fatalf("the pass kept %s and said:\n%s", renamed, moved.String())
		}
	})
}

// The pod's one workspace-bundle restore runs real git and jj against the clone, and a stale lock
// fails it: the restore is exactly the work a wedged clone costs the issue before a role has even
// started. The first pass runs before it, both ways the launcher reaches a workspace — from its
// own working directory, before it serves any role at all, and from the first role start, whose
// answer waits for the pass.
func TestTheFirstPassRunsBeforeTheBundleRestore(t *testing.T) {
	t.Run("a lock that would fail the restore is gone before it runs", func(t *testing.T) {
		testbin.RequireJJFilters(t)
		requireRoleNamespaces(t)
		f := newBundleFixture(t)
		f.workOne()
		saved := f.bundleWorkspaceOne()
		ws := f.secondPod("ws2")
		// The lock the restore's own `git update-ref` takes for each saved head: one left
		// behind fails that command with "cannot lock ref", and the restore gives up with the
		// released pod's work left in the daemon's database for good.
		gitDir := f.gitDir(ws)
		var planted []string
		for index := range saved.Tips {
			path := filepath.Join(gitDir, "refs", "heads", restoreRefPrefix+strconv.Itoa(index)+".lock")
			if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			planted = append(planted, path)
		}

		t.Chdir(ws)
		fleet := newFleet(t, 5*time.Second, "tester")
		for _, path := range planted {
			if !swept(path) {
				t.Fatalf("the launcher's first pass left %s where it was; its log:\n%s", path, fleet.stderr.String())
			}
		}
		frames, err := shimwire.SplitWorkspaceBundle("restore-ws2", bundleRelease, saved)
		if err != nil {
			t.Fatal(err)
		}
		conn := fleet.role("tester")
		for _, frame := range frames {
			if got, ok := conn.answer(frame).(shimwire.LauncherBundleResult); !ok || !got.OK {
				t.Fatalf("slice %d answered %#v", frame.Index, got)
			}
		}
		if got := conn.start(roleStart(1, ws, f.env)); !got.OK {
			t.Fatalf("the start after the bundle answered %#v; the launcher's log:\n%s", got, fleet.stderr.String())
		}
		conn.awaitExit(1)
		if conn.restored == nil || !conn.restored.OK {
			t.Fatalf("the launcher reported %#v of the restore; its log:\n%s", conn.restored, fleet.stderr.String())
		}
		if !strings.Contains(fleet.stderr.String(), "the released workspace's saved work is restored") {
			t.Fatalf("the restore left no notice in the container's log:\n%s", fleet.stderr.String())
		}
		// The work the stale lock would have cost the issue is in the workspace.
		if body, err := os.ReadFile(filepath.Join(ws, "wip.txt")); err != nil || string(body) != "wip\n" {
			t.Fatalf("the restored working copy's own change reads %q (%v)", body, err)
		}
	})

	t.Run("no role start is answered until the pass returns", func(t *testing.T) {
		requireRoleNamespaces(t)
		c := newLockClone(t)
		lock := c.lock("index.lock", "stale", time.Second)
		// The pass is held where it reads the workspace's repository: .jj/repo is a fifo, whose
		// read blocks until this row opens it for writing. The launcher learns the workspace
		// from the start below, so nothing reads the fifo before that start arrives.
		repoFile := filepath.Join(c.dir, ".jj", "repo")
		if err := os.Remove(repoFile); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(repoFile, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(t.TempDir())
		f := newFleet(t, 5*time.Second, "tester")
		conn := f.role("tester")
		conn.send(roleStart(1, c.dir, nil))
		answersNothingFor(t, conn, 300*time.Millisecond)
		if swept(lock) {
			t.Fatal("the pass removed the lock while it was still held at the workspace's repository")
		}

		released := make(chan error, 1)
		go func() { released <- os.WriteFile(repoFile, []byte(c.repo+"\n"), 0o600) }()
		select {
		case err := <-released:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
			t.Fatalf("the launcher never read the workspace's repository; its log:\n%s", f.stderr.String())
		}
		if got, ok := conn.reply().(shimwire.LauncherStartResult); !ok || !got.OK {
			t.Fatalf("the start answered %#v once the pass returned; the launcher's log:\n%s", got, f.stderr.String())
		}
		if !swept(lock) {
			t.Fatalf("the start was answered with %s still there; the launcher's log:\n%s", lock, f.stderr.String())
		}
		// The fifo served its one read; a later pass of the pod's clock reads a file again.
		if err := os.Remove(repoFile); err != nil {
			t.Fatal(err)
		}
		c.write(repoFile, c.repo+"\n")
	})
}

// An issue pod sets its roles container's working directory to the issue's workspace
// (internal/runtime/sandbox's podkind), so the launcher needs nothing from the daemon to know
// which clone is its own: it sweeps before it connects, and a role that starts at once works a
// clone whose stale locks are gone.
func TestTheLauncherTakesTheWorkspaceFromItsWorkingDirectory(t *testing.T) {
	c := newLockClone(t)
	lock := c.lock("index.lock", "stale", time.Second)
	worktree := c.worktreeLock("HEAD.lock", "stale", time.Second)
	t.Chdir(c.dir)
	f := newFleet(t, 2*time.Second, "tester")
	for _, path := range []string{lock, worktree} {
		if !swept(path) {
			t.Fatalf("the launcher connected with %s still there; its log:\n%s", path, f.stderr.String())
		}
		if !strings.Contains(f.stderr.String(), "removed the git lock "+path+",") {
			t.Fatalf("the launcher removed %s and wrote no line for it:\n%s", path, f.stderr.String())
		}
	}
}

// A launcher whose working directory is no jj workspace — a missing or unreadable .jj/repo and a
// missing store/git_target alike — says so once and sweeps nothing, since it has no clone to
// sweep. The first role start it serves names the workspace (LEGION_WORKSPACE), and that is the
// clone it sweeps, under the first pass's rule, before the start's own restore runs.
func TestALauncherWhoseDirectoryIsNoWorkspaceSweepsAtTheFirstRoleStart(t *testing.T) {
	requireRoleNamespaces(t)
	c := newLockClone(t)
	lock := c.lock("index.lock", "stale", time.Second)
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	f := newFleet(t, 5*time.Second, "tester")
	if !strings.Contains(f.stderr.String(), "working directory "+elsewhere+" is no jj workspace") {
		t.Fatalf("the launcher's log names no working directory of its own:\n%s", f.stderr.String())
	}
	if swept(lock) {
		t.Fatalf("a launcher with no workspace swept a clone nothing named to it:\n%s", f.stderr.String())
	}

	conn := f.role("tester")
	if got := conn.start(roleStart(1, c.dir, nil)); !got.OK {
		t.Fatalf("the start answered %#v; the launcher's log:\n%s", got, f.stderr.String())
	}
	if !swept(lock) {
		t.Fatalf("the start named %s and the launcher swept nothing; its log:\n%s", c.dir, f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "the git lock sweep works the workspace "+c.dir) {
		t.Fatalf("the launcher swept the start's workspace and said:\n%s", f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "removed the git lock "+lock+",") {
		t.Fatalf("the launcher removed %s and wrote no line for it:\n%s", lock, f.stderr.String())
	}
}

// The sweep ends with the container's context, and Run waits for it as it waits for the pod's
// test database: a pass still walking a clone after Run returned would be the launcher writing to
// a volume the pod is giving up.
func TestTheSweepStopsWithTheContainerContextBeforeRunReturns(t *testing.T) {
	t.Run("the ticker ends with the context", func(t *testing.T) {
		c := newLockClone(t)
		log := &lockedBuffer{}
		s := c.sweep(log, procRoot(t, os.Getpid()))
		s.interval = 20 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := s.serve(ctx)
		cancel()
		select {
		case <-done:
		case <-time.After(testwait.Bounded(t, 10*time.Second, time.Second)):
			t.Fatalf("the sweep's ticker outlived the container's context; its log:\n%s", log.String())
		}
	})

	t.Run("Run waits for the ticker", func(t *testing.T) {
		c := newLockClone(t)
		shortGitLockClock(t, 20*time.Millisecond, time.Millisecond)
		t.Chdir(c.dir)
		f := newFleet(t, 2*time.Second, "tester")
		running := c.lock("refs/heads/running.lock", "stale", time.Hour)
		testwait.Eventually(t, "the sweep's clock to remove "+running, func() bool { return swept(running) })

		f.cancel()
		select {
		case err := <-f.result:
			// Put it back: newFleet's own cleanup waits for this same send.
			f.result <- err
		case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
			t.Fatalf("Run did not return after its context ended; the launcher's log:\n%s", f.stderr.String())
		}
		after := c.lock("refs/heads/after.lock", "stale", time.Hour)
		if testwait.Holds(500*time.Millisecond, testwait.Poll, func() bool { return swept(after) }) {
			t.Fatalf("a pass ran after Run returned: the sweep outlived the container's own process")
		}
	})
}

// The sweep runs again on its clock for the life of the pod, with no role start between the
// passes: a lock a role leaves at any point of the pod's life is removed within an interval of
// becoming stale, which is the whole of what this half of the change is for.
func TestTheSweepRunsAgainOnItsClockForTheLifeOfThePod(t *testing.T) {
	c := newLockClone(t)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := s.serve(ctx)
	t.Cleanup(func() { cancel(); <-done })
	// Each of these is planted after the pass before it has run, so each goes on a clock pass of
	// its own, and the last two are passes the sweep ran with nothing but its ticker to run them.
	for _, name := range []string{"index.lock", "refs/heads/one.lock", "refs/heads/two.lock"} {
		path := c.lock(name, "stale", time.Hour)
		testwait.Eventually(t, "the clock pass that removes "+path, func() bool { return swept(path) })
		if !strings.Contains(log.String(), "removed the git lock "+path+",") {
			t.Fatalf("the clock pass removed %s and wrote no line for it:\n%s", path, log.String())
		}
	}
}

// A removal's line is what an operator reads out of the container's log once the pod is gone: the
// lock's path, how many bytes it held, when it was last written and how long before the pass that
// was.
func TestTheSweepLogsEachRemovalWithItsPathSizeTimeAndAge(t *testing.T) {
	c := newLockClone(t)
	const content = "ref: refs/heads/main\n"
	path := c.lock("packed-refs.lock", content, 0)
	written := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t))
	// Rounded to the second, as the rest of this tree prints a duration: the 400 ms go.
	s.now = func() time.Time { return written.Add(90*time.Minute + 400*time.Millisecond) }
	s.pass(context.Background(), true)
	requireSwept(t, log, path)
	line := strings.TrimSpace(log.String())
	requireLine(t, log, fmt.Sprintf("launcher: removed the git lock %s, %d bytes, last written %s (%s ago)",
		path, len(content), "2026-03-04T05:06:07Z", "1h30m0s"))
	for what, field := range map[string]string{
		"the path":            path,
		"the size in bytes":   strconv.Itoa(len(content)) + " bytes",
		"the last-write time": "2026-03-04T05:06:07Z",
		"the age":             "(1h30m0s ago)",
	} {
		if !strings.Contains(line, field) {
			t.Fatalf("the removal's line names %s nowhere: %q", what, line)
		}
	}
}

// A lock the sweep kept writes the same line, with `kept` for `removed` and the condition that
// kept it after a colon: the next pass decides it again, and the log says why this one did not.
func TestTheSweepLogsEachLockItKeepsWithTheConditionThatKeptIt(t *testing.T) {
	written := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	const content = "ref: refs/heads/main\n"

	t.Run("a process of the container holds it open", func(t *testing.T) {
		c := newLockClone(t)
		path := c.lock("index.lock", content, 0)
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatal(err)
		}
		log := &lockedBuffer{}
		s := c.sweep(log, procRoot(t, os.Getpid()))
		s.now = func() time.Time { return written.Add(2 * time.Hour) }
		s.pass(context.Background(), false)
		requireLine(t, log, fmt.Sprintf("launcher: kept the git lock %s, %d bytes, last written %s (%s ago): %s",
			path, len(content), "2026-03-04T05:06:07Z", "2h0m0s", "a process of the container holds it open"))
	})

	// The guard is printed as Go prints the duration, from the sweep's own field, so a test that
	// shortens it reads its own number rather than the package's 15 minutes.
	t.Run("it is younger than the guard", func(t *testing.T) {
		c := newLockClone(t)
		path := c.lock("index.lock", content, 0)
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatal(err)
		}
		log := &lockedBuffer{}
		s := c.sweep(log, procRoot(t))
		s.guard = 90 * time.Second
		s.now = func() time.Time { return written.Add(time.Minute) }
		s.pass(context.Background(), false)
		if swept(path) {
			t.Fatalf("a lock a minute old went under a 90 s guard:\n%s", log.String())
		}
		requireLine(t, log, fmt.Sprintf("launcher: kept the git lock %s, %d bytes, last written %s (%s ago): %s",
			path, len(content), "2026-03-04T05:06:07Z", "1m0s", "it is younger than the 1m30s guard"))
	})
}

// A pass over a clone nobody wedged says nothing at all. A line a minute for the life of a pod
// would bury every line its roles write, and the sweep has nothing to report about a clone whose
// locks are where git left them: none.
func TestAPassThatFindsNoLockLogsNothing(t *testing.T) {
	c := newLockClone(t)
	// Files git leaves that are no lock, under both the git directory and the worktree entry.
	c.plant(filepath.Join(c.gitDir, "packed-refs"), "# pack-refs with: peeled fully-peeled sorted\n", 0)
	c.plant(filepath.Join(c.worktree, "HEAD"), "ref: refs/heads/main\n", 0)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), true)
	s.pass(context.Background(), false)
	if log.String() != "" {
		t.Fatalf("a pass over a clone with no lock wrote:\n%s", log.String())
	}
}
