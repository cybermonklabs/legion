//go:build !unix

package main

import (
	"errors"
	"os"
)

// tryLock refuses: print locks its directory with flock, which this system does not give, and it
// does not write without the lock (noFlock).
func tryLock(*os.File) error {
	return errors.New(noFlock)
}

// unlock does nothing: with no flock, tryLock took no lock.
func unlock(*os.File) {}
