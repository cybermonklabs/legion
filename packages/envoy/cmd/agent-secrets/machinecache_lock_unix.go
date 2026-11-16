//go:build unix

// packages/envoy/cmd/agent-secrets/machinecache_lock_unix.go
//
// The flock that pairs the machine cache's key.pem and enrollment (machinecache.go). It is
// released by the process ending too, so a command killed mid-registration leaves no stale lock.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockMachineCache takes the flock of the lock file in the machine cache dir, exclusive to
// register a session and shared to read one, waiting for whoever holds it, and answers the
// release.
func lockMachineCache(dir string, exclusive bool) (func(), error) {
	path := filepath.Join(dir, machineCacheLock)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the machine cache's lock %s: %w", path, err)
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
