package launcher

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// The directory handles every read of the trimmer goes through, and the one rule they keep: a
// handle is opened by a single element of the tree at a time, the open carries O_NONBLOCK, and
// what it opened is a directory because its own fstat says so rather than because O_DIRECTORY
// was asked for (volumedir_linux.go holds the open itself).
//
// Both halves of that rule are this cluster's kernel. Measured in an issue pod of this
// deployment on 2026-11-12, kernel 4.19.0-gvisor, under go1.27.3:
//
//   - `openat(dirfd, "link", O_RDONLY|O_NOFOLLOW|O_DIRECTORY|O_CLOEXEC)` on a name that is a
//     symbolic link to a directory opened the link's target and listed it, where Linux answers
//     ELOOP; the same open without O_DIRECTORY answered ELOOP ("too many levels of symbolic
//     links"). os.Root opens every non-final element of a name that way (os.rootOpenDir), and
//     follows a relative link that stays lexically inside the root and then resolves that link's
//     own elements the same way. So a name with a separator in it is resolved through whatever a
//     role left on the way, and so is a single name whose link target has one.
//   - os.OpenRoot of a FIFO, Root.OpenRoot of a name that is one, and fs.ReadDir of such a name
//     through a Root's FS were each still blocked three seconds after the call, because none of
//     the three opens carries O_NONBLOCK; the same open with O_NONBLOCK answered at once, with
//     the mode the fstat refuses. A role that puts a FIFO where a cache root belongs would
//     otherwise block the trimmer's goroutine for the life of the pod, and the launcher's Run
//     waits on that goroutine at the container's stop.
//
// So no call of the trimmer's names more than one element, and every name is opened by
// openVolumeDir or dirHandle.open. What no handle can answer for is what the kernel does inside
// a call the trimmer makes once and does not drive — Root.RemoveAll re-opens the name it is
// removing with that same raw openat — and that is what the deletions' own mount namespace is
// for (volumedelete.go).
type dirHandle struct {
	// file is the descriptor the open checked. Every element below this directory is opened
	// from it, and a sweep's deletions are made through it: the deleter child is handed this
	// very descriptor, so what it removes from is the directory this handle holds and never a
	// path resolved again (volumedelete.go).
	file *os.File
	// root is an os.Root over that same descriptor, which the lstat of a name and the listing
	// are read through. It is opened through /proc/self/fd/<descriptor> — this process's own
	// descriptor table, which is not the /proc the used-path guard reads (readUsedPaths takes
	// its root as an argument) — because the os package offers no way to make a Root of a
	// descriptor already open, and Root's own openers carry the two faults above. Nothing can
	// change what that path names between the two opens: the descriptor is this process's own
	// and names the inode the fstat read, whatever has happened to the path since.
	//
	// It costs one open and one descriptor per directory the trimmer holds, which is one per
	// reclaimable root, one per Go cache shard and one per level of a walk, never one per entry.
	root *os.Root
	// path is the absolute path the handle was opened through. The trimmer's log lines are read
	// by a person, and a name under a descriptor names nothing to one, so every error raised
	// against the descriptor path is renamed to this before it is logged (named).
	path string
}

// handleOf is the handle over a descriptor already opened and checked: it takes the descriptor
// over, closing it if the Root it needs cannot be made, so a caller that gets an error holds
// nothing to close.
func handleOf(file *os.File, path string) (*dirHandle, error) {
	root, err := rootOverDescriptor(file)
	if err != nil {
		_ = file.Close()
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &dirHandle{file: file, root: root, path: path}, nil
}

// descriptorPath is one descriptor of this process in its own /proc, which is how a Root is made
// over a descriptor already open (rootOverDescriptor) and how the sweep's deleter opens the
// directories the listing passed it (removeOneRequest, volumedelete.go). It is here rather than
// beside the opens because the deleter is built for every platform this binary is, and refuses
// off Linux before it reads a request (containDeletions).
func descriptorPath(fd int) string {
	return "/proc/self/fd/" + strconv.Itoa(fd)
}

// Close closes both descriptors of the handle.
func (d *dirHandle) Close() error {
	err := d.root.Close()
	if closed := d.file.Close(); err == nil {
		err = closed
	}
	return err
}

// Lstat is one name in this directory, read through the descriptor and following no link: the
// kind, the mode and the time every guard judges by (volumeguard.go). Root.Lstat of a name with
// no separator in it is an fstatat of that descriptor and opens nothing, so it neither follows
// what the kernel would follow nor blocks on what it would block on.
func (d *dirHandle) Lstat(name string) (os.FileInfo, error) {
	info, err := d.root.Lstat(name)
	return info, d.named(err, name)
}

// ReadDir is every name in this directory with the lstat of each, read through the descriptor:
// a File opened in a Root loads each entry with an lstatat of the directory's own descriptor
// rather than with a lazy lstat of a path (os.newUnixDirent, go1.27.3), so the kind an entry
// reports is the inode this listing read. Only "." is ever named: the directory is the one the
// handle holds, and a name is opened by open below instead.
//
// The entries come back in the order their names sort, which is what fs.ReadDir answers and
// what os.ReadDir answered before the handles.
func (d *dirHandle) ReadDir() ([]os.DirEntry, error) {
	entries, err := fs.ReadDir(d.root.FS(), ".")
	return entries, d.named(err, "")
}

// named is err with the descriptor path os.Root raises it against replaced by the path this
// handle was opened through, so a line in the pod's log names a directory of the volume.
func (d *dirHandle) named(err error, name string) error {
	var path *fs.PathError
	if err != nil && errors.As(err, &path) {
		path.Path = filepath.Join(d.path, name)
	}
	return err
}
