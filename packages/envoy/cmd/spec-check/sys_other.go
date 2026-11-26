//go:build !unix

package main

import (
	"errors"
	"io/fs"
	"os"
)

// inode is 0: print takes no lock on a system that is not Unix (tryLock), so it never writes, and
// no name it would build holds an inode.
func inode(fs.FileInfo) uint64 { return 0 }

// writable answers nil: print takes no lock on a system that is not Unix, so it never writes.
func writable(string) error { return nil }

// sameOwner answers nil: print takes no lock on a system that is not Unix, so it never writes.
func sameOwner(fileSystem, string, string, string, fs.FileInfo) error { return nil }

// tryLock refuses: print locks its directory with flock, which this system does not give, and it
// does not write without the lock (noFlock).
func tryLock(*os.File) error {
	return errors.New(noFlock)
}

// unlock does nothing: with no flock, tryLock took no lock.
func unlock(*os.File) {}
