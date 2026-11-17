//go:build unix

// packages/envoy/cmd/agent-secrets/machinecache_lock_unix.go
//
// The flocks of the machine cache (machinecache.go): the one that pairs its key.pem and
// enrollment, and the one that orders writers of its Google exchange audience. A flock is
// released by the process ending too, so a command killed while it holds one leaves no stale lock.
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/redact"
)

// lockFile takes the flock of the lock file at path, made 0600 where it is not there, exclusive
// or shared, waiting for whoever holds it, and answers the release. Two opens of one file are two
// locks, so it orders goroutines of one process as it orders processes. Every lock it takes is in
// the machine cache, so its errors name the cache by where it is (machineCacheName), never by the
// lock's path: the cache holds the machine session's key.pem.
func lockFile(path string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open a lock in %s: %w", machineCacheName, redact.File(err))
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", machineCacheName, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
