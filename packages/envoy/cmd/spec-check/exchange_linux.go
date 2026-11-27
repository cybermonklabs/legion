//go:build linux

package main

import "golang.org/x/sys/unix"

// exchange puts the directory a in the place of the directory b, and b in the place of a, with one
// call: renameat2 with RENAME_EXCHANGE. A file system that does not give the exchange refuses it,
// and then neither moves.
func (osFileSystem) exchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

// moveNoReplace renames from to to, and refuses when an entry has the name to: renameat2 with
// RENAME_NOREPLACE, since a rename of a directory over an empty directory would replace it.
func (osFileSystem) moveNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
