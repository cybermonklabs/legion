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

// renameReplacing renames from to to, and replaces an entry of the name to that is of its kind:
// the system's rename, where os.Rename refuses an existing directory itself (EEXIST) whatever the
// system answers, so a move that took the name as an empty directory could not rename over it.
func renameReplacing(from, to string) error {
	if err := unix.Rename(from, to); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
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
// that it takes the place of, through fsys, as far as the process may (mayGive): the owner and
// the group where it may give both, the group alone where it may give that, and else neither, so
// the entry keeps what the process gave it, as git and a plain write do. A member of the spec
// directory's group who runs print therefore leaves the group its access, and a run as a user
// over a spec file of another user writes the file. what names the entry, such as "the new
// directory", and of names the entry that old describes. A system that refuses what the process
// may give gets the way out.
func sameOwner(fsys fileSystem, path, what, of string, old fs.FileInfo) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	have, want := info.Sys().(*syscall.Stat_t), old.Sys().(*syscall.Stat_t)
	uid, gid := mayGive(int(want.Uid), int(want.Gid))
	if uid == int(have.Uid) {
		uid = -1
	}
	if gid == int(have.Gid) {
		gid = -1
	}
	if uid < 0 && gid < 0 {
		return nil
	}
	if err := fsys.lchown(path, uid, gid); err != nil {
		asked, wayOut := refusedOwner(of, uid, gid, os.Geteuid() == 0)
		return fmt.Errorf("print gives %s %s, and the system refused it (%w): %s, then run print again", what, asked, err, wayOut)
	}
	return nil
}

// refusedOwner is what print asked the system to give an entry in the place of of, the owner uid
// and the group gid with -1 for one that it did not ask for, and the way out of a refusal of it,
// which depends on whether the process is root: a run as root, which may give any owner and
// group, is told to run as the owner of of; a run as a user, which asked only for a group it
// already belongs to, is told to run as root.
func refusedOwner(of string, uid, gid int, root bool) (asked, wayOut string) {
	switch {
	case uid >= 0 && gid >= 0:
		asked = fmt.Sprintf("the owner and the group of %s, %d:%d", of, uid, gid)
	case uid >= 0:
		asked = fmt.Sprintf("the owner of %s, %d", of, uid)
	default:
		asked = fmt.Sprintf("the group of %s, %d", of, gid)
	}
	if root {
		return asked, "run print as the owner of " + of
	}
	return asked, "run print as root"
}

// mayGive is the owner and the group that this process may give an entry that it owns, of the
// owner uid and the group gid, -1 for one that it may not give: a process as root may give any;
// another may give only its own user, and any group that it belongs to, as chown(2) lets the
// owner of an entry give it a group of its own.
func mayGive(uid, gid int) (int, int) {
	if os.Geteuid() == 0 {
		return uid, gid
	}
	if uid != os.Geteuid() {
		uid = -1
	}
	if gid != os.Getegid() {
		if groups, err := os.Getgroups(); err != nil || !slices.Contains(groups, gid) {
			gid = -1
		}
	}
	return uid, gid
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
