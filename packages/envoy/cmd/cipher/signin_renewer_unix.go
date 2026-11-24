//go:build unix

// packages/envoy/cmd/cipher/signin_renewer_unix.go
//
// The two calls of the sign-in renewer (signin_renewer.go) that need the platform: the flock that
// keeps one renewer for each sign-in, and the session of its own a renewer runs in. A flock is
// released by the process ending too, so a renewer that is killed leaves no stale lock.
package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// renewalSupported is whether this platform keeps a sign-in renewed: it has the flock.
const renewalSupported = true

// tryRenewerLock takes the exclusive flock of the lock file at path without waiting, and answers
// the release, or held where another process holds it. create makes the file, 0600, where it is
// not there; only a command creates it, never the renewer (keptSignIn.prepare).
func tryRenewerLock(path string, create bool) (release func(), held bool, err error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, false, nil
}

// detach starts cmd in a session of its own, so a hang-up of the caller's terminal and a signal
// sent to the caller's process group do not reach the renewer.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
