//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"golang.org/x/sys/unix"
)

// inode is the inode of the entry that info describes.
func inode(info fs.FileInfo) uint64 {
	return uint64(info.Sys().(*syscall.Stat_t).Ino)
}

// writable answers nil when this process may write in the directory dir and in its parent, where
// print builds the directory that takes the place of dir (stagingPath), and else the refusal. The
// refusal of the parent says what to change: write permission on the parent, or, for a dir that is
// a mount point (on another device than its parent), the way out of a mount point, which a parent
// that the process may write would meet next, when the system refuses the exchange.
func writable(dir string) error {
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return &fs.PathError{Op: "write", Path: dir, Err: err}
	}
	parent := filepath.Dir(dir)
	err := unix.Access(parent, unix.W_OK)
	if err == nil {
		return nil
	}
	wayOut := "give this process write permission on " + parent + ", or run print as a user who may write there, then run print again"
	var at, above unix.Stat_t
	if unix.Stat(dir, &at) == nil && unix.Stat(parent, &above) == nil && at.Dev != above.Dev {
		wayOut = dir + " is a mount point, which print cannot put another directory in the place of: " + mountPointWayOut
	}
	return fmt.Errorf("print builds the directory that takes the place of %s beside it, in %s, and this process may not write there (%w): %s",
		dir, parent, &fs.PathError{Op: "write", Path: parent, Err: err}, wayOut)
}

// sameOwner gives the entry path, which print made, the owner and the group of old, the entry
// that it takes the place of, through fsys, where they are others and the process may give them
// (mayGive). Else the entry keeps the owner and the group that the process gave it, as git and a
// plain write do, so a run as a user over a spec file of another user writes the file. what names
// the entry, such as "the new directory", and of names the entry that old describes. A system that
// refuses an owner that the process may give gets the way out.
func sameOwner(fsys fileSystem, path, what, of string, old fs.FileInfo) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	have, want := info.Sys().(*syscall.Stat_t), old.Sys().(*syscall.Stat_t)
	if have.Uid == want.Uid && have.Gid == want.Gid || !mayGive(int(want.Uid), int(want.Gid)) {
		return nil
	}
	if err := fsys.lchown(path, int(want.Uid), int(want.Gid)); err != nil {
		return fmt.Errorf("print gives %s the owner and the group of %s, %d:%d, and the system refused it (%w): run print as the owner of %s or as root, then run print again",
			what, of, want.Uid, want.Gid, err, of)
	}
	return nil
}

// mayGive reports whether this process may give an entry that it owns the owner uid and the group
// gid: a process as root may give any; another may give only its own user, with one of its
// groups, its effective group or a supplementary one.
func mayGive(uid, gid int) bool {
	if os.Geteuid() == 0 {
		return true
	}
	if uid != os.Geteuid() {
		return false
	}
	if gid == os.Getegid() {
		return true
	}
	groups, err := os.Getgroups()
	return err == nil && slices.Contains(groups, gid)
}

// tryLock takes the exclusive flock of the open file f without waiting, or answers errLockHeld
// when another open file of the same file holds it.
func tryLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLockHeld
	}
	return err
}

// unlock ends the flock of the open file f. A child that the process forks holds a copy of f's
// descriptor until it starts its program, and the copy shares the flock, so closing f alone would
// leave the lock held for that while.
func unlock(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
