//go:build unix

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes the exclusive flock of the open file f without waiting, or answers errLockHeld
// when another open file of the same file holds it.
func tryLock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLockHeld
	}
	return err
}
