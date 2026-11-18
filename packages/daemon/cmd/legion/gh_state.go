package main

import (
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

// ghOpenPrivate opens the file name in the directory dir for reading, where neither is a link and
// both are uid's alone (ghPrivate), and says whether it did. The directory is opened with
// O_NOFOLLOW and the file through openNoFollow, and each is checked by its open descriptor, so
// nothing can be swapped in between the check and the read.
func ghOpenPrivate(dir, name string, uid int) (*os.File, bool) {
	parent, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false
	}
	defer parent.Close()
	if info, err := parent.Stat(); err != nil || !info.IsDir() || !ghPrivate(info, uid) {
		return nil, false
	}
	fd, err := openNoFollow(int(parent.Fd()), name)
	if err != nil {
		return nil, false
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir, name))
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || !ghPrivate(info, uid) {
		file.Close()
		return nil, false
	}
	return file, true
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
