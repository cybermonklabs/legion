package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GitLocksOf is every git lock file in workspace's clone that git or jj could take: the
// `<file>.lock` git's lockfile API and jj's git backend create beside a file they are about to
// replace and rename over it once it is written. A process killed or interrupted mid-write leaves
// its lock, and every later write of that file fails on it until someone deletes it. It walks the
// clone's git directory, apart from its worktrees subtree, and then each worktree entry under
// .git/worktrees whose gitdir names workspace.Dir, in the order gitWorktreeEntries lists them;
// another workspace's entry keeps its locks, and one whose gitdir cannot be read is passed over.
// A directory that does not exist holds no locks and is no error. A .git or .git/worktrees that
// is a symlink is refused before anything is answered, as gitWorktreeEntries refuses it.
//
// When no entry's gitdir names the workspace of a clone that has a git directory, unswept says
// why, by the case of gitWorktreeEntries' table: the workspace's .git cannot be read or names no
// entry (B), the entry it names cannot be read (C), or that entry's gitdir names another path
// (D). The locks answered are then the git directory's alone, since no entry is the workspace's
// own to walk, and the .git file chose only that line, never a lock. A clone with no git
// directory, the init container's fresh disk, has no locks and nothing to say. A caller reports
// unswept and goes on.
//
// The walk is answered only once the layout guard has held the workspace and its clone to the
// directories Location names (guardWorkspace, refuseSymlinkedLayout), and this is the only way to
// it: a walk is what removes files, and a caller outside this package must not be able to take
// one unguarded. The pod's two sweeps both ask here, so both hold that rule from one place: the
// init container's, which runs once before any container of the pod starts
// (RemoveStaleGitLocks), and the roles container's launcher, which runs a pass a minute for the
// life of the pod (internal/launcher's gitLockSweep).
//
// The guard runs on every call rather than once where the clone is derived (WorkspaceOf), because
// the derivation answers a path and the directories that path names are on the issue's volume,
// mounted read-write in the roles container, where every role writes as the pod's uid. A role
// that renames the clone away and leaves a symlink to a directory of its own in its place is
// refused by the pass that follows it, and no lock under what the link names is removed. What is
// left open runs from this guard through the whole walk to each lock's first read (the
// launcher's read): WalkDir goes down by path, so a directory inside .git swapped for a link
// during the walk is followed, and the launcher's recheck only ties the removal to what that first
// read found. No check of a path can close that gap, since closing it takes openat and unlinkat
// down every component of every path, which this deployment does not build without a person's
// approval. It is a known limit of the guard and not something the walk closes.
//
// It answers the paths alone, because its two callers decide a lock differently:
// RemoveStaleGitLocks, the issue pod's `workspace-init provision`, removes every lock it is
// answered and fails its init container on one it cannot remove. Its second caller,
// the launcher of the pod's roles container, removes a lock only when no process of the container
// holds it open and it is older than a guard, and fails the container for nothing. A lock some
// process still holds must never be removed: two writers would then replace one file, so a
// caller that cannot rule a holder out must rule it out per lock.
func GitLocksOf(workspace Workspace) (locks []string, unswept error, err error) {
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return nil, nil, err
	}
	own, unswept, err := gitWorktreeEntries(workspace.Clone, workspace.Dir)
	if err != nil {
		return nil, nil, err
	}
	gitDir := filepath.Join(workspace.Clone, ".git")
	skip := filepath.Join(gitDir, "worktrees")
	for _, dir := range append([]string{gitDir}, own...) {
		err := walkLocks(dir, skip, func(path string, _ fs.DirEntry) error {
			locks = append(locks, path)
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return locks, unswept, nil
}

// RemoveStaleGitLocks removes every git lock file in workspace's clone that git or jj could take
// (GitLocksOf, which runs the layout guard and refuses a symlinked .git or .git/worktrees before
// anything is removed): the ones under the clone's git directory, apart from its worktree
// entries, and the ones under the clone's worktree entries for workspace, another workspace's
// entry keeping its locks. Each removal is logged with the lock's path and age, and a lock it
// cannot remove is an error naming it, which fails the provisioning. No entry whose gitdir names
// the workspace (GitLocksOf's unswept) is a line saying why after the removals, and fails
// nothing: the git directory's locks are gone, and every entry's are left where they are.
//
// A lock some process still holds must never be removed: two writers would then replace one file.
// This caller removes each lock unasked because no process of the issue can hold one yet: it is a
// Kubernetes issue pod's `workspace-init provision`, which runs before the pod's launcher
// container starts, on the issue's own volume, which no other pod mounts. GitLocksOf's second
// caller, the launcher of the pod's roles container, runs where processes of the issue are
// running, so it decides each lock for itself. The tmux runtime's provisioning never calls this,
// since every pane of the repository shares the clone there. jj's own locks need nothing: they
// are flock(2) locks the kernel releases with their holder, so a lock file one leaves blocks no
// jj.
func RemoveStaleGitLocks(workspace Workspace, now time.Time, log func(string)) error {
	locks, unswept, err := GitLocksOf(workspace)
	if err != nil {
		return err
	}
	why := "no container of the issue's pod has started, and no other pod mounts its volume"
	for _, path := range locks {
		if err := removeStaleLock(path, now, log, why); err != nil {
			return err
		}
	}
	if unswept != nil {
		log(fmt.Sprintf("swept only the git directory of the workspace %s's clone: %v", workspace.Dir, unswept))
	}
	return nil
}

// RemoveStaleFeedLocks removes every git lock file in the bare repository gitDir, a feed: the
// `<file>.lock` a fetch killed mid-update leaves beside a ref, packed-refs, HEAD or the shallow
// file, which blocks every later update of that file. It also removes the `tmp_pack_*` and
// `tmp_idx_*` files a transfer killed mid-pack leaves under objects/pack: git writes a received
// pack under such a name and renames it once whole, so one left there is a pack that never
// arrived, which nothing reads and which a feed a retried fetch finds would otherwise carry,
// and hard-link into the issue's clone. No other object is touched. Each removal is
// logged with the file's path and age, and one it cannot remove is an error naming it. A feed's
// only writer is Fetch, called by one process at a time: a pod's workspace-fetch, its first init
// container and the one container that mounts the feed writable, or the repository cache's process
// under its maint mutex, which also keeps its repack from running beside a fetch (repocache's
// refresh), so no lock, transfer or repack there is live when Fetch starts.
func RemoveStaleFeedLocks(gitDir string, now time.Time, log func(string)) error {
	why := "a feed's only writer is the fetch now starting"
	if err := removeLocksUnder(gitDir, now, log, why); err != nil {
		return err
	}
	// The partial packs are looked for only where git writes them, and only when neither objects
	// nor objects/pack is a link: a glob follows one, and would remove files outside the feed.
	// A linked or missing directory is left for foreignFeed, which refuses the feed after this.
	pack := filepath.Join(gitDir, "objects", "pack")
	for _, dir := range []string{filepath.Dir(pack), pack} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			return nil
		}
	}
	partial, err := filepath.Glob(filepath.Join(pack, "tmp_*"))
	if err != nil {
		return err
	}
	for _, path := range partial {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("read the partial pack %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the partial pack %s: %w", path, err)
		}
		log(fmt.Sprintf("removed the partial pack %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
	}
	return nil
}

// walkLocks visits every `*.lock` file under dir but those under skip, one subtree it walks over,
// or none when skip is empty. A directory that does not exist holds no locks and is no error, and
// every other error of the walk is answered naming dir.
//
// visit decides what becomes of each lock, and runs inside the walk: a caller that removes there
// keeps what it removed when a later entry's error ends the walk.
func walkLocks(dir, skip string, visit func(path string, entry fs.DirEntry) error) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist) && path == dir:
			return nil
		case err != nil:
			return fmt.Errorf("look for stale git locks in %s: %w", dir, err)
		case entry.IsDir() && skip != "" && path == skip:
			return fs.SkipDir
		case entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock"):
			return nil
		}
		return visit(path, entry)
	})
}

// removeLocksUnder removes every `*.lock` file under dir, logging each with its age and why no
// process can hold it. It removes inside the walk, so a walk that ends on a later error leaves
// removed what it had already removed.
func removeLocksUnder(dir string, now time.Time, log func(string), why string) error {
	return walkLocks(dir, "", func(path string, _ fs.DirEntry) error {
		return removeStaleLock(path, now, log, why)
	})
}

// removeStaleLock removes one stale lock file and writes its line: the path, how long before now
// it was last written, rounded to the second as the rest of this tree prints a duration, and why
// no process can be holding it, which is the caller's to say. A read or a removal that fails is
// an error naming the file, and it fails the init container that asked for the removal.
//
// Both of its callers are the same removal under a walk of their own, the feed's and the issue
// clone's (removeLocksUnder, RemoveStaleFeedLocks' walk; RemoveStaleGitLocks' list), so it is
// written once: a second copy would take a change to one of the three texts here — the line an
// operator greps for and the two failures — and leave the other copy as it was.
func removeStaleLock(path string, now time.Time, log func(string), why string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read the stale git lock %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove the stale git lock %s: %w", path, err)
	}
	log(fmt.Sprintf("removed the stale git lock %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
	return nil
}
