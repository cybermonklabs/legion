package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// checkoutMarkerFile is the marker createWorkspace writes inside a workspace's `.jj` directory
// right after its `jj workspace add` succeeds, with no file yet, before it records the workspace's
// creation time and, for an issue GitHub has no branch of, sets its bookmark. It stays until a `jj
// sparse reset` into the workspace succeeds and Provision removes it. While it exists beside an
// empty sparse-pattern set, the next provision knows the provision that added the workspace stopped
// somewhere from that add to its checkout's end, so it removes the workspace
// (resetUnfinishedWorkspace, step workspace-reset) and adds it again from nothing. Without the
// marker, a workspace whose sparse patterns an agent cleared keeps its untracked files.
const checkoutMarkerFile = "legion-checkout"

// writeCheckoutMarker creates or truncates `.jj/legion-checkout` in dir through openJJ and
// rolefile.OpenRegular, following no link and refusing any entry there that is no regular file.
func writeCheckoutMarker(dir string) error {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	file, err := rolefile.OpenRegular(int(jj.Fd()), checkoutMarkerFile, filepath.Join(jj.Name(), checkoutMarkerFile), unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	return nil
}

// hasCheckoutMarker reports whether `.jj/legion-checkout` exists as a regular file in dir, opened
// through openJJ and rolefile.OpenRegular without following a link or blocking on a special file.
func hasCheckoutMarker(dir string) (bool, error) {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return false, fmt.Errorf("read checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	file, err := rolefile.OpenRegular(int(jj.Fd()), checkoutMarkerFile, filepath.Join(jj.Name(), checkoutMarkerFile), unix.O_RDONLY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read checkout marker of workspace %s: %w", dir, err)
	}
	file.Close()
	return true, nil
}

// removeCheckoutMarker removes `.jj/legion-checkout` in dir through openJJ and rolefile.Unlinkat,
// following no link. A missing marker is no error.
func removeCheckoutMarker(dir string) error {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("remove checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	if err := rolefile.Unlinkat(int(jj.Fd()), checkoutMarkerFile, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove checkout marker of workspace %s: %w", dir, &fs.PathError{
			Op:   "unlinkat",
			Path: filepath.Join(jj.Name(), checkoutMarkerFile),
			Err:  err,
		})
	}
	return nil
}

// resetUnfinishedWorkspace removes the workspace Provision found marked (checkoutMarkerFile) with
// no sparse pattern, under the store lock, so the provision adds it again from nothing
// (createWorkspace). The provision that added it stopped somewhere from its `jj workspace add` to
// its checkout's end, and no role starts in a workspace before its provision returns, so nothing
// there is anyone's work: the directory holds at most what a cut-short checkout wrote, and the
// shared clone the workspace's registration, its empty working-copy commit and, for an issue
// GitHub had no branch of, the bookmark that provision set on that commit. The steps are Remove's,
// without its abandon of the workspace's own commits (a marked workspace has none but that empty
// commit, which the forget abandons) and with the deletion of the issue's bookmark from the
// working-copy commit:
//   - A directory a kill left renamed aside before its delete (removingPath) is cleared.
//   - Where the issue's bookmark is on the workspace's working-copy commit, it is deleted, so the
//     workspace added again starts where a first provision starts: at the default branch, with the
//     bookmark on its new working-copy commit, or at origin's branch where GitHub has made one
//     since. Left there, the bookmark would start the new workspace on top of the old empty commit,
//     which has no description, so jj would refuse every push of the issue's branch. The deletion
//     goes before the rename, so a kill between them leaves a marked workspace that the next
//     provision resets again.
//   - The directory is renamed aside, in one atomic rename, so a kill partway through the delete,
//     whose time follows what the checkout wrote, leaves no half-deleted workspace at its own path:
//     a kill after the rename leaves the registered-but-missing workspace that the next provision
//     adds again, deleting the directory left aside before createWorkspace forgets and adds it.
//   - The workspace is forgotten in the shared clone, which abandons its working-copy commit, empty,
//     undescribed and carrying no bookmark.
//   - Its git worktree entry, which jj's forget leaves for a directory already gone, is removed.
//   - The directory renamed aside is deleted.
func resetUnfinishedWorkspace(ctx context.Context, run Runner, workspace Workspace) error {
	cloneDir, workspaceName := workspace.Clone, filepath.Base(workspace.Dir)
	aside := removingPath(workspace.Dir)
	if err := rolefile.RemoveTree(aside); err != nil {
		return fmt.Errorf("reset workspace %s: clear a stale removal-in-progress directory: %w", workspace.Dir, err)
	}
	registered, err := workspaceRegistered(ctx, run, cloneDir, workspaceName)
	if err != nil {
		return err
	}
	if registered {
		onWorkingCopy, err := runCheckedOnClone(ctx, run, cloneDir, "log", "-r", workspaceName+"@ & bookmarks("+exactName(workspace.Bookmark)+")", "--no-graph", "-T", `commit_id ++ "\n"`)
		if err != nil {
			return err
		}
		if len(nonEmptyLines(onWorkingCopy.Stdout)) > 0 {
			if _, err := runCheckedOnClone(ctx, run, cloneDir, "bookmark", "delete", exactName(workspace.Bookmark)); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(workspace.Dir, aside); err != nil {
		return fmt.Errorf("reset workspace %s: rename it aside before removing it: %w", workspace.Dir, err)
	}
	if registered {
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "workspace", "forget", workspaceName); err != nil {
			return err
		}
	}
	if err := removeGitWorktree(cloneDir, workspace.Dir); err != nil {
		return err
	}
	if err := rolefile.RemoveTree(aside); err != nil {
		return fmt.Errorf("reset workspace %s: remove its directory: %w", workspace.Dir, err)
	}
	return nil
}
