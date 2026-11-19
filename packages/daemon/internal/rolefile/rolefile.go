// Package rolefile opens, reads and removes what a role can write, for a caller that holds
// something others wait on: the tmux daemon's outbox under a repository's store lock, or a pod's
// launcher, which is its container's PID 1. Such a file can be anything the role made it: a
// symlink to /dev/zero, which a reader that follows it and reads to the end holds whole in memory
// until the process is killed, a fifo, whose plain open waits for a writer for as long as none
// comes, a device, a socket, or a regular file of any size. OpenRegular never follows a link and
// never waits, and refuses anything but a regular file in words naming it; ReadBounded refuses a
// file larger than its caller's bound rather than read on; RemoveTree follows no link and waits on
// nothing anywhere it walks.
package rolefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Read is the content of the regular file at path, read bounded to limit bytes, or why it is no
// file a reader may take: OpenRegular's refusals, ReadBounded's, a missing file (open's own error,
// for which errors.Is(err, fs.ErrNotExist) holds), or any other failure to read it.
func Read(path string, limit int) ([]byte, error) {
	file, err := OpenRegular(unix.AT_FDCWD, path, path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ReadBounded(file, path, limit)
}

// OpenRegular opens name under the directory dirfd (unix.AT_FDCWD for a path) with flag and,
// when flag creates it, perm, and answers it only when the descriptor it opened is a regular
// file; path is name as the errors print it. It never follows a link at name (O_NOFOLLOW, which
// answers ELOOP), never waits on a fifo or a device (O_NONBLOCK, which a regular file ignores),
// never takes a terminal (O_NOCTTY), and refuses each in words naming the file — a symlink, a
// socket (which open answers with ENXIO), a directory (EISDIR, for a flag that writes), or
// whatever else the descriptor is (Kind). Any other failure is open's own error, as os.OpenFile
// words it.
func OpenRegular(dirfd int, name, path string, flag int, perm uint32) (*os.File, error) {
	fd, err := Openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, perm)
	switch {
	case errors.Is(err, unix.ELOOP):
		return nil, fmt.Errorf("%s is a symlink", path)
	case errors.Is(err, unix.ENXIO):
		return nil, fmt.Errorf("%s is a socket", path)
	case errors.Is(err, unix.EISDIR):
		return nil, fmt.Errorf("%s is a directory", path)
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%s is a %s", path, Kind(info.Mode()))
	}
	return file, nil
}

// ReadBounded reads file, which OpenRegular opened as path, from where it stands to its end, and
// refuses it rather than read on once it holds more than limit bytes: a file a role wrote can be
// any size, and a reader that holds it whole in memory is one a role can end.
func ReadBounded(file *os.File, path string, limit int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return body, nil
}

// Openat is unix.Openat, asked again when a signal interrupted it, as os.OpenFile asks.
func Openat(dirfd int, name string, flag int, perm uint32) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flag, perm)
		if !errors.Is(err, unix.EINTR) {
			return fd, err
		}
	}
}

// directoryOpen is the open OpenDirectory makes, Openat outside tests. It is a seam for one
// purpose: the kernel Legion's pods run, gVisor, has releases that follow a link under
// O_DIRECTORY|O_NOFOLLOW, which Linux never does, so a removal that asked for O_DIRECTORY (as
// os.RemoveAll does) passes every test on Linux and still deletes what a link names in a pod.
// TestRemoveTreeFollowsNoLinkOnAKernelThatWould puts such a kernel here, so that removal fails on
// Linux too.
var directoryOpen = Openat

// OpenDirectory is a descriptor of the directory name under dirfd (unix.AT_FDCWD for a path),
// path being name as the errors print it, for a caller that writes or removes under it: a symlink
// at name is refused in words rather than followed (O_NOFOLLOW, which answers ELOOP), and anything
// at name but a directory, a fifo or a device among them, is opened without a wait (O_NONBLOCK,
// O_NOCTTY) and refused by what its descriptor is, an error errors.Is matches to ENOTDIR.
// O_DIRECTORY is deliberately not asked for: gVisor, the kernel this tree's pods run under, has
// releases that follow a link at name when O_DIRECTORY and O_NOFOLLOW are both set (measured: the
// open succeeds through the link under the pods' gVisor in 2026-11 and under runsc 20260112;
// runsc 20261109 refuses it), where O_NOFOLLOW alone answers ELOOP under every one.
func OpenDirectory(dirfd int, name, path string) (int, error) {
	fd, err := directoryOpen(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP):
		return -1, fmt.Errorf("%s is a symlink", path)
	case errors.Is(err, unix.ENXIO):
		return -1, fmt.Errorf("%s is a socket", path)
	case err != nil:
		return -1, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, &fs.PathError{Op: "fstat", Path: path, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, notDirectory(path)
	}
	return fd, nil
}

// notDirectory is OpenDirectory's refusal of what is no directory: its words name the path, and
// errors.Is matches it to ENOTDIR, as the kernel's own answer would be.
type notDirectory string

func (path notDirectory) Error() string { return string(path) + " is no directory" }

func (notDirectory) Unwrap() error { return unix.ENOTDIR }

// RemoveTree removes path and everything under it, as os.RemoveAll does, and like it answers nil
// for a path that is not there (unless path's parent is a symlink, which it refuses as it refuses
// every link it meets), but it follows no link and waits on nothing anywhere it walks. A caller
// whose parent is the operator's own directory, which may be a symlink, resolves it once before it
// makes path there (the provisioning credential directory, under the tmux daemon's state directory
// or a pod's TMPDIR). Every path it is handed is under a directory a role can write, or swap: the
// shared clone's parent (an incomplete clone and its temporary sibling) and its .git/worktrees, a
// workspace's own parent (Remove's aside), the provisioning credential directory's resolved
// parent, the pod's feed and the directory beside it (a foreign git directory, a staging clone
// and what a failed transfer left), the leak-check install's parent (its staging directory and
// what it moved aside), a role's private directory (the pod's launcher clearing it before a
// generation starts, and a generation's directory under it), or the role's homes directory, which
// role-init clears (its stage, its overlay work directories, its config home and that copy's
// .partial). os.RemoveAll fails both rules there. When path is no file it can unlink outright, it
// opens path's parent with a plain open(2) (os/removeall_at.go), and a plain open of a fifo waits
// for a writer for as long as none comes, under whatever its caller holds. And it opens each
// directory below path with O_DIRECTORY and O_NOFOLLOW, which a gVisor release can follow a link
// through (OpenDirectory), so a link a role plants below path, under a directory the removal
// cannot unlink it from, has PID 1 delete what it points at. So every directory here, path's
// parent included, is opened as OpenDirectory opens one, the walk goes on only from a descriptor
// whose fstat says directory, and every entry is removed by unlinkat relative to its directory's
// descriptor, which removes a link and never what it names. A parent that is no directory, a fifo
// among them, answers ENOTDIR at once. Opening the parent needs read on it, where os.RemoveAll
// needs only write and search: a parent its caller cannot read is refused (`open <parent>:
// permission denied`), even for a path that is not there. Every caller's parent is made 0700 or
// 0755 by the daemon, workspace-init, a role's shim, the install or role-init, or is a role's
// private directory, which the pod mounts as drwxrwxrwt, so its owner can read it, and only a role
// that changed the mode of its own parent meets this; the removal then fails rather than removing.
func RemoveTree(path string) error {
	clean := filepath.Clean(path)
	parentPath, name := filepath.Dir(clean), filepath.Base(clean)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return &fs.PathError{Op: "RemoveTree", Path: path, Err: unix.EINVAL}
	}
	parent, err := OpenDirectory(unix.AT_FDCWD, parentPath, parentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return removeAt(parent, parentPath, name)
}

// removeAt removes name under the directory parent, dir being parent as the errors print it, as
// os/removeall_at.go's removeAllFrom does, with OpenDirectory in place of its openDirAt: name is
// unlinked outright when it can be, a link or anything else that is no directory among it, and is
// opened and walked only when that fails and OpenDirectory says it is a directory. Each pass over
// the directory reads at most a batch of names, removes them, and opens the directory again, since
// removing entries can make a read already under way skip some. An entry's path is joined only
// once its unlink fails, the one case that prints it or walks under it.
func removeAt(parent int, dir, name string) error {
	unlinkErr := unlinkat(parent, name, 0)
	if unlinkErr == nil || errors.Is(unlinkErr, unix.ENOENT) {
		return nil
	}
	path := filepath.Join(dir, name)
	// EISDIR: a directory, whose entries go first. EPERM or EACCES: no write to parent, but name
	// may still be a directory whose entries can go. Anything else is the answer.
	if !errors.Is(unlinkErr, unix.EISDIR) && !errors.Is(unlinkErr, unix.EPERM) && !errors.Is(unlinkErr, unix.EACCES) {
		return &fs.PathError{Op: "unlinkat", Path: path, Err: unlinkErr}
	}
	var walkErr error
	for {
		const batch = 1024
		fd, err := OpenDirectory(parent, name, path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			var openErr *fs.PathError
			if !errors.As(err, &openErr) || errors.Is(err, unix.ENOTDIR) || !isDirectoryAt(parent, name) {
				// No directory, a link among them, or a file it may not read (mode 0000): never
				// walked, and the unlink's own answer stands.
				return &fs.PathError{Op: "unlinkat", Path: path, Err: unlinkErr}
			}
			// A directory this process cannot open (no read permission on it, no descriptor
			// left): its entries stay, and as removeAllFrom does, the removal of the directory
			// itself, which takes an empty one, is still tried, with the open's error kept.
			if walkErr == nil {
				walkErr = err
			}
			break
		}
		listing := os.NewFile(uintptr(fd), path)
		read := 0
		for {
			names, readErr := listing.Readdirnames(batch)
			if readErr != nil && readErr != io.EOF {
				_ = listing.Close()
				if errors.Is(readErr, fs.ErrNotExist) {
					return nil
				}
				return &fs.PathError{Op: "readdirnames", Path: path, Err: readErr}
			}
			read = len(names)
			failed := 0
			for _, entry := range names {
				if err := removeAt(fd, path, entry); err != nil {
					failed++
					if walkErr == nil {
						walkErr = err
					}
				}
			}
			// A batch of which any entry went: read afresh. One of which none went: read on past it.
			if failed != batch {
				break
			}
		}
		_ = listing.Close()
		if read < batch {
			break
		}
	}
	removeErr := unlinkat(parent, name, unix.AT_REMOVEDIR)
	if removeErr == nil || errors.Is(removeErr, unix.ENOENT) {
		return nil
	}
	if walkErr != nil {
		return walkErr
	}
	return &fs.PathError{Op: "unlinkat", Path: path, Err: removeErr}
}

// unlinkat is unix.Unlinkat, asked again when a signal interrupted it.
func unlinkat(dirfd int, name string, flags int) error {
	for {
		err := unix.Unlinkat(dirfd, name, flags)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// isDirectoryAt is whether name under the directory dirfd is a directory, read without following
// a link at name. A name it cannot read is taken as no directory. removeAt asks it only once
// OpenDirectory failed, to tell a directory it may not open from a file it may not read.
func isDirectoryAt(dirfd int, name string) bool {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return stat.Mode&unix.S_IFMT == unix.S_IFDIR
}

// Kind names a file's kind as OpenRegular's refusals print it.
func Kind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	case mode&fs.ModeNamedPipe != 0:
		return "fifo"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}
