//go:build linux

package launcher

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// volumeOpenFlags is every open the trimmer makes of the volume. O_NONBLOCK so that no open of
// the trimmer's can wait on what it meets — a FIFO where a cache root belongs answers at once
// here and blocks for ever without it. O_NOFOLLOW, and deliberately no O_DIRECTORY, so that a
// name that is a symbolic link is refused: on this kernel the two flags together are what makes
// a link followed rather than refused, and the fstat below is what makes the open a directory's
// (volumedir.go, where both are measured).
const volumeOpenFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK | syscall.O_CLOEXEC

// openVolumeDir opens an absolute path as a directory, one element at a time from the root of
// the filesystem: the volume root the size walk measures and the four reclaimable roots the
// sweep lists are reached this way, and nothing else of the trimmer's resolves a path at all.
//
// It is one open per element because the elements above a reclaimable root are the roles' own:
// /legion is the pod's mount point, and scratch, go, scratch/bun and scratch/bun/install under
// it are directories a role may rename and leave a link in the place of, with no race at all. A
// path resolved in one call follows that link — and so does an lstat of the same path, which is
// why the handle and the path agreed and the sweep went on to delete in the link's target
// (INFRA-2009, review 7). An element that is a link is refused here instead, naming the element.
func openVolumeDir(path string) (*dirHandle, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	// The root of the filesystem is the one name no role can be between: it is never a link and
	// never anything but a directory, and every element after it is opened from a descriptor.
	fd, err := openRetrying(func() (int, error) {
		return unix.Open("/", volumeOpenFlags|syscall.O_DIRECTORY, 0)
	})
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: "/", Err: err}
	}
	at := "/"
	for _, element := range strings.Split(strings.Trim(clean, "/"), "/") {
		if element == "" {
			break // the path is the root of the filesystem itself, which fd already holds
		}
		at = filepath.Join(at, element)
		next, err := openElementAt(fd, element, at)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return handleOf(os.NewFile(uintptr(fd), clean), clean)
}

// open is one element of the tree, opened from the descriptor this handle holds: the one read
// that is resolved by that descriptor and nothing else. A name with a separator in it, or one
// that is "." or "..", is refused rather than resolved, which is this change's own rule made a
// check instead of a convention (volumedir.go).
func (d *dirHandle) open(name string) (*dirHandle, error) {
	path := filepath.Join(d.path, name)
	fd, err := openElementAt(int(d.file.Fd()), name, path)
	if err != nil {
		return nil, err
	}
	return handleOf(os.NewFile(uintptr(fd), path), path)
}

// openElementAt opens one name in the directory parent holds open and answers the descriptor,
// which is a directory because its own fstat says so. path is what the name is called on the
// volume, which the error names.
func openElementAt(parent int, name, path string) (int, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return -1, &fs.PathError{Op: "openat", Path: path, Err: syscall.EINVAL}
	}
	fd, err := openRetrying(func() (int, error) {
		return unix.Openat(parent, name, volumeOpenFlags, 0)
	})
	if err != nil {
		return -1, &fs.PathError{Op: "openat", Path: path, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "fstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		// Asked of the descriptor and not of the open, because O_DIRECTORY is the flag this
		// kernel follows a link under, and because an open that has already happened is the
		// only reading of a name no role can move under it.
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "openat", Path: path, Err: syscall.ENOTDIR}
	}
	return fd, nil
}

// openRetrying is open again on an interrupted call, as the os package's own opens are
// (ignoringEINTR): a signal the launcher takes — the SIGCHLD of every generation it reaps — must
// not make the trimmer skip a root.
func openRetrying(open func() (int, error)) (int, error) {
	for {
		fd, err := open()
		if err != syscall.EINTR {
			return fd, err
		}
	}
}

// rootOverDescriptor is an os.Root over a descriptor already opened and checked, which is how
// the lstats and the listing of a handle are read and how the deleter child removes an entry
// (volumedir.go says why it goes through /proc/self/fd and what that costs).
func rootOverDescriptor(file *os.File) (*os.Root, error) {
	return os.OpenRoot(descriptorPath(int(file.Fd())))
}
