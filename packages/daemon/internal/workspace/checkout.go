package workspace

import (
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
// creation time and, for an issue GitHub has no branch of, sets its bookmark. Provision keeps it
// through every provision that finds it there with no sparse pattern, until a `jj sparse reset`
// into the workspace succeeds and Provision removes it. While it exists beside an empty
// sparse-pattern set, the next provision knows the last checkout Legion started, or was about to
// start, was cut short, so it clears every entry of the workspace except `.jj` and `.git` before
// running `jj sparse reset` again, after finishing any unfinished registration (creation record
// and bookmark) in finishUnfinishedRegistration. Without the marker, a workspace whose sparse
// patterns an agent cleared keeps its untracked files.
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

// clearWorkingCopy removes every entry of dir except `.jj` and `.git`, in the process and without
// following a link anywhere it walks (rolefile.OpenDirectory and rolefile.RemoveTree). Nothing
// outside dir is touched.
func clearWorkingCopy(dir string) error {
	fd, err := rolefile.OpenDirectory(unix.AT_FDCWD, dir, dir)
	if err != nil {
		return fmt.Errorf("clear workspace %s: %w", dir, err)
	}
	listing := os.NewFile(uintptr(fd), dir)
	names, err := listing.Readdirnames(0)
	closeErr := listing.Close()
	if err != nil {
		return fmt.Errorf("clear workspace %s: %w", dir, err)
	}
	if closeErr != nil {
		return fmt.Errorf("clear workspace %s: %w", dir, closeErr)
	}
	for _, name := range names {
		if name == ".jj" || name == ".git" {
			continue
		}
		if err := rolefile.RemoveTree(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("clear workspace %s: %w", dir, err)
		}
	}
	return nil
}
