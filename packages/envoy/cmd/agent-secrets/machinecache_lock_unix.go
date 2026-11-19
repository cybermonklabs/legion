//go:build unix

// packages/envoy/cmd/agent-secrets/machinecache_lock_unix.go
//
// The flocks of the machine cache (machinecache.go): the one that pairs its key.pem and
// enrollment, and the one that orders writers of its Google exchange audience. A flock is
// released by the process ending too, so a command killed while it holds one leaves no stale lock.
package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes the flock of the lock file at path, made 0600 where it is not there, exclusive
// or shared, waiting for whoever holds it, and answers the release. Two opens of one file are two
// locks, so it orders goroutines of one process as it orders processes.
func lockFile(path string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the lock %s: %w", path, err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// tryLockShared takes the shared flock of the lock file at path, made 0600 where it is not there,
// only where nobody holds it exclusive now, and answers its release; held is false, and nothing
// is taken, where a writer holds it. It never waits, for a caller that must not wait on whatever
// the writer is doing (heldSigner).
func tryLockShared(path string) (release func(), held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open the lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, true, nil
}
