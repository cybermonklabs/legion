package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The roles container's sweep of the issue clone's stale git locks. A git or jj killed or
// interrupted mid-write leaves the `<file>.lock` it took beside the file it was replacing, and
// every later write of that file — every role's, since the pod's roles share one clone — fails on
// it until someone deletes it. The pod's `workspace-init provision` removes every lock it finds
// before any container of the pod starts (workspace.RemoveStaleGitLocks), which left the locks
// made while the pod runs: nothing removed those, and the roles worked a wedged clone until the
// issue's pod was replaced.
//
// The sweep runs in the launcher process of an issue pod's roles container, where processes of
// the issue are running, so it decides each lock for itself rather than removing what it finds:
// a lock some process still holds must never be removed, since two writers would then replace one
// file.

// gitLockInterval is how often a pass runs. A minute is short against the hours a wedged clone
// would otherwise cost the issue — nothing else removes such a lock while the pod runs — and long
// against one pass, which walks the clone's git directory and the container's /proc once.
var gitLockInterval = time.Minute

// gitLockGuard is the age under which a lock is kept: a lock younger than this is taken for one
// a live writer made, whatever /proc said. It is Gitaly's, GitLab's git service, which removes a
// stale lock file after 15 minutes, because the length of git's close-to-rename window is not
// measured here: nothing in this tree knows how long the slowest write of a ref, of packed-refs
// or of the index may hold its lock on an issue's volume, and a number taken from a service that
// runs git at scale is a better guess than one invented. A test varies it through the sweep's
// own guard field rather than here.
var gitLockGuard = 15 * time.Minute

// gitLockRecheck is the wait between the two reads of a lock a clock pass is about to remove. The
// holder read and the age read are two instants of one clone that is being written: a second read
// of the same file, with no holder and the same mtime, is how a pass tells a lock nothing is
// using from one a writer took between the pass's reads. A test shortens it, because this
// package's tests run under the race detector.
var gitLockRecheck = 2 * time.Second

// gitLockFirstPassBound is how long the launcher's first pass may take. It exists for the reason
// testPGBound exists: the first pass runs before the launcher serves any role, the daemon's boot
// timeout for a pod is 120 s by default, and a pass on a filesystem that does not answer must not
// take the pod's whole boot with it. A pod whose locks were left where they are, with a line in
// the container's log, is better than a pod the daemon gives up on.
var gitLockFirstPassBound = 30 * time.Second

// gitLockSweep removes the stale git locks of one issue's clone, for the life of the roles
// container. It holds its inputs as fields rather than reading them from the launcher's
// configuration or the package's variables, so a test drives it over a clone and a /proc of its
// own, and a second tree that sweeps the same locks differently reuses it.
//
// workspace is the jj workspace the clone is swept through, which the launcher reads from its own
// working directory and otherwise learns from the first role start it serves (learn), and
// fromWorkDir says which of the two it was. proc is the container's /proc, which the holder read
// walks. guard, interval and recheck are the package's three numbers, read once by whoever builds
// the sweep. locks, gitDir, remove and now are the walk (workspace.GitLocks), the clone's git
// directory (workspace.GitDir), the removal (os.Remove) and the clock, each a field so a test can
// answer them itself. log is the launcher's own log line, roleLog.linef, which carries no role
// prefix: the sweep belongs to no role.
//
// mu serializes the passes and the learning, so the pod's roles never sweep one clone twice at
// once and a role start that arrives while the first pass runs waits for it.
type gitLockSweep struct {
	proc     string
	guard    time.Duration
	interval time.Duration
	recheck  time.Duration
	locks    func(gitDir, workspaceDir string) ([]string, error)
	gitDir   func(string) (string, error)
	remove   func(string) error
	now      func() time.Time
	log      func(format string, args ...any)

	// fromWorkDir says the launcher's own working directory gave the sweep its workspace. It is
	// settled before the launcher serves any role and never written again, so learn reads it
	// without mu, which a pass holds for its whole span.
	fromWorkDir bool

	mu        sync.Mutex
	workspace string
	// refusal is the last reason a pass was refused for, so the same one is written once rather
	// than once a minute for the pod's life. A pass that got as far as the locks forgets it, so
	// a clone that breaks again is reported again.
	refusal string
}

// newGitLockSweep is the roles container's sweep over the workspace its own working directory is:
// the pod sets that container's working directory to the issue's workspace
// (internal/runtime/sandbox's podkind, WorkingDir: l.workspace), so the launcher needs nothing
// from the daemon to know which clone is its own. A working directory that cannot be read, or
// that is no jj workspace — a missing or unreadable .jj/repo and a missing store/git_target
// alike, which is every failure of workspace.GitDir — leaves the sweep without one: it writes
// that here, once, and takes the workspace from the first role start it serves instead, which
// carries LEGION_WORKSPACE. A working directory that did supply it is recorded as having done so
// (fromWorkDir), so a role start finds there is nothing to learn without taking the sweep's lock.
//
// The three numbers are read here, in the caller's own goroutine, and never inside the ticker's:
// they are package variables a test shortens, and a goroutine still reading one after the test
// restored it is a data race the race detector fails the test for (serveTestPG reads its interval
// the same way).
func newGitLockSweep(log func(format string, args ...any)) *gitLockSweep {
	s := &gitLockSweep{
		proc: "/proc", guard: gitLockGuard, interval: gitLockInterval, recheck: gitLockRecheck,
		locks: workspace.GitLocks, gitDir: workspace.GitDir, remove: os.Remove, now: time.Now, log: log,
	}
	dir, err := os.Getwd()
	if err != nil {
		s.log("launcher: the git lock sweep cannot read the launcher's working directory (%v); it takes its workspace from the first role start", err)
		return s
	}
	if _, err := s.gitDir(dir); err != nil {
		s.log("launcher: the git lock sweep's working directory %s is no jj workspace (%v); it takes its workspace from the first role start", dir, err)
		return s
	}
	s.workspace = dir
	s.fromWorkDir = true
	return s
}

// serve runs the launcher's first pass and then one pass per interval until ctx ends, and answers
// a channel closed once its ticker goroutine has returned, so a caller that ends ctx can wait for
// it rather than leave it running (serveTestPG answers the same way).
//
// The first pass is this call's, before the launcher serves any role: its rule is that no process
// of the container can hold a lock yet, which holds only while that is true. It is bounded by
// gitLockFirstPassBound, which ends it at its next step rather than inside a syscall the kernel
// owns: a pass that hits the bound writes one line and the launcher goes on to serve its roles.
func (s *gitLockSweep) serve(ctx context.Context) <-chan struct{} {
	interval := s.interval
	bounded, cancel := context.WithTimeout(ctx, gitLockFirstPassBound)
	s.pass(bounded, true)
	ended := bounded.Err() != nil && ctx.Err() == nil
	cancel()
	if ended {
		s.log("launcher: the git lock sweep's first pass did not finish within %s; the launcher serves its roles", gitLockFirstPassBound)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pass(ctx, false)
			}
		}
	}()
	return done
}

// learn takes the workspace of a role start (LEGION_WORKSPACE) when the sweep has none, and
// sweeps it at once, under the first pass's rule: the start that follows runs the pod's bundle
// restore, which runs real git and jj against that clone, and a stale lock fails it. It happens
// once per pod — a sweep that already has a workspace keeps it, since the roles of a pod work one
// workspace — and a second role's start that arrives while it runs waits here for it, which is
// how the pod's one restore serves every role (bundleRestore.apply).
//
// This pass carries no gitLockFirstPassBound: that bound is the pod's boot, which the launcher's
// own first pass runs inside, where this one runs inside a start whose restore walks the same
// clone with real git and jj under a budget of its own (restoreBudget). It runs under the
// container's context all the same, so the container's stop ends it.
//
// A nil sweep learns nothing: a manager built without one is a test's, and the controller pod's
// single-role launcher has none at all.
//
// A sweep the launcher's own working directory gave its workspace learns nothing either, and
// answers so before it takes mu: mu is held for a whole pass, the recheck wait inside it, and in
// the ordinary pod — where that working directory is the issue's workspace — a start would
// otherwise wait a pass out to find there was nothing to learn. fromWorkDir is settled before any
// role is served and never written again, so reading it unlocked races nothing, and the locked
// body it skips provably does nothing: workspace is not empty wherever fromWorkDir is set, which
// is the one thing that body tests.
func (s *gitLockSweep) learn(ctx context.Context, dir string) {
	if s == nil || dir == "" || s.fromWorkDir {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workspace != "" {
		return
	}
	s.workspace = dir
	s.log("launcher: the git lock sweep works the workspace %s, which the first role start named", dir)
	s.sweep(ctx, true)
}

// pass runs one pass of the sweep: the launcher's first (first), which removes every lock no
// process holds, or a clock pass, which decides each lock by its three conditions.
func (s *gitLockSweep) pass(ctx context.Context, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep(ctx, first)
}

// sweep is one pass with the lock held. The clock is read once, so every line of one pass dates
// the locks it found from the same instant.
//
// A pass that cannot resolve the clone's git directory or list its locks writes one line and ends
// there; it fails nothing, since this runs in PID 1 of the container and a launcher that failed
// its container would cost the issue every role it serves. No lock at all is no line: a pass a
// minute for a pod's life has nothing to say about a clone nobody wedged.
func (s *gitLockSweep) sweep(ctx context.Context, first bool) {
	if s.workspace == "" {
		return
	}
	gitDir, err := s.gitDir(s.workspace)
	if err != nil {
		s.refuse(fmt.Sprintf("the workspace %s: %v", s.workspace, err))
		return
	}
	paths, err := s.locks(gitDir, s.workspace)
	if err != nil {
		s.refuse(fmt.Sprintf("the clone of %s: %v", s.workspace, err))
		return
	}
	s.refusal = ""
	if len(paths) == 0 {
		return
	}
	now := s.now()
	found := s.read(paths)
	if len(found) == 0 {
		return
	}
	// The holder read runs even on the first pass, where no age bound and no second read apply:
	// it costs one walk of /proc and keeps a lock open in a process the sweep did not expect —
	// a `kubectl exec` into the container, a jj of a role whose start raced the launcher's own.
	held, err := s.holders(found)
	if err != nil {
		s.refuse(err.Error())
		return
	}
	var again []gitLock
	for _, lock := range found {
		if ctx.Err() != nil {
			return
		}
		switch {
		case held.blind:
			s.kept(lock, now, "a git or jj whose open files cannot be read is running")
		case held.open[lock.path]:
			s.kept(lock, now, "a process of the container holds it open")
		case first:
			s.removed(lock, now)
		case now.Sub(lock.info.ModTime()) < s.guard:
			s.kept(lock, now, fmt.Sprintf("it is younger than the %s guard", s.guard))
		default:
			again = append(again, lock)
		}
	}
	if len(again) == 0 {
		return
	}
	// The second read of the locks this pass would remove: at least the recheck wait after the
	// first, one more walk of /proc, and the same file by device, inode and mtime. A writer that
	// took one of them between the two reads is what this catches, and the next pass decides it
	// again.
	select {
	case <-ctx.Done():
		return
	case <-time.After(s.recheck):
	}
	later, err := s.holders(again)
	if err != nil {
		s.refuse(err.Error())
		return
	}
	for _, lock := range again {
		if ctx.Err() != nil {
			return
		}
		info, err := os.Lstat(lock.path)
		if errors.Is(err, fs.ErrNotExist) {
			// Gone between the reads: its writer finished with it, which is the outcome this
			// pass wanted and no line of its own.
			continue
		}
		switch {
		case later.blind:
			s.kept(lock, now, "a git or jj whose open files cannot be read is running")
		case later.open[lock.path]:
			s.kept(lock, now, "a process of the container holds it open")
		case err != nil || !os.SameFile(info, lock.info) || !info.ModTime().Equal(lock.info.ModTime()):
			s.kept(lock, now, "it changed between the two reads")
		default:
			s.removed(lock, now)
		}
	}
}

// gitLock is one lock file as the pass read it: its path and the FileInfo every later read is
// compared with, by device and inode (os.SameFile) and by mtime.
type gitLock struct {
	path string
	info fs.FileInfo
}

// read is the first read of the locks the walk answered. A lock already gone is nobody's to
// remove and no line; one that cannot be read is logged and left, since a pass that cannot read a
// file must not remove it.
func (s *gitLockSweep) read(paths []string) []gitLock {
	found := make([]gitLock, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			s.log("launcher: the git lock %s could not be read (%v); it stays where it is", path, err)
			continue
		}
		found = append(found, gitLock{path: path, info: info})
	}
	return found
}

// gitLockHolders is what one walk of the container's /proc answered about the locks of one read:
// which of them some process holds open, and whether a git or jj whose open files could not be
// read is running, which counts as a holder of every one of them.
type gitLockHolders struct {
	open  map[string]bool
	blind bool
}

// holders walks the container's /proc once and reads every process's open files, comparing each
// descriptor with each lock by device and inode (os.SameFile over the FileInfo of os.Stat on the
// descriptor and the one the lock was read as), never by the path the link prints: a descriptor's
// link carries the name the file had when it was opened, which a rename leaves behind, and
// `(deleted)` once it is gone.
//
// A process whose fd directory cannot be read is the one case a walk cannot decide. When its comm
// is exactly `git` or `jj` it counts as a holder of every lock of this read, since it is the kind
// of process that takes one; comm is a line, so the comparison is of its trimmed content, and
// nothing else matches — a `git-remote-https` holds no lock of this clone. Any other such process
// is passed over, as is one that has gone between the walk and the read of its files.
func (s *gitLockSweep) holders(locks []gitLock) (gitLockHolders, error) {
	pids, err := procPIDs(s.proc)
	if err != nil {
		return gitLockHolders{}, fmt.Errorf("read the container's processes: %v", err)
	}
	held := gitLockHolders{open: map[string]bool{}}
	for _, pid := range pids {
		process := filepath.Join(s.proc, strconv.Itoa(pid))
		open, err := os.ReadDir(filepath.Join(process, "fd"))
		if err != nil {
			if gitOrJJ(process) {
				held.blind = true
			}
			continue
		}
		for _, fd := range open {
			info, err := os.Stat(filepath.Join(process, "fd", fd.Name()))
			if err != nil {
				continue
			}
			for _, lock := range locks {
				if os.SameFile(info, lock.info) {
					held.open[lock.path] = true
				}
			}
		}
	}
	return held, nil
}

// gitOrJJ says whether the process at dir is a git or a jj, read from its comm: a process that
// takes the locks this sweep removes. A process that has gone answers nothing and is neither.
func gitOrJJ(dir string) bool {
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return false
	}
	switch strings.TrimSpace(string(comm)) {
	case "git", "jj":
		return true
	}
	return false
}

// removed takes one lock and writes its line. A removal that fails is logged and the pass goes
// on: this runs in PID 1 of the container and must never end it, and the next pass decides the
// lock again. One already gone is no line, since this pass removed nothing.
func (s *gitLockSweep) removed(lock gitLock, now time.Time) {
	if err := s.remove(lock.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		s.log("launcher: the git lock %s could not be removed (%v); it stays where it is", lock.path, err)
		return
	}
	s.log("%s", s.line("removed", lock, now))
}

// kept writes the line of a lock a condition kept where it is. The next pass decides it again.
func (s *gitLockSweep) kept(lock gitLock, now time.Time, why string) {
	s.log("%s", s.line("kept", lock, now)+": "+why)
}

// line is what a removal and a keep both say of a lock: its path, its size, when it was last
// written and how long ago that was, rounded to the second as the rest of this tree prints a
// duration. An operator reading the container's log has there what the file was, without the pod
// to look at.
func (s *gitLockSweep) line(verb string, lock gitLock, now time.Time) string {
	return fmt.Sprintf("launcher: %s the git lock %s, %d bytes, last written %s (%s ago)",
		verb, lock.path, lock.info.Size(), lock.info.ModTime().UTC().Format(time.RFC3339),
		now.Sub(lock.info.ModTime()).Round(time.Second))
}

// refuse writes one line for a pass that could not run, and never the same reason twice in a row:
// a clone the sweep cannot read is a line a minute for the pod's whole life otherwise, which
// buries every other line the roles write.
func (s *gitLockSweep) refuse(reason string) {
	if reason == s.refusal {
		return
	}
	s.refusal = reason
	s.log("launcher: the git lock sweep: %s", reason)
}
