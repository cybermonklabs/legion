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
//
// A stop between the checkout's end and the marker's removal leaves the marker on a whole
// workspace, which no role may start in while the marker is there, or an agent's later sparse
// clear would read as a stopped checkout: Provision removes it from a workspace CheckedOut finds
// whole, an issue pod runs Provision at every start, and a tmux launch sends a whole workspace that
// still carries the marker through Provision (HasCheckoutMarker; the daemon's
// outbox.unprovisioned) before its pane starts.
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

// HasCheckoutMarker reports whether `.jj/legion-checkout` (checkoutMarkerFile) exists as a regular
// file in dir, opened through openJJ and rolefile.OpenRegular without following a link or blocking
// on a special file. Provision asks it of a workspace with no sparse pattern, and a tmux launch of
// one CheckedOut finds whole, which still needs a provision to remove the marker when it is there.
func HasCheckoutMarker(dir string) (bool, error) {
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

// resetUnfinishedWorkspace takes away the workspace Provision found marked (checkoutMarkerFile)
// with no sparse pattern, under the store lock, so the provision adds it again from nothing
// (createWorkspace). The provision that added it stopped somewhere from its `jj workspace add` to
// its checkout's end, and no role starts in a workspace before its provision returns, nor in a
// whole one before a provision has removed a marker left there, so nothing there is anyone's work:
// the directory holds at most what a cut-short checkout wrote, and the shared clone the
// workspace's registration, its empty working-copy commit and, for an issue GitHub had no branch
// of, the bookmark that provision set on that commit. The steps are the ones Remove takes under
// the store lock, without its abandon of the workspace's own commits (a marked workspace has none
// but that empty commit, which the forget abandons) and with the deletion of the issue's bookmark
// from the working-copy commit:
//   - Where the issue's bookmark is on the workspace's working-copy commit, it is deleted, so the
//     workspace added again starts where a first provision starts: at the default branch, with the
//     bookmark on its new working-copy commit, or at origin's branch where GitHub has made one
//     since. Left there, the bookmark would start the new workspace on top of the old empty commit,
//     which has no description, so jj would refuse every push of the issue's branch. The deletion
//     goes before the rename, so a kill between them leaves a marked workspace that the next
//     provision resets again.
//   - The directory is renamed aside (removingPath), in one atomic rename. Provision cleared that
//     path before it took the store lock, so the rename finds nothing there.
//   - The workspace is forgotten in the shared clone, which abandons its working-copy commit, empty,
//     undescribed and carrying no bookmark.
//   - Its git worktree entry, which jj's forget leaves for a directory already gone, is removed.
//
// The directory renamed aside is not deleted here. That delete's time follows what the checkout
// wrote, and it touches nothing of the clone's, so Provision runs it once it has let the store lock
// go (step workspace-delete), as Remove does, and the provisions and removals of the repository's
// other issues do not wait for it. A stop anywhere after the rename and before that delete ends
// leaves the directory aside, which the next provision deletes before it takes the store lock (step
// workspace-delete too). A stop before the registration after this reset ended leaves no workspace
// at its own path, which the next provision adds again, createWorkspace forgetting it first where
// it is still registered; a stop after it leaves the new workspace marked, which the next provision
// resets again.
func resetUnfinishedWorkspace(ctx context.Context, run Runner, workspace Workspace) error {
	cloneDir, workspaceName := workspace.Clone, filepath.Base(workspace.Dir)
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
	if err := os.Rename(workspace.Dir, removingPath(workspace.Dir)); err != nil {
		return fmt.Errorf("reset workspace %s: rename it aside before removing it: %w", workspace.Dir, err)
	}
	if registered {
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "workspace", "forget", workspaceName); err != nil {
			return err
		}
	}
	return removeGitWorktree(cloneDir, workspace.Dir)
}
