package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// The stores `legion gh` keeps under the role's $XDG_STATE_HOME/legion-gh, its call log
// (appendGhCallLog) and its copies of gh's help (ghHelpKeep), make their directories, open their
// files and write them through these helpers, so both hold one rule: a directory that is no link,
// and a file that is no link, each owned by this process's user with no permission for its group
// or for others (ghPrivate), and a file written whole or not at all (ghReplaceFile).

// ghStateDir makes dir with mode 0700 where it is missing and says whether it is one this user may
// write a store into: a directory, not a link to one, owned by uid, which it then sets to mode 0700.
func ghStateDir(dir string, uid int) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !ghOwnedBy(info, uid) {
		return false
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return false
		}
	}
	return true
}

// ghOpenDir and ghOpenAt are ghOpenPrivate's opens of its directory and of the file in it. A test
// replaces them with opens that follow a link.
var (
	ghOpenDir = func(dir string) (*os.File, error) {
		return os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	}
	ghOpenAt = openNoFollow
)

// ghOpenPrivate opens the file name in the directory dir for reading, where neither is a link and
// both are uid's alone (ghPrivate), and says whether it did. The directory is opened with
// O_NOFOLLOW and the file through openNoFollow, and each is checked by its open descriptor, so
// nothing can be swapped in between the check and the read.
//
// Neither open is trusted to refuse a link. Under gVisor, the kernel every Legion pod runs, an
// open of a link to a directory with O_DIRECTORY|O_NOFOLLOW opens the directory the link names,
// where Linux answers ELOOP (measured on 2026-11-18 under gVisor 4.19.0). So after each open,
// ghOpenedAs reads the path with Lstat, and the open is refused unless the path holds the same
// file as the descriptor and that file is no link.
func ghOpenPrivate(dir, name string, uid int) (*os.File, bool) {
	parent, err := ghOpenDir(dir)
	if err != nil {
		return nil, false
	}
	defer parent.Close()
	if !ghOpenedAs(parent, dir, fs.ModeDir, uid) {
		return nil, false
	}
	fd, err := ghOpenAt(int(parent.Fd()), name)
	if err != nil {
		return nil, false
	}
	path := filepath.Join(dir, name)
	file := os.NewFile(uintptr(fd), path)
	if !ghOpenedAs(file, path, 0, uid) {
		file.Close()
		return nil, false
	}
	return file, true
}

// ghOpenedAs says whether file, just opened from path, is uid's alone (ghPrivate) and of the type
// kind (fs.ModeDir for a directory, 0 for a regular file), and whether Lstat of path finds the same
// file: of that type too, so not a link, with the descriptor's device and inode (os.SameFile).
func ghOpenedAs(file *os.File, path string, kind fs.FileMode, uid int) bool {
	opened, err := file.Stat()
	if err != nil || opened.Mode().Type() != kind || !ghPrivate(opened, uid) {
		return false
	}
	found, err := os.Lstat(path)
	return err == nil && found.Mode().Type() == kind && os.SameFile(opened, found)
}

// ghReplaceFile writes data as the file at path: into a file of mode 0600 made beside it, whose
// name starts with prefix, renamed over it, so a reader finds the old file or the new one whole.
// A file that cannot be written leaves the old one.
func ghReplaceFile(path, prefix string, data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(path), prefix)
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), path)
}

// ghPrivate says whether info is uid's and no one else's: owned by that user, with no permission
// for its group or for others.
func ghPrivate(info os.FileInfo, uid int) bool {
	return ghOwnedBy(info, uid) && info.Mode().Perm()&0o077 == 0
}

// ghOwnedBy says whether uid owns info.
func ghOwnedBy(info os.FileInfo, uid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid
}
