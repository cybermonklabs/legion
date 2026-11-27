//go:build darwin

package main

import "golang.org/x/sys/unix"

// exchange puts the directory a in the place of the directory b, and b in the place of a, with one
// call: renamex_np with RENAME_SWAP. A file system that does not give the exchange refuses it, and
// then neither moves.
func (osFileSystem) exchange(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}

// moveNoReplace renames from to to, and refuses when an entry has the name to: renamex_np with
// RENAME_EXCL, since a rename of a directory over an empty directory would replace it.
func (osFileSystem) moveNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
