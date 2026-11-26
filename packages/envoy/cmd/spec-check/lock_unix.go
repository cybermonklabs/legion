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

// unlock ends the flock of the open file f. A child that the process forks holds a copy of f's
// descriptor until it starts its program, and the copy shares the flock, so closing f alone would
// leave the lock held for that while.
func unlock(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
