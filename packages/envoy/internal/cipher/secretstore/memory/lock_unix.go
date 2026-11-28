//go:build unix

package memory

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// fileLockMissing is nil: this system has flock, the lock between processes the writer takes.
func fileLockMissing() error { return nil }

// lockWaitMax is the longest wait between two tries of a lock another writer holds.
const lockWaitMax = 50 * time.Millisecond

// lockFile takes an exclusive flock on path, made if absent, and answers its release: every
// writer of one memory secrets file, in this process or another, takes it around its write. While
// another writer holds it, it tries again after a wait that doubles up to lockWaitMax, until ctx
// ends, and then answers ctx's error having taken nothing, so a write whose time runs out while
// it waits writes nothing.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for wait := time.Millisecond; ; wait = min(2*wait, lockWaitMax) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
