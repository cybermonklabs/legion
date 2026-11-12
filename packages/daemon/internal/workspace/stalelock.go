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

// GitLocks is every git lock file in one issue's clone that git or jj could take: the
// `<file>.lock` git's lockfile API and jj's git backend create beside a file they are about to
// replace and rename over it once it is written. A process killed or interrupted mid-write leaves
// its lock, and every later write of that file fails on it until someone deletes it. It walks
// gitDir, the clone's git directory, apart from its worktrees subtree, and then each worktree
// entry under gitDir/worktrees whose gitdir names workspaceDir, in the order
// gitDirWorktreeEntries lists them; another workspace's entry keeps its locks. Both the skipped
// subtree and the entries are read from the one gitDir it is given, so a caller whose git
// directory is not `<clone>/.git` is answered about that directory and no other. A directory that
// does not exist holds no locks and is no error. A gitDir or gitDir/worktrees that is a symlink is
// refused before anything is answered, as gitDirWorktreeEntries refuses it.
//
// It answers the paths alone, because its two callers decide a lock differently:
// RemoveStaleGitLocks, the issue pod's `workspace-init provision`, removes every lock it is
// answered and fails its init container on one it cannot remove. Its second caller,
// the launcher of the pod's roles container, removes a lock only when no process of the container
// holds it open and it is older than a guard, and fails the container for nothing. A lock some
// process still holds must never be removed: two writers would then replace one file, so a
// caller that cannot rule a holder out must rule it out per lock.
func GitLocks(gitDir, workspaceDir string) ([]string, error) {
	own, err := gitDirWorktreeEntries(gitDir, workspaceDir)
	if err != nil {
		return nil, err
	}
	skip := filepath.Join(gitDir, "worktrees")
	var locks []string
	for _, dir := range append([]string{gitDir}, own...) {
		err := walkLocks(dir, skip, func(path string, _ fs.DirEntry) error {
			locks = append(locks, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return locks, nil
}

// RemoveStaleGitLocks removes every git lock file in workspace's clone that git or jj could take
// (GitLocks, which also refuses a symlinked .git or .git/worktrees before anything is removed):
// the ones under the clone's git directory, apart from its worktree entries, and the ones under
// the clone's worktree entries for workspace, another workspace's entry keeping its locks. Each
// removal is logged with the lock's path and age, and a lock it cannot remove is an error naming
// it, which fails the provisioning.
//
// A lock some process still holds must never be removed: two writers would then replace one file.
// This caller removes each lock unasked because no process of the issue can hold one yet: it is a
// Kubernetes issue pod's `workspace-init provision`, which runs before the pod's launcher
// container starts, on the issue's own volume, which no other pod mounts. GitLocks' second
// caller, the launcher of the pod's roles container, runs where processes of the issue are
// running, so it decides each lock for itself. The tmux runtime's provisioning never calls this,
// since every pane of the repository shares the clone there. jj's own locks need nothing: they
// are flock(2) locks the kernel releases with their holder, so a lock file one leaves blocks no
// jj.
func RemoveStaleGitLocks(workspace Workspace, now time.Time, log func(string)) error {
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return err
	}
	locks, err := GitLocks(filepath.Join(workspace.Clone, ".git"), workspace.Dir)
	if err != nil {
		return err
	}
	why := "no container of the issue's pod has started, and no other pod mounts its volume"
	for _, path := range locks {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("read the stale git lock %s: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the stale git lock %s: %w", path, err)
		}
		log(fmt.Sprintf("removed the stale git lock %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
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
	return walkLocks(dir, "", func(path string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("read the stale git lock %s: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove the stale git lock %s: %w", path, err)
		}
		log(fmt.Sprintf("removed the stale git lock %s, last written %s ago: %s", path, now.Sub(info.ModTime()).Round(time.Second), why))
		return nil
	})
}
