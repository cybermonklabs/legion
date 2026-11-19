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

// directoryOpen is the open OpenDirectory makes, Openat outside tests. It is a seam with two uses.
// The kernel Legion's pods run, gVisor, has releases that follow a link under
// O_DIRECTORY|O_NOFOLLOW, which Linux never does, so a removal that asked for O_DIRECTORY (as
// os.RemoveAll does) passes every test on Linux and still deletes what a link names in a pod:
// TestRemoveTreeFollowsNoLinkOnAKernelThatWould puts such a kernel here, so that removal fails on
// Linux too. And TestRemoveTreeRemovesNothingAbovePath refuses every open here, so that a lost
// guard on path's last element cannot have the test walk the machine's root.
var directoryOpen = Openat

// OpenDirectory is a descriptor of the directory name under dirfd (unix.AT_FDCWD for a path),
// path being name as the errors print it, for a caller that writes or removes under it: a symlink
// at name is refused in words rather than followed (O_NOFOLLOW, which answers ELOOP), a socket,
// which open answers with ENXIO, is refused as "<path> is a socket", and anything else at name but
// a directory, a fifo or a device among them, is opened without a wait (O_NONBLOCK, O_NOCTTY) and
// refused by what its descriptor is, an error errors.Is matches to ENOTDIR.
// O_DIRECTORY is deliberately not asked for: gVisor, the kernel this tree's pods run under, has
// releases that follow a link at name when O_DIRECTORY and O_NOFOLLOW are both set (measured: the
// open succeeds through the link under the pods' gVisor in 2026-11 and under runsc 20260112;
// runsc 20261109 refuses it), where O_NOFOLLOW alone answers ELOOP under every one.
func OpenDirectory(dirfd int, name, path string) (int, error) {
	fd, op, err := openDirectory(dirfd, name)
	switch {
	case err == nil:
		return fd, nil
	case op == "":
		return -1, notDirectory(path)
	case op == "open" && errors.Is(err, unix.ELOOP):
		return -1, fmt.Errorf("%s is a symlink", path)
	case op == "open" && errors.Is(err, unix.ENXIO):
		return -1, fmt.Errorf("%s is a socket", path)
	}
	return -1, &fs.PathError{Op: op, Path: path, Err: err}
}

// openDirectory is OpenDirectory's open and its check, with no path to print: it answers the
// descriptor, or the call that failed ("open" or "fstat") and its own error, or, for what it opened
// and found no directory, no call and ENOTDIR. removeAt opens each directory of its walk through it
// and makes words of a refusal only for the one error RemoveTree answers.
func openDirectory(dirfd int, name string) (int, string, error) {
	fd, err := directoryOpen(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "open", err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, "fstat", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, "", unix.ENOTDIR
	}
	return fd, "", nil
}

// notDirectory is OpenDirectory's refusal of what is no directory: its words name the path, and
// errors.Is matches it to ENOTDIR, as the kernel's own answer would be.
type notDirectory string

func (path notDirectory) Error() string { return string(path) + " is no directory" }

func (notDirectory) Unwrap() error { return unix.ENOTDIR }

// RemoveTree removes path and everything under it, as os.RemoveAll does save for a ".." in path
// (below), but follows no link and waits on nothing anywhere it walks, for a caller that removes
// what a role can write or swap.
//
//   - A link at path or anywhere below it is unlinked, or the unlink's error answered where the
//     walk may not write its directory, and what it names is never opened.
//   - path's parent is cleaned by text (filepath.Clean) and then opened as OpenDirectory opens a
//     directory: a parent that is a link is refused, however path spells it, and one that is no
//     directory is refused at once, without a wait (a fifo or a file answers ENOTDIR, a socket
//     "<parent> is a socket"). The clean takes off repeated and trailing separators and ".", and a
//     ".." takes off the element before it by text, never through what a link there names; so a
//     path through a missing entry, a file or a link before ".." names the folded directory, where
//     os.RemoveAll would go through the kernel's answer for it. No caller passes "..". A caller
//     whose parent is an operator's directory that may be a link resolves it first.
//   - Opening the parent needs read on it: a parent its caller cannot read is refused (`open
//     <parent>: permission denied`), even for a path that is not there.
//   - A path that is not there answers nil, and so does "", as os.RemoveAll answers. A path whose
//     last element is "." or ".." answers EINVAL, and nothing above path is removed.
//   - What the walk holds grows with a tree's size and not with the square of its depth: each level
//     holds its own name, and an entry's whole path is built only for the error RemoveTree answers.
//
// It is not os.RemoveAll because os.RemoveAll fails both rules where a role writes. When path is
// no file it can unlink outright, os.RemoveAll opens path's parent with a plain open(2)
// (os/removeall_at.go), and a plain open of a fifo waits for a writer for as long as none comes,
// under whatever its caller holds. And it opens each directory below path with O_DIRECTORY and
// O_NOFOLLOW, which a gVisor release can follow a link through (OpenDirectory), so a link a role
// plants below path, under a directory the removal cannot unlink it from, has PID 1 delete what it
// points at. So every directory here is opened as OpenDirectory opens one, the walk goes on only
// from a descriptor whose fstat says directory, and every entry is removed by unlinkat relative to
// its directory's descriptor, which removes a link and never what it names.
func RemoveTree(path string) error {
	if path == "" {
		return nil
	}
	name := filepath.Base(path)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return &fs.PathError{Op: "RemoveTree", Path: path, Err: unix.EINVAL}
	}
	// The last element is read before the clean, which would fold "." or ".." into a name above.
	parentPath := filepath.Dir(filepath.Clean(path))
	parent, err := OpenDirectory(unix.AT_FDCWD, parentPath, parentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if err := removeAt(parent, &node{name: parentPath}, name); err != nil {
		return &fs.PathError{Op: err.op, Path: err.at.path(), Err: err.err}
	}
	return nil
}

// node is the walk's root, path's cleaned parent, or an entry whose unlink failed: a directory the
// walk opens, or the entry an error names. Each holds its own name and a pointer to its parent's
// node, so a chain of directories a role nests costs the walk its names and not their whole paths,
// which grow with the square of the chain's depth.
type node struct {
	parent *node
	name   string
}

// path is the node's whole path, its ancestors' names joined, built for an error RemoveTree answers.
func (n *node) path() string {
	depth := 0
	for at := n; at != nil; at = at.parent {
		depth++
	}
	names := make([]string, depth)
	for at := n; at != nil; at = at.parent {
		depth--
		names[depth] = at.name
	}
	return filepath.Join(names...)
}

// walkError is a failure of the walk at the node at: the call that failed and its own error. Its
// path is built only when RemoveTree answers it as an *fs.PathError.
type walkError struct {
	op  string
	at  *node
	err error
}

// removeAt removes name under the directory parent, dir being parent's node, as
// os/removeall_at.go's removeAllFrom does, with openDirectory in place of its openDirAt: name is
// unlinked outright when it can be, a link or anything else that is no directory among it, and is
// opened and walked only when that fails and it is a directory. Each pass over the directory reads
// at most a batch of names, removes them, and opens the directory again, since removing entries
// can make a read already under way skip some. It answers the first failure it went on past, an
// entry it could not remove or a directory it could not open, unless the directory's own removal
// then succeeds (an empty directory it could not open), which answers nil; a failed read of a
// directory's names ends that walk and is answered at once. The entry's own node is made only once
// its unlink fails: most entries go at once.
func removeAt(parent int, dir *node, name string) *walkError {
	unlinkErr := unlinkat(parent, name, 0)
	if unlinkErr == nil || errors.Is(unlinkErr, unix.ENOENT) {
		return nil
	}
	at := &node{parent: dir, name: name}
	// EISDIR: a directory, whose entries go first. EPERM or EACCES: no write to parent, but name
	// may still be a directory whose entries can go. Anything else is the answer.
	if !errors.Is(unlinkErr, unix.EISDIR) && !errors.Is(unlinkErr, unix.EPERM) && !errors.Is(unlinkErr, unix.EACCES) {
		return &walkError{op: "unlinkat", at: at, err: unlinkErr}
	}
	var walkErr *walkError
	for {
		const batch = 1024
		fd, op, err := openDirectory(parent, at.name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			if !isDirectoryAt(parent, at.name) {
				// No directory, a link among them, or a file it may not read (mode 0000): never
				// walked, and the unlink's own answer stands.
				return &walkError{op: "unlinkat", at: at, err: unlinkErr}
			}
			// A directory this process cannot open (no read permission on it, no descriptor
			// left): its entries stay, and as removeAllFrom does, the removal of the directory
			// itself, which takes an empty one, is still tried, with the open's error kept.
			if op == "" {
				op = "open"
			}
			if walkErr == nil {
				walkErr = &walkError{op: op, at: at, err: err}
			}
			break
		}
		listing := os.NewFile(uintptr(fd), at.name)
		read := 0
		for {
			names, readErr := listing.Readdirnames(batch)
			if readErr != nil && readErr != io.EOF {
				_ = listing.Close()
				if errors.Is(readErr, fs.ErrNotExist) {
					return nil
				}
				var named *fs.PathError
				if errors.As(readErr, &named) {
					readErr = named.Err
				}
				return &walkError{op: "readdirnames", at: at, err: readErr}
			}
			read = len(names)
			failed := 0
			for _, entry := range names {
				if err := removeAt(fd, at, entry); err != nil {
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
	removeErr := unlinkat(parent, at.name, unix.AT_REMOVEDIR)
	if removeErr == nil || errors.Is(removeErr, unix.ENOENT) {
		return nil
	}
	if walkErr != nil {
		return walkErr
	}
	return &walkError{op: "unlinkat", at: at, err: removeErr}
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
// openDirectory failed, to tell a directory it may not open from a file it may not read.
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
