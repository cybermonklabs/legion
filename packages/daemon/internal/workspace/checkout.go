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

// checkoutMarkerFile is the marker Provision writes inside a workspace's `.jj` directory just
// before `jj sparse reset` into a workspace that holds no file Legion did not write (one
// createWorkspace registered in this provision, or one whose marker is already there), and removes
// once that checkout succeeds. While it exists beside an empty sparse-pattern set, the next
// provision knows the last checkout Legion started was cut short, so it clears every entry of the
// workspace except `.jj` and `.git` before running `jj sparse reset` again. Without the marker, a
// workspace whose sparse patterns an agent cleared keeps its untracked files.
const checkoutMarkerFile = "legion-checkout"

// writeCheckoutMarker creates or truncates `.jj/legion-checkout` in dir through openJJ and openIn,
// following no link and refusing any non-regular entry there.
func writeCheckoutMarker(dir string) error {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	file, err := openIn(jj, checkoutMarkerFile, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NONBLOCK|unix.O_NOCTTY, 0o644)
	if err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return fmt.Errorf("the checkout marker %s is not a regular file (%s)", filepath.Join(jj.Name(), checkoutMarkerFile), info.Mode().Type())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write checkout marker of workspace %s: %w", dir, err)
	}
	return nil
}

// hasCheckoutMarker reports whether `.jj/legion-checkout` exists as a regular file in dir, opened
// through openJJ and openIn without following a link or blocking on a special file.
func hasCheckoutMarker(dir string) (bool, error) {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return false, fmt.Errorf("read checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	file, err := openIn(jj, checkoutMarkerFile, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read checkout marker of workspace %s: %w", dir, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("read checkout marker of workspace %s: %w", dir, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("the checkout marker %s is not a regular file (%s)", filepath.Join(jj.Name(), checkoutMarkerFile), info.Mode().Type())
	}
	return true, nil
}

// removeCheckoutMarker removes `.jj/legion-checkout` in dir through openJJ and unlinkat, following
// no link. A missing marker is no error.
func removeCheckoutMarker(dir string) error {
	jj, err := openJJ(dir, nil)
	if err != nil {
		return fmt.Errorf("remove checkout marker of workspace %s: %w", dir, err)
	}
	defer jj.Close()
	for {
		err = unix.Unlinkat(int(jj.Fd()), checkoutMarkerFile, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil && !errors.Is(err, unix.ENOENT) {
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
