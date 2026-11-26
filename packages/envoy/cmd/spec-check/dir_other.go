//go:build !unix

package main

import "io/fs"

// inode is 0: print takes no lock on a system that is not Unix (tryLock), so it never writes, and
// no name it would build holds an inode.
func inode(fs.FileInfo) uint64 { return 0 }

// writable answers nil: print takes no lock on a system that is not Unix, so it never writes.
func writable(string) error { return nil }

// sameOwner answers nil: print takes no lock on a system that is not Unix, so it never writes.
func sameOwner(fileSystem, string, string, string, fs.FileInfo) error { return nil }
