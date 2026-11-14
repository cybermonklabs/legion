//go:build linux

package launcher

import (
	"errors"
	"fmt"
	"io"
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
	if !isOneName(name) {
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

// removeEntryAt removes name from the directory dir holds open, whatever it holds, and follows no
// symbolic link on the way: it is the removal the sweep's deleter makes for every entry it is
// asked for (removeOneRequest, volumedelete.go), in place of Root.RemoveAll, which opens every
// directory it recurses into with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a
// link under (volumedir.go).
//
// It works bottom up through descriptors, as Rust's std::fs::remove_dir_all does since
// CVE-2022-21658. Every name is first unlinked as the name it is, which removes a file, a link or
// a pipe and opens nothing. A name the kernel will not unlink that way is opened as one element of
// the directory it is in (openElementAt: O_NOFOLLOW and no O_DIRECTORY, a directory because the
// descriptor's own fstat says so), its names are read through that descriptor and removed the
// same way, and it is removed last with unlinkat(AT_REMOVEDIR) from the descriptor of the
// directory it is in. A name that became a link or a file between the unlink and the open is
// refused by the open (ELOOP, or the fstat's ENOTDIR) and unlinked again as what it is now. A
// directory renamed away while its names are removed is emptied where it went, through the
// descriptor already open on it, and the link left in its place is refused by the last
// unlinkat, which answers ENOTDIR and leaves the link standing: raced so in a test process with
// no containment, 80 trials of 80, on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3
// (TestTheDeletersRemovalFollowsNoLinkWithNoContainment).
//
// The error names what it failed on relative to dir, so a line in the pod's log is the entry's
// path followed by a name under it. names is the buffer every directory's names are read into,
// which the deleter makes once for all its removals (VolumeTrimDelete): a directory's names are
// all read before any of them is removed, so one buffer serves the whole tree.
func removeEntryAt(dir *os.File, name string, names []byte) error {
	removal := treeRemoval{names: names}
	return removal.remove(int(dir.Fd()), name)
}

// treeRemoval is one removeEntryAt: the buffer every directory's names are read into.
type treeRemoval struct {
	names []byte
}

// remove removes name from the directory parent holds open. It is given the name alone and
// never the path above it: an error names what it failed on by its path from parent, built on
// the way back up a name at a time (under), so that no level of a deep tree holds a copy of the
// whole path above it while the levels below it are removed. Measured in a test process on
// 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3, on a chain 1500 directories deep of
// 250-byte names: the heap in use rose 289 MB over the removal with the path carried down to
// every level, and 1.5 MB with the name alone.
func (r *treeRemoval) remove(parent int, name string) error {
	if !isOneName(name) {
		return &fs.PathError{Op: "unlinkat", Path: name, Err: syscall.EINVAL}
	}
	if err := unlinkRetrying(parent, name, 0); err == nil || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	fd, err := openElementAt(parent, name, name)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return nil
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		// Not a directory now, whatever the unlink met: unlinked as the name it is, or
		// refused with the reason the kernel gives for that.
		if err := unlinkRetrying(parent, name, 0); err != nil && !errors.Is(err, syscall.ENOENT) {
			return &fs.PathError{Op: "unlinkat", Path: name, Err: err}
		}
		return nil
	case err != nil:
		return err
	}
	emptied := r.empty(fd)
	_ = unix.Close(fd)
	if emptied != nil {
		return under(name, emptied)
	}
	if err := unlinkRetrying(parent, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, syscall.ENOENT) {
		return &fs.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

// under is err, raised below the directory name, renamed to its path from the directory name is
// in. An error that names the directory itself carries no path of its own.
func under(name string, err error) error {
	failed, ok := err.(*fs.PathError)
	if !ok {
		return err
	}
	path := name
	if failed.Path != "" {
		path = name + "/" + failed.Path
	}
	return &fs.PathError{Op: failed.Op, Path: path, Err: failed.Err}
}

// volumeRemovalPasses is how many times empty removes the names it reads from one directory: the
// first pass, and one more for the names created while the first ran.
const volumeRemovalPasses = 2

// errStillFilling is a directory whose names were still appearing once empty had made all its
// passes over it: something is creating names in it as fast as they are removed. The sweep
// passes such an entry over as a removal that failed part way (trimmer.remove).
var errStillFilling = fmt.Errorf("names were still appearing in it after %d passes over its names", volumeRemovalPasses)

// empty removes every name of the directory fd holds open. It reads the names again after each
// pass, for what was created while it removed, and stops at a pass that removed nothing, with the
// first error of that pass. It makes volumeRemovalPasses passes at most: a directory that still
// has names after them is answered with errStillFilling rather than read again, since a writer
// that keeps creating names would otherwise hold the removal, and the sweep waiting on it, for as
// long as it writes. Its errors name what they failed on by its path from this directory, and
// the directory itself by no path at all (under).
func (r *treeRemoval) empty(fd int) error {
	for pass := 1; ; pass++ {
		names, err := r.read(fd)
		if err != nil {
			return &fs.PathError{Op: "getdents", Err: err}
		}
		if len(names) == 0 {
			return nil
		}
		if pass > volumeRemovalPasses {
			return &fs.PathError{Op: "getdents", Err: errStillFilling}
		}
		var first error
		removed := false
		for _, name := range names {
			if err := r.remove(fd, name); err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			removed = true
		}
		if !removed {
			return first
		}
	}
}

// read is every name of the directory fd holds open but "." and "..", read from its start
// through that descriptor.
func (r *treeRemoval) read(fd int) ([]string, error) {
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return nil, err
	}
	var names []string
	for {
		n, err := unix.Getdents(fd, r.names)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(r.names[:n], -1, names)
	}
}

// unlinkRetrying is unlinkat again on an interrupted call, as openRetrying is open again.
func unlinkRetrying(parent int, name string, flags int) error {
	for {
		if err := unix.Unlinkat(parent, name, flags); err != syscall.EINTR {
			return err
		}
	}
}

// rootOverDescriptor is an os.Root over a descriptor already opened and checked, which is how
// the lstats and the listing of a handle are read (volumedir.go says why it goes through
// /proc/self/fd and what that costs).
func rootOverDescriptor(file *os.File) (*os.Root, error) {
	return os.OpenRoot(descriptorPath(int(file.Fd())))
}
