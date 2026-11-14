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
// against one pass, which walks the clone's git directory once and reads the container's /proc
// only where that walk found a lock: once for the locks it found, and a second time on a clock
// pass that reaches the recheck before it removes any of them (holders).
var gitLockInterval = time.Minute

// gitLockGuard is the age under which a lock is kept: a lock younger than this is taken for one
// a live writer made, whatever /proc said. It is Gitaly's, GitLab's git service, which removes a
// stale lock file after 15 minutes, because the length of git's close-to-rename window is not
// measured here: nothing in this tree knows how long the slowest write of a ref, of packed-refs
// or of the index may hold its lock on an issue's volume, and a number taken from a service that
// runs git at scale is a better guess than one invented. A test varies it through the sweep's
// own guard field rather than here.
const gitLockGuard = 15 * time.Minute

// gitLockRecheck is the wait between the two reads of a lock a clock pass is about to remove. The
// holder read and the age read are two instants of one clone that is being written: a second read
// of the same file, with no holder and the same mtime, is how a pass tells a lock nothing is
// using from one a writer took between the pass's reads. A test shortens it, because this
// package's tests run under the race detector.
var gitLockRecheck = 2 * time.Second

// gitLockRemove is the removal newGitLockSweep gives the sweep (gitLockSweep.remove): os.Remove.
// It is a variable for the row that holds a real launcher's pass inside a role start, which
// builds its sweep here and so takes no field of its own. The removal is the one place such a
// row can hold a pass at: every read a pass makes of the clone is one a role cannot hold, a
// worktree entry's gitdir that is a fifo among them (internal/workspace's readGitdir).
var gitLockRemove = os.Remove

// gitLockFirstPassBound is how long the launcher's first pass may spend between the locks it
// decides. It exists for the reason testPGBound exists: the first pass runs before the launcher
// serves any role, the daemon's boot timeout for a pod is 120 s by default, and a pass that goes
// on and on must not take the pod's whole boot with it. A pod whose locks were left where they
// are, with a line in the container's log, is better than a pod the daemon gives up on.
//
// What it bounds is the loop over the locks, which is where a pass reads its context (decide).
// The recheck wait between a clock pass's two reads is no part of it: a first pass decides every
// lock where it stands (case first), leaves none for a second read and returns before that wait.
// The walk of the clone (workspace.GitLocksOf), the walk of /proc, os.Lstat and os.Remove take no
// context of their own, so a volume that hangs inside one of them holds the launcher's start for
// as long as it hangs, and this bound does not cover that. The first pass is run in the
// launcher's own goroutine all the same, rather than in one of its own the launcher stops
// waiting for at the bound, because the first pass's rule — every lock no process holds goes,
// whatever its age and with no second read — holds only while no process of the container can
// hold a lock. A pass still walking the clone once the launcher serves its roles would sweep a
// clone a role is already working, under a rule that by then is false.
const gitLockFirstPassBound = 30 * time.Second

// gitLockSweep removes the stale git locks of one issue's clone, for the life of the roles
// container. It holds its inputs as fields rather than reading them from the launcher's
// configuration or the package's variables, so a test drives it over a clone and a /proc of its
// own, and a second tree that sweeps the same locks differently reuses it.
//
// ws is the jj workspace the clone is swept through and the clone itself, which the launcher
// derives from its own working directory and otherwise learns from the first role start it
// serves (learn), and fromWorkDir says which of the two it was. proc is the container's /proc,
// which the holder read walks. guard, interval and recheck are the package's three numbers, read
// once by whoever builds the sweep. remove and now are the removal (os.Remove, through
// gitLockRemove) and the clock, each a field because a test answers them itself: one to refuse or
// hold a removal, one to pin a line's age. The walk (workspace.GitLocksOf) is called outright,
// since every caller of the sweep, test or not, wants the real one; it is the layout guard and
// the walk of the clone in one call, so every pass of this sweep holds the rule the init
// container's own sweep holds, from the same helper. The workspace is derived once, by whoever
// gave the sweep its workspace, and held in ws. log is the launcher's own log line,
// roleLog.linef, which carries no role prefix: the sweep belongs to no role.
//
// mu serializes the passes and the learning, so the pod's roles never sweep one clone twice at
// once and a role start that arrives while the first pass runs waits for it.
type gitLockSweep struct {
	proc     string
	guard    time.Duration
	interval time.Duration
	recheck  time.Duration
	remove   func(string) error
	now      func() time.Time
	log      func(format string, args ...any)

	// fromWorkDir says the launcher's own working directory gave the sweep its workspace. It is
	// settled before the launcher serves any role and never written again, so learn reads it
	// without mu, which a pass holds for its whole span.
	fromWorkDir bool

	mu sync.Mutex
	// ws is the workspace the sweep walks and the shared clone it belongs to, derived once
	// (workspace.WorkspaceOf) by whoever gave the sweep its workspace, and read by every pass
	// after. It is derived from the workspace's own path through the runtime's layout, and the
	// two files jj resolves a git directory through — the workspace's .jj/repo and the
	// repository's store/git_target — are not read at all: every role of the pod can write
	// them, and an absolute path in either is taken as it stands, so a sweep that read them
	// would walk and remove under whatever directory the last writer named, under another
	// role's entry of /roles, which role-init hides from the role, as readily as under the
	// clone's own.
	//
	// The first pass's rule and this derivation rest on two different properties, and a roles
	// container that restarts on the pod's own disk keeps only one of them. The pod's restart
	// policy is Always and its disk lives as long as the pod, so a container that ends — an
	// out-of-memory kill among the ways — comes back on a disk its own last roles wrote
	// (restore.go's bundle restore already handles such a restart).
	//
	// "No process of the container holds a lock yet" survives that: the restarted container
	// starts with no role process of its own, which is what lets the first pass remove every
	// lock no process holds whatever its age, and the launcher's own first pass runs before it
	// serves anything (newGitLockSweep, serve).
	//
	// "What the pod's provisioning wrote is what is read" does not survive it: a role of the
	// container before the restart could have written store/git_target, and the new launcher
	// reading it would take a role's file for the provisioning's and send its first pass, which
	// removes at any age, wherever that file points. That is why the clone is derived from the
	// layout rather than read from the workspace's files.
	//
	// What the derivation settles is the text of a path, which no role can write. The
	// directories that text names are another matter: they are on the issue's volume, mounted
	// read-write in this container, where every role writes as the pod's uid, so a role can
	// rename the clone away and leave a symlink to a directory of its own where it stood. What
	// holds the sweep inside the clone is therefore not the derivation but the layout guard,
	// which runs on every pass before the walk (workspace.GitLocksOf, refuseSymlinkedLayout),
	// together with the walk's own check that each worktree entry it opens names this
	// workspace. A planted link still there when a pass runs is refused in one line, and that
	// pass removes nothing. What is left open runs from the guard through the whole walk to each
	// lock's first read (read): WalkDir goes down by path, so a directory inside .git swapped for
	// a link during the walk is followed, and the recheck only ties the removal to what that
	// first read found. Closing that takes openat and unlinkat down every component of every path
	// the sweep touches, which this deployment does not build without a person's approval. It is
	// a named limit of this sweep, deliberately left open.
	ws workspace.Workspace
	// learned says the sweep has already been given a workspace — by the launcher's own working
	// directory (newGitLockSweep) or by a role start, whether or not the derivation that followed
	// a start answered one — so no later start derives again. It is what learn's locked body
	// tests.
	learned bool
	// refusal is the last reason a pass was refused for, so the same one is written once rather
	// than once a minute for the pod's life. Only a pass that made every read it meant to and
	// was not stopped forgets it (sweep), so a clone that breaks again is reported again.
	refusal string
}

// newGitLockSweep is the roles container's sweep over the workspace its own working directory is:
// the pod sets that container's working directory to the issue's workspace
// (internal/runtime/sandbox's podkind, WorkingDir: l.workspace), so the launcher needs nothing
// from the daemon to know which clone is its own. A working directory that cannot be read, or
// whose clone cannot be derived from it — a directory outside the runtime's workspace layout, a
// directory of that layout below the state directory that is a symlink, a derived clone with no
// git directory, and a workspace whose .git cannot be read, names no worktree entry, or names an
// entry whose gitdir names another path (each named with the line that says so) alike, which is
// every failure of workspace.WorkspaceOf — leaves the sweep without one: it writes that
// here, once, and takes the workspace from the first role start it serves instead, which carries
// LEGION_WORKSPACE. A workspace whose .git names an entry whose gitdir cannot be read is no such
// failure: the derivation accepts it, so a container restarted over that disk still sweeps the
// clone's git directory, and its first pass names the entry. A working directory that did supply
// it is recorded as having done so (fromWorkDir), so a role start finds there is nothing to learn
// without taking the sweep's lock.
//
// A working directory the layout accounts for gives its clone here and nowhere else (ws). The
// derived clone goes out in a line of its own, as does the reason a derivation was refused, so a
// person reading the pod's log sees which clone every pass of the pod walks, or which one it
// would not walk and why.
//
// The two numbers a test shortens, the interval and the recheck, and the removal a test holds are
// read here, in the caller's own goroutine, and never inside the ticker's: they are package
// variables, and a goroutine still reading one after the test restored it is a data race the race
// detector fails the test for (serveTestPG reads its interval the same way). The guard and the
// first pass's bound are constants nothing writes; a test that varies the guard writes the
// sweep's own field.
func newGitLockSweep(log func(format string, args ...any)) *gitLockSweep {
	s := &gitLockSweep{
		proc: "/proc", guard: gitLockGuard, interval: gitLockInterval, recheck: gitLockRecheck,
		remove: gitLockRemove, now: time.Now, log: log,
	}
	dir, err := os.Getwd()
	if err != nil {
		s.log("launcher: the git lock sweep cannot read the launcher's working directory (%q); it takes its workspace from the first role start", err)
		return s
	}
	ws, err := workspace.WorkspaceOf(dir)
	if err != nil {
		s.log("launcher: the git lock sweep's working directory %q is no jj workspace it can sweep (%q); it takes its workspace from the first role start", dir, err)
		return s
	}
	s.ws = ws
	s.fromWorkDir = true
	s.learned = true
	s.log("launcher: the git lock sweep works the workspace %q, which is the launcher's own working directory, and its clone %q", dir, ws.Clone)
	return s
}

// serve runs the launcher's first pass and then one pass per interval until ctx ends, and answers
// a channel closed once its ticker goroutine has returned, so a caller that ends ctx can wait for
// it rather than leave it running (serveTestPG answers the same way).
//
// The first pass is this call's, before the launcher serves any role: its rule is that no process
// of the container can hold a lock yet, which holds only while that is true. It runs under
// gitLockFirstPassBound, which ends it between two of the clone's locks rather than inside a
// syscall the kernel owns, and which covers that much of a pass and no more
// (gitLockFirstPassBound). A pass the bound stopped writes one line and the launcher goes on to
// serve its roles; a pass that took longer than the bound and finished all the same writes
// nothing, since it left nothing undecided.
func (s *gitLockSweep) serve(ctx context.Context) <-chan struct{} {
	interval := s.interval
	bounded, cancel := context.WithTimeout(ctx, gitLockFirstPassBound)
	stopped := s.pass(bounded, true)
	cancel()
	if stopped && ctx.Err() == nil {
		s.log("launcher: the git lock sweep's first pass stopped at its bound of %s with locks it had not decided; the launcher serves its roles", gitLockFirstPassBound)
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
// at the first start that carries a non-empty LEGION_WORKSPACE — a start that carries none is
// passed over below, and a sweep that already has a workspace keeps it, since the roles of a
// pod work one workspace — and a second role's start that arrives while it runs waits here for
// it, which is how the pod's one restore serves every role (bundleRestore.apply).
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
// body it skips provably does nothing: newGitLockSweep sets learned wherever it sets fromWorkDir,
// and learned is what that body tests.
//
// The clone is derived here, once, from the workspace's own path (workspace.WorkspaceOf), and
// the workspace's files are not read for it at all, which is what makes the moment this runs no
// part of the argument (ws). It runs inside the start that serves the pod's first role
// generation, before that start's own child exists (manager.start calls this before
// process.start), and mu holds a second role's start behind it. A derivation that fails is the
// sweep's answer for the pod's life all the same (learned), so no later start derives again with
// a role of the pod already running: such a start's learn would run a first pass, which removes
// every unheld lock at any age — a rule that holds only while no process of the container can
// hold one — over a clone the pod's roles are working. The sweep refuses with the reason here,
// once, and every pass after has no workspace and nothing to sweep (sweep).
func (s *gitLockSweep) learn(ctx context.Context, dir string) {
	if s == nil || dir == "" || s.fromWorkDir {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.learned {
		return
	}
	s.learned = true
	ws, err := workspace.WorkspaceOf(dir)
	if err != nil {
		s.refuse(fmt.Sprintf("the workspace %s: %v", dir, err))
		return
	}
	s.ws = ws
	s.log("launcher: the git lock sweep works the workspace %q, which the first role start named, and its clone %q", dir, ws.Clone)
	s.sweep(ctx, true)
}

// pass runs one pass of the sweep: the launcher's first (first), which removes every lock no
// process holds, or a clock pass, which decides each lock by its three conditions. It answers
// what sweep answers, whether the pass stopped on ctx with locks left undecided.
func (s *gitLockSweep) pass(ctx context.Context, first bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweep(ctx, first)
}

// sweep is one pass with the lock held, and answers whether the pass stopped on ctx with locks
// left undecided, which is what the launcher's first pass reads its bound from (serve).
//
// A pass that cannot list the clone's locks or read the container's processes writes one line and
// ends there; it fails nothing, since this runs in PID 1 of the container and a launcher that
// failed its container would cost the issue every role it serves. A sweep with no workspace has
// nothing to sweep and ends here without a line: either it never learned a workspace, or the one
// derivation it made failed and wrote its reason where it was made (newGitLockSweep, learn).
//
// A pass that found no worktree entry whose gitdir names this workspace (workspace.GitLocksOf's
// unswept, one of the cases of internal/workspace's gitWorktreeEntries table) decides the locks
// it did list, the clone's git directory's, and then writes why through refuse, so the reason is
// one line while it lasts and not a line a minute. The workspace's .git chose only that line:
// what the pass walked is the git directory and the entries whose own gitdir names the
// workspace. A refusal of the same pass comes first: it is the line that pass writes, and the
// reason follows at the first pass that gets past it.
//
// The stored refusal is forgotten here, and only by a pass that made every read it meant to: a
// pass that listed the locks and then could not read /proc, or found no entry of this workspace,
// would otherwise clear the very reason it is about to write again, and refuse's rule — the same
// reason once, rather than once a minute for the pod's life — would hold for no pass at all. A
// reason that ends is therefore forgotten by the first pass that finds an own entry, and written
// again if it comes back.
func (s *gitLockSweep) sweep(ctx context.Context, first bool) bool {
	if s.ws.Dir == "" {
		return false
	}
	paths, unswept, err := workspace.GitLocksOf(s.ws)
	if err != nil {
		s.refuse(fmt.Sprintf("the clone of %s: %v", s.ws.Dir, err))
		return false
	}
	reason, stopped := s.decide(ctx, first, paths)
	switch {
	case reason != "":
		s.refuse(reason)
	case unswept != nil:
		s.refuse(fmt.Sprintf("the clone of %s: %v; the pass swept only the clone's git directory", s.ws.Dir, unswept))
	case !stopped:
		s.refusal = ""
	}
	return stopped
}

// decide is the body of one pass over the locks the walk listed: it answers the reason the pass
// was refused for, or no reason at all, and whether it stopped on ctx with locks left undecided.
// The clock is read once, so every line of one pass dates the locks it found from the same
// instant. No lock at all is no line: a pass a minute for a pod's life has nothing to say about a
// clone nobody wedged.
func (s *gitLockSweep) decide(ctx context.Context, first bool, paths []string) (string, bool) {
	if len(paths) == 0 {
		return "", false
	}
	now := s.now()
	found := s.read(paths)
	if len(found) == 0 {
		return "", false
	}
	// The holder read runs even on the first pass, where no age bound and no second read apply:
	// it costs one walk of /proc and keeps a lock open in a process the sweep did not expect —
	// a `kubectl exec` into the container, a jj of a role whose start raced the launcher's own.
	held, err := s.holders(found)
	if err != nil {
		return err.Error(), false
	}
	var again []gitLock
	for _, lock := range found {
		if ctx.Err() != nil {
			return "", true
		}
		switch why := held.keeps(lock.path); {
		case why != "":
			s.kept(lock, now, why)
		case first:
			s.removed(lock, now)
		case now.Sub(lock.info.ModTime()) < s.guard:
			s.kept(lock, now, fmt.Sprintf("it is younger than the %s guard", s.guard))
		default:
			again = append(again, lock)
		}
	}
	if len(again) == 0 {
		return "", false
	}
	// The second read of the locks this pass would remove: at least the recheck wait after the
	// first, one more walk of /proc, and the same file by device, inode and mtime. A writer that
	// took one of them between the two reads is what this catches, and the next pass decides it
	// again.
	select {
	case <-ctx.Done():
		return "", true
	case <-time.After(s.recheck):
	}
	later, err := s.holders(again)
	if err != nil {
		return err.Error(), false
	}
	for _, lock := range again {
		if ctx.Err() != nil {
			return "", true
		}
		info, err := os.Lstat(lock.path)
		if errors.Is(err, fs.ErrNotExist) {
			// Gone between the reads: its writer finished with it, which is the outcome this
			// pass wanted and no line of its own.
			continue
		}
		switch why := later.keeps(lock.path); {
		case why != "":
			s.kept(lock, now, why)
		case err != nil || !os.SameFile(info, lock.info) || !info.ModTime().Equal(lock.info.ModTime()):
			s.kept(lock, now, "it changed between the two reads")
		default:
			s.removed(lock, now)
		}
	}
	return "", false
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
			s.log("launcher: the git lock %q could not be read (%q); it stays where it is", path, err)
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

// keeps is the reason this walk of /proc keeps the lock at path where it is, or no reason at
// all: a git or jj whose open files could not be read is running, which counts as a holder of
// every lock of the read, or a process of the container holds this one open. Both of a clock
// pass's decisions ask it, the one before the recheck and the one after, so a holder reads the
// same at either.
func (h gitLockHolders) keeps(path string) string {
	switch {
	case h.blind:
		return "a git or jj whose open files cannot be read is running"
	case h.open[path]:
		return "a process of the container holds it open"
	}
	return ""
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

// removed takes one lock and writes its line. A removal that fails writes the lock's keep line
// with the failure as the condition that kept it, since a lock the pass did not remove is a lock
// it kept and carries what every kept lock's line carries, and the pass goes on: this runs in
// PID 1 of the container and must never end it, and the next pass decides the lock again. One
// already gone is no line, since this pass removed nothing.
func (s *gitLockSweep) removed(lock gitLock, now time.Time) {
	if err := s.remove(lock.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		s.kept(lock, now, fmt.Sprintf("it could not be removed: %q", err))
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
//
// The path is quoted, as is every name and every error text the sweep's lines carry. A lock's
// path comes from a walk of a clone every role of the pod writes, and a Linux file name may hold
// a newline: a role that made `refs/heads/x\ntester: ...lock` would otherwise split this line in
// two in PID 1's log, which carries no role prefix, and the second half would read as that
// role's own output (internal/runtime/sandbox's observe, roleLines) — the very split
// prefixWriter exists to keep. An error of os.Lstat or os.Remove carries the same name in its
// own text, so it is quoted too.
func (s *gitLockSweep) line(verb string, lock gitLock, now time.Time) string {
	return fmt.Sprintf("launcher: %s the git lock %q, %d bytes, last written %s (%s ago)",
		verb, lock.path, lock.info.Size(), lock.info.ModTime().UTC().Format(time.RFC3339),
		now.Sub(lock.info.ModTime()).Round(time.Second))
}

// refuse writes one line for a pass that could not run, or that found no worktree entry of this
// workspace (sweep), and never the same reason twice in a row: a clone the sweep cannot
// read is a line a minute for the pod's whole life otherwise, which buries every other line the
// roles write. The reason goes out quoted, for the reason a lock's path does (line): it carries
// the paths the walk of the clone stopped on, which are names a role of the pod wrote.
func (s *gitLockSweep) refuse(reason string) {
	if reason == s.refusal {
		return
	}
	s.refusal = reason
	s.log("launcher: the git lock sweep: %q", reason)
}
