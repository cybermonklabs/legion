package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
	"golang.org/x/sys/unix"
)

// Every row here works a real clone on disk — a git directory, the issue workspace's own worktree
// entry under it and real lock files — because the walk the sweep reads (workspace.GitLocksOf)
// tells this workspace's locks from another workspace's by the gitdir file of each worktree
// entry, and a fixture that answered the paths itself would prove nothing about the clone a pod
// holds.
//
// No row here calls t.Parallel, and none may: the rows that drive a launcher use t.Chdir, which
// panics in a parallel test, and the package's sweep numbers are package variables a row shortens.

// The repository and the issue every clone of these rows belongs to. They name directories
// rather than anything the rows assert, but they have to be a repository and an issue
// workspace.Location would accept: provisioning puts a workspace at
// <state>/workspaces/<owner>/<name>/<issue> and its shared clone at
// <state>/repos/github.com/<owner>/<name>, and the launcher's sweep derives the clone from that
// layout and from nothing inside the workspace (workspace.WorkspaceOf), so a clone laid out
// anywhere else is one no row could drive a launcher over.
const (
	lockCloneOwner = "acme"
	lockCloneRepo  = "widgets"
	lockCloneIssue = "infra-1"
)

// lockClone is one issue pod's clone and the workspace in it, as the sweep reads them: the shared
// clone's git directory, that workspace's worktree entry under it, and the two jj files jj
// resolves a git directory through (.jj/repo and the repository's store/git_target), which a real
// clone holds and the sweep never reads.
type lockClone struct {
	t *testing.T
	// root is the state directory the clone and the workspace are laid out under
	// (workspace.Location). dir is the jj workspace, clone the shared clone, gitDir the clone's
	// git directory and worktree the workspace's own entry under gitDir/worktrees. repo is the
	// jj repository .jj/repo names.
	root, dir, clone, gitDir, worktree, repo string
}

func newLockClone(t *testing.T) *lockClone {
	t.Helper()
	return newLockCloneUnder(t, t.TempDir())
}

// newLockCloneUnder is that clone under a state directory the row names, for the one row whose
// holder runs in a role's own mount namespace: role-init mounts a private tmpfs over /tmp there,
// which would hide a clone kept under it from the very process the row has hold its lock open,
// so that row builds its clone under namespaceScratch instead.
func newLockCloneUnder(t *testing.T, root string) *lockClone {
	t.Helper()
	// The layout comes from the call that lays it out, which is the one the sweep's derivation
	// inverts (workspace.Location, workspace.WorkspaceOf), so no fixture here can drift from it.
	located, err := workspace.Location(root, ghrepo.MustParse(lockCloneOwner+"/"+lockCloneRepo), lockCloneIssue)
	if err != nil {
		t.Fatal(err)
	}
	c := &lockClone{t: t, root: root, dir: located.Dir, clone: located.Clone}
	c.gitDir = filepath.Join(c.clone, ".git")
	// git names a worktree entry after the directory's base name, which in a pod is the issue's
	// key, since that is what Location names the workspace directory.
	c.worktree = filepath.Join(c.gitDir, "worktrees", lockCloneIssue)
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

// sweep is a sweep over this clone with the launcher's own removal and clock — the walk is the
// launcher's own wherever the sweep runs, since it calls it outright — and a /proc root of the
// row's own. The clone is derived here through the call the launcher derives it with, once,
// before any role process exists (workspace.WorkspaceOf, gitLockSweep.ws), so every row of this
// package drives a sweep over a workspace that derivation accepted. The numbers are the
// package's, which a row changes on the sweep it was given rather than on the package, since
// nothing but its own sweep reads them here.
func (c *lockClone) sweep(log *lockedBuffer, proc string) *gitLockSweep {
	c.t.Helper()
	ws, err := workspace.WorkspaceOf(c.dir)
	if err != nil {
		c.t.Fatal(err)
	}
	return &gitLockSweep{
		proc: proc, guard: gitLockGuard, interval: gitLockInterval, recheck: time.Millisecond,
		remove: os.Remove, now: time.Now,
		log: (&roleLog{stdout: log, stderr: log}).linef, ws: ws,
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

// blindProc is a /proc root holding one process whose open files cannot be read, which is the one
// case the holder read cannot decide. Its entry is a directory the row wrote rather than a
// symlink into the container's own /proc, and the fd there is a plain file, which ReadDir refuses
// with ENOTDIR whatever uid reads it: a directory with its permissions taken away would be read
// by the root a pod's launcher runs as, and the row would prove nothing in a pod. comm is what
// the row wants that process's name read as, the line a kernel writes there.
func blindProc(t *testing.T, comm string) string {
	t.Helper()
	root := t.TempDir()
	process := filepath.Join(root, "7")
	if err := os.MkdirAll(process, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"fd": "no directory of open files\n", "comm": comm + "\n"} {
		if err := os.WriteFile(filepath.Join(process, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// gatedProc is a /proc root a row steps the sweep's two holder reads through. Beside one symlink
// per process the row cares about, it holds two processes of its own the walk stops at: the fd of
// each is a plain file, so the read of its open files fails and the walk goes on to read its comm
// to decide whether it is a git it must take for a holder of every lock, and that comm is a fifo,
// whose open blocks until the row opens it for writing. A directory read is what cannot be
// blocked that way — Go opens a directory with O_DIRECTORY, which answers ENOTDIR for a fifo
// rather than waiting for a writer — and the comm read, which is an ordinary read of a file, is
// what can. What the row hands that read is nothing at all, a name that is no git and no jj, so a
// gate keeps no lock of its own.
//
// One gate is walked before every process the row named and one after, which is how a row acts
// between the pass's two reads with no sleep of its own and no race against the recheck: the
// second gate's rendezvous says the first read has seen every process, and the first gate holds
// the second read before it sees them again, so whatever the row does in between is what the
// second read finds and the first could not have.
type gatedProc struct {
	dir string
	// gates are the two comm fifos, in the order one holder read opens them.
	gates []string
}

func newGatedProc(t *testing.T, pids ...int) *gatedProc {
	t.Helper()
	g := &gatedProc{dir: t.TempDir()}
	for _, name := range []string{"1", "9"} {
		process := filepath.Join(g.dir, name)
		if err := os.MkdirAll(process, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(process, "fd"), []byte("no directory of open files\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		comm := filepath.Join(process, "comm")
		if err := syscall.Mkfifo(comm, 0o600); err != nil {
			t.Fatal(err)
		}
		g.gates = append(g.gates, comm)
	}
	// A row that fails while a goroutine is parked on a gate would leave it parked for the rest
	// of the run: through's own timeout calls t.Fatalf without unblocking the writer parked in
	// its open, and the pass goroutine is parked on the other gate's read. Opening a fifo
	// read-only and non-blocking returns at once, and the reader it gives a parked writer ends
	// that writer's open, whose own writer then ends a parked reader's: both return.
	t.Cleanup(func() {
		for _, gate := range g.gates {
			if file, err := os.OpenFile(gate, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = file.Close()
			}
		}
	})
	// A directory read answers its entries sorted by name, so a process named 2, 3 … is walked
	// between the two gates whatever its pid is: the name is the walk's order, and the symlink's
	// target is the process the row means.
	for index, pid := range pids {
		name := strconv.Itoa(2 + index)
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(g.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// through lets one of the sweep's holder reads past both gates and answers once it has read every
// process between them. The row calls it from its own goroutine, with the pass running in
// another; which names the read in the failure of a pass that never reached a gate.
func (g *gatedProc) through(t *testing.T, which string) {
	t.Helper()
	for _, gate := range g.gates {
		opened := make(chan error, 1)
		go func() {
			file, err := os.OpenFile(gate, os.O_WRONLY, 0)
			if err == nil {
				err = file.Close()
			}
			opened <- err
		}()
		select {
		case err := <-opened:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
			t.Fatalf("the sweep's %s holder read never reached %s", which, gate)
		}
	}
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

// requireKept fails the row unless path is still where it was and the pass wrote its keep line
// with why after the colon. The line's age is read from the clock these rows let run, so the two
// halves that say which lock and which condition are matched rather than the whole line, which
// TestTheSweepLogsEachLockItKeepsWithTheConditionThatKeptIt pins from a clock of its own.
func requireKept(t *testing.T, log *lockedBuffer, path, why string) {
	t.Helper()
	if swept(path) {
		t.Fatalf("the sweep removed %s, which it had to keep (%s); its log:\n%s", path, why, log.String())
	}
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, "kept the git lock "+strconv.Quote(path)+",") && strings.HasSuffix(line, ": "+why) {
			return
		}
	}
	t.Fatalf("the log holds no line keeping %s for %q; it holds:\n%s", path, why, log.String())
}

// requireRemoved fails the row unless path is gone and the pass wrote its removal line.
func requireRemoved(t *testing.T, log *lockedBuffer, path string) {
	t.Helper()
	requireSwept(t, log, path)
	if !strings.Contains(log.String(), "removed the git lock "+strconv.Quote(path)+",") {
		t.Fatalf("the sweep removed %s and wrote no line for it:\n%s", path, log.String())
	}
}

// awaitPass fails the row unless the pass running in a goroutine of its own has returned within
// d, with the sweep's own log for what it was doing.
func awaitPass(t *testing.T, done <-chan struct{}, d time.Duration, log *lockedBuffer) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testwait.Bounded(t, d, time.Second)):
		t.Fatalf("the pass did not return within %s; its log:\n%s", d, log.String())
	}
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

// roleNamespaceHolder starts one role generation the way an issue pod's launcher starts every
// role — role-init, this test binary re-entered (TestRoleInitHelper), cloned into the role's own
// user, mount, PID and IPC namespaces, which starts its shim inside them — over a layout of its
// own under dir, with a shim that opens lock and holds it open for the row. It answers that
// shim's pid in the container's own /proc, which is the process table the sweep walks.
//
// The shim cannot name that pid itself: the /proc it sees is the role's, which numbers the role's
// processes from its own init, so the pid is read here, outside the namespace, as the one child
// role-init has.
func roleNamespaceHolder(t *testing.T, dir, lock string) int {
	t.Helper()
	cfg := RoleInitConfig{
		RolesDir: filepath.Join(dir, "roles"), Role: "tester", Generation: 1,
		Home: filepath.Join(dir, "home"), HomeMounts: filepath.Join(dir, "home-mounts"),
		FixedRoot: filepath.Join(dir, "fixed"),
		// The shell's own open of the lock, which its exec of sleep keeps: one process of the
		// role's namespaces holds the lock open, and it says so once it does.
		Argv: []string{"/bin/sh", "-c", "exec 9<'" + lock + "'; echo holding; exec sleep 300"},
	}
	// The layout role-init binds back at the fixed paths, as the probe's is built
	// (CheckRoleNamespaces): this role's own directories and nothing of any other role.
	dirs := []string{cfg.Home, cfg.HomeMounts}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir} {
		dirs = append(dirs, filepath.Join(cfg.RolesDir, cfg.Role, name), filepath.Join(cfg.FixedRoot, name))
	}
	for _, path := range dirs {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	useRoleLayout(t, cfg.Home, cfg.HomeMounts, cfg.FixedRoot)
	out := &lockedBuffer{}
	process, err := newRoleProcess(cfg, os.Environ(), out, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The kernel ends a PID namespace with its init, so this takes the shim with it.
		_ = process.cmd.Process.Kill()
		_ = process.cmd.Wait()
		_ = process.status.Close()
	})
	var holder int
	testwait.Eventually(t, "the role's shim to open "+lock, func() bool {
		if !strings.Contains(out.String(), "holding") {
			return false
		}
		holder = childOf(t, process.cmd.Process.Pid)
		return holder != 0
	})
	return holder
}

// childOf is the pid of parent's one child in the container's own /proc, and 0 while it has none:
// every process's status names its parent's pid there, which is how a process of a role's
// namespaces is found from outside them.
func childOf(t *testing.T, parent int) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			// Gone between the read of the process table and the read of its status.
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if rest, ok := strings.CutPrefix(line, "PPid:"); ok && strings.TrimSpace(rest) == strconv.Itoa(parent) {
				return pid
			}
		}
	}
	return 0
}

// The launcher's first pass removes every lock of the clone that no process holds open, whatever
// its age: it runs before the launcher serves any role and before it starts the pod's test
// database, so no process of the container can hold one, and a lock written a second before the
// pod started is as stale as one written a day before. The locks of the clone's own git directory
// and of the workspace's worktree entry go alike.
func TestTheFirstPassRemovesEveryLockAtLauncherStart(t *testing.T) {
	// A lock's path is a name a role of the pod wrote, and a Linux file name may hold a newline.
	// The line carries it quoted (gitLockSweep.line), so no role can split a line of PID 1's
	// log, which carries no role prefix, and have its second half read as another role's own
	// output. This row stands before the rest of this one, on a clone of its own: the rows below
	// read a path quoted as well, so a line that stopped quoting would fail them first and this
	// row, which is the one that proves what the quoting is for, would never run.
	t.Run("a lock whose name holds a newline writes one line", func(t *testing.T) {
		split := newLockClone(t)
		planted := split.lock("refs/heads/x\ntester: y.lock", "a role's", time.Second)
		log := &lockedBuffer{}
		s := split.sweep(log, procRoot(t, os.Getpid()))
		s.pass(context.Background(), true)
		requireSwept(t, log, planted)
		for _, line := range strings.Split(log.String(), "\n") {
			if strings.HasPrefix(line, "tester:") {
				t.Fatalf("the lock's name split the sweep's line and %q reads as the tester's own output:\n%s", line, log.String())
			}
		}
	})

	c := newLockClone(t)
	index := c.lock("index.lock", "one", time.Second)
	ref := c.lock("refs/heads/legion.lock", "two", time.Second)
	working := c.worktreeLock("index.lock", "three", time.Second)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), true)
	for _, path := range []string{index, ref, working} {
		requireSwept(t, log, path)
		if !strings.Contains(log.String(), "removed the git lock "+strconv.Quote(path)+",") {
			t.Fatalf("the first pass removed %s and wrote no line for it:\n%s", path, log.String())
		}
	}

	// The same locks under the clock's rule: a pass that runs once the roles are working keeps
	// each of them, because the guard is what a lock a live writer took looks like.
	t.Run("a clock pass keeps what the first pass removed unasked", func(t *testing.T) {
		again := c.lock("index.lock", "one", time.Second)
		clock := &lockedBuffer{}
		s := c.sweep(clock, procRoot(t, os.Getpid()))
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
	if !strings.Contains(log.String(), "kept the git lock "+strconv.Quote(held)+",") ||
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
		if !strings.Contains(moved.String(), "kept the git lock "+strconv.Quote(renamed)+",") {
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
		// The pass is held where it removes the lock: the launcher builds its sweep with the
		// removal gitLockRemove answers (newGitLockSweep), and the one this row sets there waits
		// for the row before it removes anything. No read of the clone can hold a pass — a
		// gitdir that is a fifo is an entry the walk skips without waiting for a writer
		// (internal/workspace's readGitdir) — so the removal, which the pass reaches only once
		// it has derived the clone (workspace.WorkspaceOf), listed its locks
		// (workspace.GitLocksOf) and read /proc, is where it is held. The launcher learns the
		// workspace from the start below, so nothing removes before that start arrives.
		reached := make(chan string, 1)
		release := make(chan struct{})
		previous := gitLockRemove
		gitLockRemove = func(path string) error {
			select {
			case reached <- path:
			default:
			}
			<-release
			return os.Remove(path)
		}
		// Restored after the launcher's own cleanup, which waits for Run to return, since this
		// is registered before newFleet registers that one.
		t.Cleanup(func() { gitLockRemove = previous })
		// A row that fails while the pass is held lets it go before the launcher's cleanup
		// waits for Run: a deferred call runs before any cleanup of the row.
		released := false
		free := func() {
			if !released {
				released = true
				close(release)
			}
		}
		defer free()
		t.Chdir(t.TempDir())
		f := newFleet(t, 5*time.Second, "tester")
		conn := f.role("tester")
		conn.send(roleStart(1, c.dir, nil))
		select {
		case path := <-reached:
			if path != lock {
				t.Fatalf("the pass held at the removal of %s, not of %s; the launcher's log:\n%s", path, lock, f.stderr.String())
			}
		case <-time.After(testwait.Bounded(t, 30*time.Second, time.Second)):
			t.Fatalf("the launcher's pass never reached the removal of %s; its log:\n%s", lock, f.stderr.String())
		}
		answersNothingFor(t, conn, 300*time.Millisecond)
		if swept(lock) {
			t.Fatal("the pass removed the lock while it was still held at its removal")
		}

		free()
		if got, ok := conn.reply().(shimwire.LauncherStartResult); !ok || !got.OK {
			t.Fatalf("the start answered %#v once the pass returned; the launcher's log:\n%s", got, f.stderr.String())
		}
		if !swept(lock) {
			t.Fatalf("the start was answered with %s still there; the launcher's log:\n%s", lock, f.stderr.String())
		}
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
		if !strings.Contains(f.stderr.String(), "removed the git lock "+strconv.Quote(path)+",") {
			t.Fatalf("the launcher removed %s and wrote no line for it:\n%s", path, f.stderr.String())
		}
	}
}

// A launcher whose working directory is no workspace the sweep can derive a clone for — here a
// directory the runtime's layout does not account for, which is every directory outside
// <state>/workspaces/<owner>/<name>/<issue> — says so once and sweeps nothing, since it has no
// clone to sweep. The first role start it serves names the workspace (LEGION_WORKSPACE), and
// that is the clone it sweeps, under the first pass's rule, before the start's own restore runs.
func TestALauncherWhoseDirectoryIsNoWorkspaceSweepsAtTheFirstRoleStart(t *testing.T) {
	requireRoleNamespaces(t)
	c := newLockClone(t)
	lock := c.lock("index.lock", "stale", time.Second)
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	f := newFleet(t, 5*time.Second, "tester")
	if !strings.Contains(f.stderr.String(), "working directory "+strconv.Quote(elsewhere)+" is no jj workspace") {
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
	if !strings.Contains(f.stderr.String(), "the git lock sweep works the workspace "+strconv.Quote(c.dir)) {
		t.Fatalf("the launcher swept the start's workspace and said:\n%s", f.stderr.String())
	}
	if !strings.Contains(f.stderr.String(), "removed the git lock "+strconv.Quote(lock)+",") {
		t.Fatalf("the launcher removed %s and wrote no line for it:\n%s", lock, f.stderr.String())
	}
}

// A derivation that failed is the sweep's answer for the pod's life. learn records that it has
// taken a workspace before it derives, so a start that arrives later finds one already taken and
// derives nothing: were the two the other way round, that later start's learn would run
// sweep(ctx, true), the first pass's rule — every unheld lock goes, whatever its age and with no
// second read — over a clone the pod's roles are already working, and that rule holds only while
// no process of the container can hold a lock.
//
// It drives the sweep's own learn and needs no launcher and no role namespaces of its own.
func TestALearnWhoseDerivationFailedLearnsNothingFromALaterStart(t *testing.T) {
	broken := newLockClone(t)
	stale := broken.lock("index.lock", "stale", time.Hour)
	// The one thing a derived git directory is checked for is a worktree entry naming this
	// workspace, so removing that entry's gitdir leaves a clone where the layout says it is
	// with nothing in it this workspace owns.
	gitdir := filepath.Join(broken.worktree, "gitdir")
	if err := os.Remove(gitdir); err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	s := &gitLockSweep{
		proc: procRoot(t, os.Getpid()), guard: gitLockGuard, interval: gitLockInterval,
		recheck: time.Millisecond, remove: os.Remove, now: time.Now,
		log: (&roleLog{stdout: log, stderr: log}).linef,
	}
	s.learn(context.Background(), broken.dir)
	refusal := fmt.Sprintf("the git directory %s of the clone of %s serves no git worktree of it", broken.gitDir, broken.dir)
	if !strings.Contains(log.String(), refusal) {
		t.Fatalf("the learn that could derive no git directory said:\n%s", log.String())
	}
	if swept(stale) {
		t.Fatalf("a sweep that derived no git directory removed %s:\n%s", stale, log.String())
	}

	// The entry is back, as a role of the pod that repaired the clone would leave it, and a
	// second start names the same workspace: the sweep derives nothing and sweeps nothing.
	broken.write(gitdir, filepath.Join(broken.dir, ".git")+"\n")
	s.learn(context.Background(), broken.dir)
	if s.ws.Dir != "" {
		t.Fatalf("a second start gave the sweep the workspace %q, which it derived with a role of the pod running", s.ws.Dir)
	}
	if swept(stale) {
		t.Fatalf("a second start made the sweep remove %s:\n%s", stale, log.String())
	}
	if strings.Contains(log.String(), "the git lock sweep works the workspace") {
		t.Fatalf("a second start made the sweep take a workspace it had already taken:\n%s", log.String())
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
	// The wait is for the line and not for the file: the pass removes the lock and writes its
	// line after it (gitLockSweep.removed), so a wait on the file alone reads the log of a pass
	// that has not written yet.
	for _, name := range []string{"index.lock", "refs/heads/one.lock", "refs/heads/two.lock"} {
		path := c.lock(name, "stale", time.Hour)
		line := "removed the git lock " + strconv.Quote(path) + ","
		testwait.Eventually(t, "the clock pass that removes "+path, func() bool {
			return strings.Contains(log.String(), line)
		})
		if !swept(path) {
			t.Fatalf("the clock pass wrote its line for %s and left the file where it was:\n%s", path, log.String())
		}
	}

	// The refusal rule is for that same life: a pass that listed the clone's locks and then
	// could not read the container's processes refuses, and the next pass, meeting the same
	// thing, writes nothing — a line a minute for the pod's life would bury every line its roles
	// write. A pass that got as far as the locks is no pass that made every read it meant to
	// (gitLockSweep.sweep).
	t.Run("a refusal after the locks were listed is written once", func(t *testing.T) {
		blind := newLockClone(t)
		stale := blind.lock("index.lock", "stale", time.Hour)
		log := &lockedBuffer{}
		s := blind.sweep(log, filepath.Join(t.TempDir(), "no-proc"))
		s.pass(context.Background(), false)
		s.pass(context.Background(), false)
		if swept(stale) {
			t.Fatalf("a pass that could not read the container's processes removed %s:\n%s", stale, log.String())
		}
		if count := strings.Count(log.String(), "read the container's processes"); count != 1 {
			t.Fatalf("two passes wrote the refusal %d times:\n%s", count, log.String())
		}
	})
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
	requireLine(t, log, fmt.Sprintf("launcher: removed the git lock %q, %d bytes, last written %s (%s ago)",
		path, len(content), "2026-03-04T05:06:07Z", "1h30m0s"))
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
		requireLine(t, log, fmt.Sprintf("launcher: kept the git lock %q, %d bytes, last written %s (%s ago): %s",
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
		requireLine(t, log, fmt.Sprintf("launcher: kept the git lock %q, %d bytes, last written %s (%s ago): %s",
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

// A clock pass removes a lock no process holds open and nothing has written since the guard, in
// the clone's own git directory and in the issue workspace's worktree entry alike: the entry
// holds this workspace's HEAD, index and refs, which its git locks as it locks the clone's own
// files, so a pass that swept one and not the other would leave half the clone wedged. This is
// the pass the pod runs for its whole life, where the first pass's rule — no process of the
// container can hold a lock yet — no longer holds.
func TestAClockPassRemovesAnUnheldLockOfTheCloneAndTheWorktreeEntry(t *testing.T) {
	c := newLockClone(t)
	index := c.lock("index.lock", "one", time.Hour)
	ref := c.lock("refs/heads/legion.lock", "two", time.Hour)
	working := c.worktreeLock("index.lock", "three", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), false)
	for _, path := range []string{index, ref, working} {
		requireRemoved(t, log, path)
	}
}

// The holder read decides a lock at either of a clock pass's two reads: a process that holds it
// open at the first, and a writer that takes it between the two, which is the second half of the
// third condition and the reason a pass reads twice at all. Two writers replacing one file is
// what either read prevents.
func TestAClockPassKeepsALockAProcessHoldsOpen(t *testing.T) {
	t.Run("a process holds it open at the first read", func(t *testing.T) {
		c := newLockClone(t)
		held := c.lock("index.lock", "held", time.Hour)
		file, err := os.OpenFile(held, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		log := &lockedBuffer{}
		s := c.sweep(log, procRoot(t, os.Getpid()))
		// An hour's recheck this pass must never reach: a lock the first read found a holder
		// for is decided there and never read again, where a lock that reaches the second read
		// would hold the pass for that hour. A row that gave the sweep its usual millisecond
		// would read the same keep line whichever of the two reads wrote it.
		s.recheck = time.Hour
		done := make(chan struct{})
		go func() { defer close(done); s.pass(context.Background(), false) }()
		awaitPass(t, done, 10*time.Second, log)
		requireKept(t, log, held, "a process of the container holds it open")
	})

	t.Run("a writer takes it between the two reads", func(t *testing.T) {
		c := newLockClone(t)
		taken := c.lock("index.lock", "free at the first read", time.Hour)
		log := &lockedBuffer{}
		proc := newGatedProc(t, os.Getpid())
		s := c.sweep(log, proc.dir)
		done := make(chan struct{})
		go func() { defer close(done); s.pass(context.Background(), false) }()
		proc.through(t, "first")
		// The first read found no holder and the lock is older than the guard, so the second
		// read is the one condition left between this writer and a removal under its hands.
		file, err := os.OpenFile(taken, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		proc.through(t, "second")
		awaitPass(t, done, 30*time.Second, log)
		requireKept(t, log, taken, "a process of the container holds it open")
	})
}

// A role's git runs in the role's own user, mount and PID namespaces, where the launcher's sweep
// does not: the descriptor it holds is compared by device and inode, which the sweep reads
// through the container's own /proc, so a lock a role is writing is kept whatever namespace its
// writer is in. A comparison by the path the descriptor's link prints would be a comparison of
// two different mount namespaces' names for one file.
func TestAClockPassKeepsALockAGitOfARoleNamespaceHoldsOpen(t *testing.T) {
	requireRoleNamespaces(t)
	scratch := namespaceScratch(t)
	c := newLockCloneUnder(t, filepath.Join(scratch, "issue"))
	held := c.lock("index.lock", "a role's git is writing this", time.Hour)
	free := c.lock("refs/heads/legion.lock", "nobody's", time.Hour)
	holder := roleNamespaceHolder(t, filepath.Join(scratch, "role"), held)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, holder, os.Getpid()))
	s.pass(context.Background(), false)
	requireKept(t, log, held, "a process of the container holds it open")
	// The other lock of the same clone goes: the holder read answered about the one descriptor
	// the role's process holds and not about the clone.
	requireRemoved(t, log, free)
}

// A process whose open files cannot be read is the one case the holder read cannot decide, and a
// git or jj is the kind of process that takes these locks: it counts as a holder of every lock of
// the read, so the pass keeps them all. The alternative is a pass that removes a lock the one
// process it could not read was writing.
func TestAClockPassKeepsALockWhoseGitHolderItCannotRead(t *testing.T) {
	for _, comm := range []string{"git", "jj"} {
		t.Run("a "+comm+" whose open files cannot be read", func(t *testing.T) {
			c := newLockClone(t)
			index := c.lock("index.lock", "stale", time.Hour)
			worktree := c.worktreeLock("HEAD.lock", "stale", time.Hour)
			log := &lockedBuffer{}
			s := c.sweep(log, blindProc(t, comm))
			s.pass(context.Background(), false)
			for _, path := range []string{index, worktree} {
				requireKept(t, log, path, "a git or jj whose open files cannot be read is running")
			}
		})
	}
}

// Any other process whose open files cannot be read is passed over, and the lock goes: a pass
// that took every unreadable process for a holder would keep every lock of a container running
// anything the sweep cannot read into, and the wedged clone it exists to clear would stay wedged.
func TestAClockPassRemovesALockWhoseUnreadableProcessIsNoGitOrJJ(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("index.lock", "stale", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, blindProc(t, "bash"))
	s.pass(context.Background(), false)
	requireRemoved(t, log, stale)

	// comm is matched whole, so nothing a git ships beside itself is a holder of this clone's
	// locks: a git-remote-https speaks to a server and takes no lock here. The kernel cuts comm
	// at fifteen characters, which is the second spelling, and neither of them is `git`.
	for _, comm := range []string{"git-remote-https", "git-remote-http"} {
		t.Run("a "+comm+" is no git", func(t *testing.T) {
			c := newLockClone(t)
			stale := c.lock("index.lock", "stale", time.Hour)
			log := &lockedBuffer{}
			s := c.sweep(log, blindProc(t, comm))
			s.pass(context.Background(), false)
			requireRemoved(t, log, stale)
		})
	}
}

// A lock younger than the guard is kept whatever /proc said, because the length of git's
// close-to-rename window is not measured here: a writer that took its lock a moment ago may hold
// no descriptor the walk can see yet, and a pass with no age bound would remove the lock of a
// write that is happening. The line names that condition with the sweep's own guard in it.
func TestAClockPassKeepsALockYoungerThanTheGuard(t *testing.T) {
	c := newLockClone(t)
	young := c.lock("index.lock", "a live writer's", 5*time.Minute)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.guard = time.Hour
	s.pass(context.Background(), false)
	requireKept(t, log, young, "it is younger than the 1h0m0s guard")

	// The same clone, the same empty answer from /proc, and a lock the other side of the same
	// guard: its age is the whole of the difference.
	t.Run("the same lock older than the guard goes", func(t *testing.T) {
		old := c.lock("refs/heads/legion.lock", "a killed writer's", 2*time.Hour)
		log := &lockedBuffer{}
		s := c.sweep(log, procRoot(t, os.Getpid()))
		s.guard = time.Hour
		s.pass(context.Background(), false)
		requireRemoved(t, log, old)
	})
}

// The holder read and the age read are two instants of a clone that is being written, so a lock
// the pass is about to remove is read a second time and kept unless it is the same file, last
// written at the same moment: a writer that took it between the two reads leaves a file whose
// mtime moved, or another file altogether at that path, and neither is the lock the pass decided
// to remove.
func TestAClockPassKeepsALockThatChangedBetweenTheTwoReads(t *testing.T) {
	t.Run("its last write moved", func(t *testing.T) {
		c := newLockClone(t)
		path := c.lock("index.lock", "stale at the first read", time.Hour)
		log := &lockedBuffer{}
		proc := newGatedProc(t, os.Getpid())
		s := c.sweep(log, proc.dir)
		done := make(chan struct{})
		go func() { defer close(done); s.pass(context.Background(), false) }()
		proc.through(t, "first")
		// A writer that wrote into the lock between the reads, and holds no descriptor the walk
		// can see: its mtime is the only thing the pass has left to read it by.
		written := time.Now().Add(-30 * time.Minute)
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatal(err)
		}
		proc.through(t, "second")
		awaitPass(t, done, 30*time.Second, log)
		requireKept(t, log, path, "it changed between the two reads")
	})

	t.Run("another file took its place", func(t *testing.T) {
		c := newLockClone(t)
		path := c.lock("index.lock", "stale at the first read", time.Hour)
		log := &lockedBuffer{}
		proc := newGatedProc(t, os.Getpid())
		s := c.sweep(log, proc.dir)
		done := make(chan struct{})
		go func() { defer close(done); s.pass(context.Background(), false) }()
		proc.through(t, "first")
		// The lock git takes, finishes with and takes again: the path is the same and the file
		// is another, which the second read tells by device and inode and not by the path.
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		c.plant(path, "a second writer's", time.Hour)
		proc.through(t, "second")
		awaitPass(t, done, 30*time.Second, log)
		requireKept(t, log, path, "it changed between the two reads")
	})
}

// The sweep waits for no quiet container: a git or jj of the issue that holds no descriptor of
// the lock keeps nothing, since the roles of a pod run git and jj all day and a pass that stood
// down while one was running would never run at all.
func TestAClockPassRemovesALockWhileAnotherGitRunsInTheContainer(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("index.lock", "stale", time.Hour)
	// A real git of the container, running and holding no descriptor of the clone: `git
	// hash-object --stdin` reads its standard input to the end, so it runs for as long as this
	// row holds the pipe open, and it needs no repository of its own.
	git := exec.Command("git", "hash-object", "--stdin")
	stdin, err := git.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := git.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = git.Wait()
	})
	// What the walk reads that process as, so a row whose git had already exited would fail here
	// rather than pass for the wrong reason.
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(git.Process.Pid), "comm"))
	if err != nil || strings.TrimSpace(string(comm)) != "git" {
		t.Fatalf("the row's own git reads its comm as %q (%v)", comm, err)
	}
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, git.Process.Pid, os.Getpid()))
	s.pass(context.Background(), false)
	requireRemoved(t, log, stale)
}

// The sweep answers for one workspace's locks and leaves every other file of the volume where it
// is: another workspace of the same clone has a pod of its own deciding its locks, and the pod's
// fetch feed is a bare repository whose only writer is the fetch that starts a pod
// (workspace.RemoveStaleFeedLocks). A sweep that walked either would remove a lock a live writer
// outside this container holds.
func TestTheSweepLeavesAnotherWorkspacesWorktreeEntryAndTheFeedAlone(t *testing.T) {
	c := newLockClone(t)
	ours := c.worktreeLock("index.lock", "ours", time.Hour)
	// A second workspace's entry under the same clone: git names an entry after the directory's
	// base name, which is the issue's key, and its gitdir names that workspace's own .git, which
	// is how the walk tells one workspace's locks from another's.
	other := filepath.Join(c.gitDir, "worktrees", "infra-2")
	c.plant(filepath.Join(other, "gitdir"), filepath.Join(filepath.Dir(c.dir), "infra-2", ".git")+"\n", 0)
	theirs := c.plant(filepath.Join(other, "index.lock"), "another pod's", time.Hour)
	// The pod's feed on the same volume, laid out as workspace.FeedRepository names it.
	feed := c.plant(filepath.Join(c.root, "feed", lockCloneOwner, lockCloneRepo+".git", "refs", "heads", "main.lock"), "the feed's", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.pass(context.Background(), false)
	requireRemoved(t, log, ours)
	for _, path := range []string{theirs, feed} {
		if swept(path) {
			t.Fatalf("the sweep removed %s, which is no lock of its workspace; its log:\n%s", path, log.String())
		}
		if strings.Contains(log.String(), path) {
			t.Fatalf("the sweep named %s in its log, which it never reads:\n%s", path, log.String())
		}
	}

	// The two files jj resolves a git directory through, .jj/repo and the repository's
	// store/git_target, are files every role of the pod can write, and an absolute path in
	// either is taken as it stands. The sweep reads neither: it derives the clone from the
	// workspace's own path (workspace.WorkspaceOf, gitLockSweep.ws), so a role that writes one
	// line into store/git_target while the pod runs moves no pass of the sweep, and the
	// launcher — in a mount namespace that sees what role-init hides from each role — removes
	// nothing under the directory that line names.
	//
	// The sweep is built where the launcher builds it, from its own working directory
	// (newGitLockSweep), as the restart rows below build theirs: a row whose sweep came from
	// the fixture's own derivation would stay green with the production derivation reverted.
	t.Run("a git_target rewritten while the pod runs moves no pass", func(t *testing.T) {
		elsewhere := filepath.Join(t.TempDir(), "another-role")
		// What store/git_target holds in a colocated clone is `<clone>/.git`, so the decoy is
		// laid out as a clone, with a lock under the git directory the rewritten line names and
		// one under the directory above it: a sweep that took that line for either walks a
		// lock of this row.
		planted := []string{
			c.plant(filepath.Join(elsewhere, ".git", "refs", "heads", "main.lock"), "a sibling role's", time.Hour),
			c.plant(filepath.Join(elsewhere, "refs", "heads", "main.lock"), "a sibling role's", time.Hour),
		}
		c.write(filepath.Join(c.repo, "store", "git_target"), filepath.Join(elsewhere, ".git")+"\n")
		mine := c.lock("packed-refs.lock", "ours", time.Hour)
		t.Chdir(c.dir)
		log := &lockedBuffer{}
		s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
		s.proc = procRoot(t, os.Getpid())
		s.recheck = time.Millisecond
		s.pass(context.Background(), false)
		for _, path := range planted {
			if swept(path) {
				t.Fatalf("the sweep removed %s, which only the rewritten git_target named; its log:\n%s", path, log.String())
			}
		}
		if strings.Contains(log.String(), elsewhere) {
			t.Fatalf("the sweep named the rewritten git directory %s in its log:\n%s", elsewhere, log.String())
		}
		// The pass walked the git directory the sweep derived, and its lines still name it.
		requireRemoved(t, log, mine)
	})

	// The same rewritten file read by a launcher that starts after it was written, which is a
	// roles container restarting on the pod's own disk and not a fresh pod: the pod's restart
	// policy is Always and its disk lives as long as the pod, so a container an out-of-memory
	// kill ended comes back on a disk its own last roles wrote. The sweep such a launcher builds
	// reads store/git_target exactly as a role left it, and its first pass removes at any age,
	// so a role that pointed that file at a directory it can write would have the new launcher
	// empty that directory — a sibling role's entry of /roles among them, which role-init hides
	// from the role but not from PID 1. The derivation makes the moment of the read no part of
	// the argument: the clone comes from the workspace's path, so both the lock the role left an
	// hour ago and the one it wrote as the launcher started stay where they are.
	t.Run("a git_target a role rewrote before a roles container restart moves no new sweep", func(t *testing.T) {
		// decoyed is that disk: a clone of this row's own, and the directory the rewritten
		// git_target names, holding one old lock and one written at this moment.
		type decoyed struct {
			*lockClone
			sibling, old, fresh string
		}
		restarted := func(t *testing.T) decoyed {
			t.Helper()
			d := decoyed{lockClone: newLockClone(t)}
			d.sibling = filepath.Join(t.TempDir(), "roles", "sibling")
			d.old = d.plant(filepath.Join(d.sibling, "refs", "heads", "old.lock"), "a sibling role's", time.Hour)
			d.fresh = d.plant(filepath.Join(d.sibling, "refs", "heads", "fresh.lock"), "a sibling role's", 0)
			d.write(filepath.Join(d.repo, "store", "git_target"), d.sibling+"\n")
			return d
		}
		untouched := func(t *testing.T, d decoyed, log *lockedBuffer) {
			t.Helper()
			for _, path := range []string{d.old, d.fresh} {
				if swept(path) {
					t.Fatalf("the restarted container's first pass removed %s, which only the rewritten git_target named; its log:\n%s", path, log.String())
				}
			}
			if strings.Contains(log.String(), d.sibling) {
				t.Fatalf("the sweep named the rewritten git directory %s in its log:\n%s", d.sibling, log.String())
			}
		}

		// Both ways a launcher reaches a workspace, since each derives the git directory for
		// itself. The /proc of each is the row's own, as every other row's is; what the row
		// drives is the real constructor, which is where the derivation is made.
		t.Run("through the launcher's own working directory", func(t *testing.T) {
			d := restarted(t)
			mine := d.lock("index.lock", "ours", time.Hour)
			t.Chdir(d.dir)
			log := &lockedBuffer{}
			s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
			s.proc = procRoot(t, os.Getpid())
			s.pass(context.Background(), true)
			// The sibling directory first: a reverted derivation empties it, and that is the
			// fault this row is for, where the clone's own lock is removed either way.
			untouched(t, d, log)
			requireRemoved(t, log, mine)
		})

		t.Run("through the workspace the first role start names", func(t *testing.T) {
			d := restarted(t)
			mine := d.lock("index.lock", "ours", time.Hour)
			t.Chdir(t.TempDir())
			log := &lockedBuffer{}
			s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
			s.proc = procRoot(t, os.Getpid())
			s.learn(context.Background(), d.dir)
			untouched(t, d, log)
			requireRemoved(t, log, mine)
		})
	})
}

// A worktree entry's gitdir is a file every role of the pod can write, and both the derivation
// (workspace.WorkspaceOf) and every pass's walk (workspace.GitLocksOf) read the gitdir of every
// entry under the clone's worktrees, not only this workspace's. A role that makes another entry's
// gitdir a link to /dev/zero, or a fifo, must neither end PID 1 nor stall it: a read of /dev/zero
// to its end runs the launcher out of memory, and a read of a fifo waits for a writer for ever
// with the sweep's lock held, so a role start that learns waits behind it. Such an entry names
// no workspace and is skipped (internal/workspace's readGitdir): the pass returns, removes this
// workspace's own stale locks as ever, leaves the entry's and writes nothing of it. A gitdir the
// launcher may not open at all (a `chmod 000`) is skipped the same way rather than refusing the
// pass: one role must not be able to switch the sweep off with a sibling's entry.
//
// Each of readGitdir's guards has a row here that fails with that guard alone reverted: the
// link that names this workspace's .git (O_NOFOLLOW: followed, the sibling is taken for this
// workspace's entry and its lock removed), the fifo a writer holds open (the regular-file check:
// read, it waits for ever), and the terabyte whose first bytes name this workspace (read without
// the bound, it runs away; read to the bound and not refused there, those bytes name this
// workspace and the sibling's lock is removed).
//
// The sweep is built where the launcher builds it, from its own working directory, after the
// entry is planted: that is a roles container restarting on a volume a role planted one on,
// whose derivation reads every entry before the launcher serves anything. A clock pass follows,
// which is what a running pod reads every minute.
//
// The row bounds itself, so a read that runs away or waits fails the row and not the run: the
// derivation and the pass run in a goroutine the row waits for under a deadline, a read of
// /dev/zero, or of the gitdir past a mebibyte, is ended by endRunawayReads, which the row fails
// on, and a reader the fifo holds is let go once the row has failed, so nothing of the row is
// left running.
func TestTheSweepSkipsAWorktreeEntryWhoseGitdirIsNoFileGitWrote(t *testing.T) {
	// writer is the descriptor the held fifo's row keeps open on it, which its unstick closes.
	var writer *os.File
	for _, row := range []struct {
		name string
		// plant makes gitdir what the row is about; unstick lets go a read of it that is
		// waiting, for a row that has already failed.
		plant   func(t *testing.T, c *lockClone, gitdir string)
		unstick func(gitdir string)
	}{
		{
			name: "a gitdir that links to /dev/zero",
			plant: func(t *testing.T, _ *lockClone, gitdir string) {
				if err := os.Symlink("/dev/zero", gitdir); err != nil {
					t.Fatal(err)
				}
			},
			unstick: func(string) {},
		},
		{
			name: "a gitdir that is a fifo",
			plant: func(t *testing.T, _ *lockClone, gitdir string) {
				if err := syscall.Mkfifo(gitdir, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			// A writer opened without blocking is refused while no reader waits and lets a
			// waiting reader's open return, whose read then ends at once with no content.
			unstick: func(gitdir string) {
				if file, err := os.OpenFile(gitdir, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = file.Close()
				}
			},
		},
		{
			// A fifo with a writer that writes nothing: a read of it waits for the writer, so
			// only the regular-file check keeps the walk from reading it.
			name: "a gitdir that is a fifo a writer holds open",
			plant: func(t *testing.T, _ *lockClone, gitdir string) {
				if err := syscall.Mkfifo(gitdir, 0o600); err != nil {
					t.Fatal(err)
				}
				// Read-write, so the open needs no reader to be waiting.
				file, err := os.OpenFile(gitdir, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				writer = file
				t.Cleanup(func() { _ = file.Close() })
			},
			// With its last writer gone, a waiting read ends at once with no content.
			unstick: func(string) { _ = writer.Close() },
		},
		{
			name: "a gitdir that is a directory",
			plant: func(t *testing.T, _ *lockClone, gitdir string) {
				if err := os.Mkdir(gitdir, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			unstick: func(string) {},
		},
		{
			name: "a gitdir the launcher may not open",
			plant: func(t *testing.T, c *lockClone, gitdir string) {
				c.write(gitdir, filepath.Join(filepath.Dir(c.dir), "infra-2", ".git")+"\n")
				if err := os.Chmod(gitdir, 0); err != nil {
					t.Fatal(err)
				}
				if file, err := os.Open(gitdir); err == nil {
					_ = file.Close()
					t.Fatalf("%s is readable at mode 000, so this row would prove nothing (is the test running as root?)", gitdir)
				}
			},
			unstick: func(string) {},
		},
		{
			// The link itself is no file git writes; what it names is a regular file holding
			// this workspace's .git, which a walk that followed it would take for this
			// workspace's own entry, and walk.
			name: "a gitdir that links to a file naming this workspace",
			plant: func(t *testing.T, c *lockClone, gitdir string) {
				named := filepath.Join(t.TempDir(), "gitdir")
				c.write(named, filepath.Join(c.dir, ".git")+"\n")
				if err := os.Symlink(named, gitdir); err != nil {
					t.Fatal(err)
				}
			},
			unstick: func(string) {},
		},
		{
			// A sparse regular file of a terabyte, which a regular-file check accepts and
			// O_NOFOLLOW does not touch. Its first gitdirBound bytes are this workspace's .git
			// behind enough slashes to fill them, a newline last: a read that stops at the bound
			// and takes what it read finds this workspace's entry there.
			name: "a gitdir of a terabyte whose first bytes name this workspace",
			plant: func(t *testing.T, c *lockClone, gitdir string) {
				c.write(gitdir, boundFillingGitdir(c))
				if err := os.Truncate(gitdir, 1<<40); err != nil {
					t.Fatal(err)
				}
			},
			unstick: func(string) {},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newLockClone(t)
			ours := c.lock("index.lock", "ours", time.Hour)
			entry := c.worktreeLock("HEAD.lock", "ours", time.Hour)
			sibling := filepath.Join(c.gitDir, "worktrees", "infra-2")
			theirs := c.plant(filepath.Join(sibling, "index.lock"), "nobody's", time.Hour)
			gitdir := filepath.Join(sibling, "gitdir")
			row.plant(t, c, gitdir)
			t.Chdir(c.dir)
			log := &lockedBuffer{}
			proc := procRoot(t, os.Getpid())
			stop := make(chan struct{})
			ranAway := endRunawayReads(t, stop, gitdir)
			done := make(chan struct{})
			go func() {
				defer close(done)
				s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
				s.proc, s.recheck = proc, time.Millisecond
				s.pass(context.Background(), false)
			}()
			bound := testwait.Bounded(t, 10*time.Second, time.Second)
			select {
			case <-done:
			case <-time.After(bound):
				// A read the fifo holds is let go as often as it is held — the derivation's,
				// then the pass's — so the goroutine returns with the row.
			unstuck:
				for range 1000 {
					row.unstick(gitdir)
					select {
					case <-done:
						break unstuck
					case <-time.After(10 * time.Millisecond):
					}
				}
				close(stop)
				t.Fatalf("the derivation and a clock pass did not return within %s; the sweep's log:\n%s", bound, log.String())
			}
			close(stop)
			if ended := <-ranAway; ended > 0 {
				t.Fatalf("the sweep read %s past where any gitdir ends, and %d such read(s) had to be ended at end of file; the sweep's log:\n%s", gitdir, ended, log.String())
			}
			requireRemoved(t, log, ours)
			requireRemoved(t, log, entry)
			if swept(theirs) {
				t.Fatalf("the sweep removed %s, which is no lock of its workspace; its log:\n%s", theirs, log.String())
			}
			if strings.Contains(log.String(), sibling) {
				t.Fatalf("the sweep wrote a line naming the entry it skips:\n%s", log.String())
			}
		})
	}
}

// gitdirBound is internal/workspace's gitdirLimit, which this package cannot read: the most of a
// worktree entry's gitdir the walk reads, one byte more than PATH_MAX.
const gitdirBound = 4096 + 1

// boundFillingGitdir is a gitdir of exactly gitdirBound bytes that names this workspace's .git:
// its path behind as many slashes as fill the bound, which a path's cleaning drops, and a
// newline. A walk that read it to the bound and took what it read would find this workspace's
// entry in it; the walk refuses it for reaching the bound.
func boundFillingGitdir(c *lockClone) string {
	own := filepath.Join(c.dir, ".git")
	return strings.Repeat("/", gitdirBound-1-len(own)) + own + "\n"
}

// endRunawayReads ends every read this process makes of /dev/zero, and every read of the file at
// gitdir once it has gone a mebibyte in — far past gitdirBound, where a bounded read stops —
// until stop closes, and then answers how many it ended. A descriptor of the process open on
// either is replaced, in place, with one of /dev/null, so the read that is running on it reaches
// end of file at its next call rather than filling memory until the runtime ends the process:
// what a row that would otherwise run away costs is the memory it read in a millisecond, and its
// failure. A bounded read of gitdir never reaches the mebibyte, so it is never ended.
func endRunawayReads(t *testing.T, stop <-chan struct{}, gitdir string) <-chan int {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	// A descriptor's link names the file by its real path.
	parent, err := filepath.EvalSymlinks(filepath.Dir(gitdir))
	if err != nil {
		t.Fatal(err)
	}
	gitdir = filepath.Join(parent, filepath.Base(gitdir))
	ended := make(chan int, 1)
	go func() {
		defer null.Close()
		count := 0
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				ended <- count
				return
			case <-ticker.C:
			}
			fds, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				continue
			}
			for _, fd := range fds {
				target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
				if err != nil || (target != "/dev/zero" && target != gitdir) {
					continue
				}
				number, err := strconv.Atoi(fd.Name())
				if err != nil {
					continue
				}
				if target == gitdir {
					// A fifo has no offset to read, and is never a read that runs away.
					if offset, err := unix.Seek(number, 0, unix.SEEK_CUR); err != nil || offset <= 1<<20 {
						continue
					}
				}
				if unix.Dup2(int(null.Fd()), number) == nil {
					count++
				}
			}
		}
	}()
	return ended
}

// unreadableGitdirs is each way a role can leave this workspace's own worktree entry's gitdir
// unreadable to the walk (internal/workspace's readGitdir), and the cause the sweep's line names
// for it, given the gitdir's path. Each is planted where the row's gitdir was (breakGitdir), and
// repairGitdir puts back the gitdir git wrote.
var unreadableGitdirs = []struct {
	name  string
	plant func(t *testing.T, c *lockClone, gitdir string)
	cause func(gitdir string) string
}{
	{
		name: "a fifo",
		plant: func(t *testing.T, _ *lockClone, gitdir string) {
			if err := syscall.Mkfifo(gitdir, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		cause: func(gitdir string) string { return gitdir + " is a fifo" },
	},
	{
		name: "a symlink to a file naming this workspace",
		plant: func(t *testing.T, c *lockClone, gitdir string) {
			named := filepath.Join(t.TempDir(), "gitdir")
			c.write(named, filepath.Join(c.dir, ".git")+"\n")
			if err := os.Symlink(named, gitdir); err != nil {
				t.Fatal(err)
			}
		},
		cause: func(gitdir string) string { return gitdir + " is a symlink" },
	},
	{
		name:  "a file that fills the bound",
		plant: func(_ *testing.T, c *lockClone, gitdir string) { c.write(gitdir, boundFillingGitdir(c)) },
		cause: func(gitdir string) string { return gitdir + " is larger than 4096 bytes" },
	},
	{
		name: "a directory",
		plant: func(t *testing.T, _ *lockClone, gitdir string) {
			if err := os.Mkdir(gitdir, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		cause: func(gitdir string) string { return gitdir + " is a directory" },
	},
	{
		name: "a file the launcher may not open",
		plant: func(t *testing.T, c *lockClone, gitdir string) {
			c.write(gitdir, filepath.Join(c.dir, ".git")+"\n")
			if err := os.Chmod(gitdir, 0); err != nil {
				t.Fatal(err)
			}
			if file, err := os.Open(gitdir); err == nil {
				_ = file.Close()
				t.Fatalf("%s is readable at mode 000, so this row would prove nothing (is the test running as root?)", gitdir)
			}
		},
		cause: func(gitdir string) string { return "open " + gitdir + ": permission denied" },
	},
}

// breakGitdir replaces the gitdir of this workspace's own worktree entry with plant's, and
// repairGitdir puts back the one git wrote.
func (c *lockClone) breakGitdir(t *testing.T, plant func(*testing.T, *lockClone, string)) {
	t.Helper()
	gitdir := filepath.Join(c.worktree, "gitdir")
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	plant(t, c, gitdir)
}

func (c *lockClone) repairGitdir(t *testing.T) {
	t.Helper()
	gitdir := filepath.Join(c.worktree, "gitdir")
	if err := os.RemoveAll(gitdir); err != nil {
		t.Fatal(err)
	}
	c.write(gitdir, filepath.Join(c.dir, ".git")+"\n")
}

// unreadableEntry is the reason the walk gives for this workspace's own entry when its gitdir
// cannot be read for cause, as both the derivation's refusal and a pass's line carry it.
func (c *lockClone) unreadableEntry(cause string) string {
	return fmt.Sprintf("the clone of %s: the git worktree entry %s, which %s names, cannot be read: %s",
		c.dir, c.worktree, filepath.Join(c.dir, ".git"), cause)
}

// This workspace's own worktree entry, the one its .git names, holds the workspace's index,
// HEAD and refs, and its gitdir is a file a role of the pod can make unreadable while the pod
// runs, as it can any other entry's. Passed over in silence, as a sibling's is, the entry's locks
// — an index.lock that wedges every role's git — would stay for the container's life with no
// line saying why. The pass names the entry and the cause instead, once while the entry stays
// unreadable rather than once a minute, and sweeps the rest of the clone on every pass all the
// same: the git directory's own stale lock goes each time. An entry readable again is swept
// again and forgotten, so a second break is a second line.
//
// The sweep is the launcher's own, built from its working directory while the entry is sound,
// which is a running pod whose role breaks the entry after the derivation.
func TestTheSweepNamesThisWorkspacesUnreadableWorktreeEntryOnceAndSweepsTheRest(t *testing.T) {
	for _, bad := range unreadableGitdirs {
		t.Run(bad.name, func(t *testing.T) {
			c := newLockClone(t)
			t.Chdir(c.dir)
			log := &lockedBuffer{}
			s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
			if s.ws.Dir == "" {
				t.Fatalf("the launcher derived no workspace from a sound clone; its log:\n%s", log.String())
			}
			s.proc, s.recheck = procRoot(t, os.Getpid()), time.Millisecond
			c.breakGitdir(t, bad.plant)
			line := fmt.Sprintf("launcher: the git lock sweep: %q",
				c.unreadableEntry(bad.cause(filepath.Join(c.worktree, "gitdir")))+"; the pass swept the rest of the clone and left that entry's locks where they are")
			entry := c.worktreeLock("index.lock", "ours", time.Hour)
			for range 3 {
				stale := c.lock("index.lock", "stale", time.Hour)
				s.pass(context.Background(), false)
				requireSwept(t, log, stale)
				if swept(entry) {
					t.Fatalf("the sweep removed %s, under an entry it could not read; its log:\n%s", entry, log.String())
				}
			}
			requireLine(t, log, line)
			if count := strings.Count(log.String(), line); count != 1 {
				t.Fatalf("three passes wrote the entry's line %d times, want once:\n%s", count, log.String())
			}

			t.Run("a repaired entry is swept, and a second break is a second line", func(t *testing.T) {
				c.repairGitdir(t)
				s.pass(context.Background(), false)
				requireRemoved(t, log, entry)
				if count := strings.Count(log.String(), line); count != 1 {
					t.Fatalf("the pass that read the repaired entry wrote its line again (%d in all):\n%s", count, log.String())
				}
				c.breakGitdir(t, bad.plant)
				s.pass(context.Background(), false)
				if count := strings.Count(log.String(), line); count != 2 {
					t.Fatalf("the entry broken a second time was written %d times in all, want twice:\n%s", count, log.String())
				}
			})
		})
	}
}

// The same entry unreadable before the launcher starts — a roles container restarting on a
// volume where a role broke it — is the derivation's refusal, and the refusal names the entry and
// the cause. "Serves no git worktree of it" would be false there: the entry is in place, and what
// is wrong with it is what the line says. The first role start's derivation refuses with the
// same cause.
func TestTheDerivationNamesThisWorkspacesUnreadableWorktreeEntry(t *testing.T) {
	for _, bad := range unreadableGitdirs {
		t.Run(bad.name, func(t *testing.T) {
			c := newLockClone(t)
			c.breakGitdir(t, bad.plant)
			reason := c.unreadableEntry(bad.cause(filepath.Join(c.worktree, "gitdir")))
			t.Chdir(c.dir)
			log := &lockedBuffer{}
			s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
			requireLine(t, log, fmt.Sprintf("launcher: the git lock sweep's working directory %q is no jj workspace it can sweep (%q); it takes its workspace from the first role start",
				c.dir, reason))
			if s.ws.Dir != "" {
				t.Fatalf("a launcher whose own worktree entry is unreadable gave its sweep the workspace %q; log:\n%s", s.ws.Dir, log.String())
			}
			s.learn(context.Background(), c.dir)
			requireLine(t, log, fmt.Sprintf("launcher: the git lock sweep: %q", "the workspace "+c.dir+": "+reason))
			if strings.Contains(log.String(), "serves no git worktree") {
				t.Fatalf("the refusal says the clone serves no worktree of a workspace whose entry is in place:\n%s", log.String())
			}
		})
	}
}

// A clone whose git directory is a symbolic link is refused before anything is answered, and the
// pass removes nothing: git never creates one, and a link a process of the issue planted would
// put every removal of the pass somewhere outside the clone. The refusal is the pass's one line.
//
// The link is planted after the sweep has its directory, which is the order a pod has: the
// launcher derives the clone's git directory before it serves a role, and a process of the issue
// that plants a link does it while the pod runs, so what refuses the link is the walk of each
// pass and not the one derivation.
func TestTheSweepRefusesASymlinkedGitDirectory(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("index.lock", "stale", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	elsewhere := c.gitDir + "-real"
	if err := os.Rename(c.gitDir, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, c.gitDir); err != nil {
		t.Fatal(err)
	}
	s.pass(context.Background(), false)
	refusal := fmt.Sprintf("launcher: the git lock sweep: %q", fmt.Sprintf("the clone of %s: refusing the git worktrees of %s: %s is a symlink, which git never creates",
		c.dir, c.gitDir, c.gitDir))
	requireLine(t, log, refusal)
	if swept(stale) {
		t.Fatalf("the sweep removed %s through a symlinked git directory:\n%s", stale, log.String())
	}

	// A clone the sweep cannot read is a pass a minute for the pod's whole life, and the same
	// line each time would bury every line its roles write.
	t.Run("the refusal is written once", func(t *testing.T) {
		s.pass(context.Background(), false)
		if count := strings.Count(log.String(), refusal); count != 1 {
			t.Fatalf("two passes wrote the refusal %d times:\n%s", count, log.String())
		}
	})
}

// The same for the worktrees directory, which is read as the git directory is: the entries under
// it are where the pass writes, and a link there would put its removals outside the clone just as
// surely.
func TestTheSweepRefusesASymlinkedWorktreesDirectory(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("index.lock", "stale", time.Hour)
	working := c.worktreeLock("HEAD.lock", "stale", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	worktrees := filepath.Join(c.gitDir, "worktrees")
	elsewhere := worktrees + "-real"
	if err := os.Rename(worktrees, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, worktrees); err != nil {
		t.Fatal(err)
	}
	s.pass(context.Background(), false)
	refusal := fmt.Sprintf("launcher: the git lock sweep: %q", fmt.Sprintf("the clone of %s: refusing the git worktrees of %s: %s is a symlink, which git never creates",
		c.dir, c.gitDir, worktrees))
	requireLine(t, log, refusal)
	// Neither the clone's own lock nor the entry's: the walk answers no path at all, so the pass
	// ends before it has decided anything.
	for _, path := range []string{stale, working} {
		if swept(path) {
			t.Fatalf("the sweep removed %s through a symlinked worktrees directory:\n%s", path, log.String())
		}
	}

	t.Run("the refusal is written once", func(t *testing.T) {
		s.pass(context.Background(), false)
		if count := strings.Count(log.String(), refusal); count != 1 {
			t.Fatalf("two passes wrote the refusal %d times:\n%s", count, log.String())
		}
	})
}

// A clone directory a role replaced with a symlink after the sweep derived it is refused by the
// layout guard every pass runs before it walks (workspace.GitLocksOf, refuseSymlinkedLayout),
// and the pass removes nothing.
//
// What the derivation settles is the text of a path, which no role can write; the directories
// that text names are on the issue's volume, mounted read-write in the roles container, where
// every role writes as the pod's uid. A role can therefore rename the clone away and leave a
// link to a directory of its own where it stood, and nothing in the path the launcher derived
// says so. Without the guard the walk follows that link: the worktree entries it finds under it
// are no entry of this workspace, so the entries are skipped, but the git directory itself is
// walked all the same and every unheld lock over the guard's age under it is removed, with the
// layout's path in the line rather than the directory the removal happened in.
//
// The link is planted after the sweep has its workspace, which is the order a pod has: the
// launcher derives before it serves a role, and a role of the issue plants while the pod runs.
func TestTheSweepRefusesACloneReplacedWithASymlinkAfterTheDerivation(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("refs/heads/main.lock", "stale", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	// The clone, as the role left it: the real directory under a name of the role's own, and a
	// link to it where the layout puts the clone. The lock planted above moved with it, and the
	// path below is where it now lies.
	elsewhere := c.clone + "-real"
	if err := os.Rename(c.clone, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, c.clone); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(elsewhere, ".git", "refs", "heads", "main.lock")
	if swept(planted) {
		t.Fatalf("the row's lock is not at %s, where the renamed clone holds it", planted)
	}
	s.pass(context.Background(), false)
	refusal := fmt.Sprintf("launcher: the git lock sweep: %q", fmt.Sprintf("the clone of %s: refusing workspace operation in %s: %s is a symlink, and no directory of the workspace layout below the state directory is one",
		c.dir, c.dir, c.clone))
	requireLine(t, log, refusal)
	// The lock is where it was, under the directory the link names and under the clone's own
	// path, which still reaches it through that link.
	for _, path := range []string{planted, stale} {
		if swept(path) {
			t.Fatalf("the sweep removed %s through a clone replaced with a symlink:\n%s", path, log.String())
		}
	}
	if strings.Contains(log.String(), "removed the git lock") {
		t.Fatalf("the refused pass removed a lock of the directory the link names:\n%s", log.String())
	}
	if count := strings.Count(log.String(), refusal); count != 1 {
		t.Fatalf("the pass wrote its refusal %d times:\n%s", count, log.String())
	}

	// A link a role leaves in place is a pass a minute for the rest of the pod's life, and the
	// same line each time would bury every line its roles write.
	t.Run("the refusal is written once", func(t *testing.T) {
		s.pass(context.Background(), false)
		if count := strings.Count(log.String(), refusal); count != 1 {
			t.Fatalf("two passes wrote the refusal %d times:\n%s", count, log.String())
		}
	})
}

// A clone directory that is already a symlink when the launcher starts is refused by the
// derivation itself (workspace.WorkspaceOf, which runs the layout guard before it reads
// .git/worktrees), and the sweep is left with no workspace at all.
//
// Every pass guards again (workspace.GitLocksOf), so a sweep that took such a clone would still
// remove nothing; what the derivation's own guard settles is the step before that. Without it the
// derivation reads the worktree entries through the link — the clone's .git is no link itself, so
// nothing else refuses — finds this workspace's entry there and answers the clone, and the log
// then says the sweep works a clone it would refuse on every pass, rather than the one line that
// says why it has none.
func TestTheDerivationRefusesACloneThatIsASymlinkBeforeTheSweepStarts(t *testing.T) {
	c := newLockClone(t)
	stale := c.lock("refs/heads/main.lock", "stale", time.Hour)
	elsewhere := c.clone + "-real"
	if err := os.Rename(c.clone, elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, c.clone); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(elsewhere, ".git", "refs", "heads", "main.lock")
	t.Chdir(c.dir)
	log := &lockedBuffer{}
	s := newGitLockSweep((&roleLog{stdout: log, stderr: log}).linef)
	refusal := fmt.Sprintf("launcher: the git lock sweep's working directory %q is no jj workspace it can sweep (%q); it takes its workspace from the first role start",
		c.dir, fmt.Sprintf("refusing workspace operation in %s: %s is a symlink, and no directory of the workspace layout below the state directory is one", c.dir, c.clone))
	requireLine(t, log, refusal)
	if s.ws.Dir != "" || s.fromWorkDir {
		t.Fatalf("a launcher whose clone is a symlink gave its sweep the workspace %q (from its working directory: %v); log:\n%s", s.ws.Dir, s.fromWorkDir, log.String())
	}
	if strings.Contains(log.String(), "the git lock sweep works the workspace") {
		t.Fatalf("the sweep says it works a workspace whose clone is a symlink:\n%s", log.String())
	}
	s.pass(context.Background(), false)
	for _, path := range []string{planted, stale} {
		if swept(path) {
			t.Fatalf("a sweep with no workspace removed %s:\n%s", path, log.String())
		}
	}
}

// A removal that fails is one lock's problem and no one else's: the pass keeps that lock, with
// the failure as the condition that kept it, goes on to the next lock and ends as any other pass
// does. This runs in PID 1 of the roles container, where a failure that ended the process would
// cost the issue every role it serves.
func TestTheSweepGoesOnAfterALockItCannotRemove(t *testing.T) {
	c := newLockClone(t)
	// The walk answers the clone's locks in the order it found them, which is the order a
	// directory read sorts them in, so the lock whose removal fails is the first of the two: what
	// the row proves is the pass going on to the second.
	refused := c.lock("index.lock", "one", time.Hour)
	next := c.lock("refs/heads/legion.lock", "two", time.Hour)
	log := &lockedBuffer{}
	s := c.sweep(log, procRoot(t, os.Getpid()))
	s.remove = func(path string) error {
		if path == refused {
			return &fs.PathError{Op: "remove", Path: path, Err: syscall.EROFS}
		}
		return os.Remove(path)
	}
	s.pass(context.Background(), false)
	requireKept(t, log, refused, fmt.Sprintf("it could not be removed: %q", fmt.Sprintf("remove %s: read-only file system", refused)))
	if swept(refused) {
		t.Fatalf("the sweep reported a removal that failed and the lock is gone:\n%s", log.String())
	}
	if strings.Contains(log.String(), "removed the git lock "+strconv.Quote(refused)+",") {
		t.Fatalf("the sweep wrote a removal line for a lock it could not remove:\n%s", log.String())
	}
	requireRemoved(t, log, next)

	// The next pass decides it again, which is the whole of what a failed removal costs: a lock
	// the volume refused a minute ago goes the moment it can be removed.
	t.Run("the next pass decides it again", func(t *testing.T) {
		s.remove = os.Remove
		s.pass(context.Background(), false)
		requireRemoved(t, log, refused)
	})
}

// The note a role reads when a git lock fails its command (workspace.StaleGitLockNote) tells the
// role what this sweep does, in words: the age a lock must pass before a pass removes it, how
// often a pass looks again, and the wait those two add up to. The note cannot read the two
// numbers — it lives in internal/workspace, which this package imports, so reading them from here
// would be an import cycle — so this row is what holds the text and the numbers together: a
// change of either number without a rewording of the note fails here, which is the only place it
// can fail.
func TestStaleGitLockNoteCarriesTheLaunchersSweepNumbers(t *testing.T) {
	c := newLockClone(t)
	lock := c.lock("index.lock", "a killed writer's", 31*time.Minute)
	note := workspace.StaleGitLockNote(
		fmt.Sprintf("error: cannot lock ref 'refs/heads/main': Unable to create '%s': File exists.", lock), time.Now())
	if note == "" {
		t.Fatalf("the note for the planted lock %s is empty, want the note a role reads", lock)
	}
	// The note says the guard in whole minutes and the interval as `every minute`: numbers it
	// cannot print from here, so a change of their shape is a rewording of the note, not a
	// rewriting of this row's arithmetic.
	if gitLockGuard%time.Minute != 0 || gitLockInterval != time.Minute {
		t.Fatalf("gitLockGuard is %s and gitLockInterval is %s: the note words them as whole minutes and as a minute (workspace.staleGitLockNoteText), so reword the note and this row",
			gitLockGuard, gitLockInterval)
	}
	guard := int(gitLockGuard / time.Minute)
	for _, want := range []string{
		fmt.Sprintf("more than %d minutes old", guard),
		"it looks again every minute",
		fmt.Sprintf("Run the command again in about %d minutes.", guard+int(gitLockInterval/time.Minute)),
	} {
		if !strings.Contains(note, want) {
			t.Errorf("the note is %q, want %q in it", note, want)
		}
	}
}
