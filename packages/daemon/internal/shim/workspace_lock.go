package shim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The two locks the shim takes in the workspace its role works in. The adoption's commands run
// with --ignore-working-copy (runAdoption), so they take no lock of jj's: jj takes its
// working-copy lock only to snapshot the working copy or to write it. A jj command that snapshots
// this same workspace could then run beside the adoption, and one that loaded the repository
// before the adoption's change and commits after it commits a sibling operation, which the next
// load merges with the change's: @'s change is then divergent, @ is one of its commits, and the
// working copy records the snapshot's tree.
//
// The workspace lock is the shim's own (lockWorkspace): the adoption and the workspace read each
// hold it from before their first jj command until their last one has ended, so neither starts
// while the other runs, in this role's shim or in another role's, since every role of an issue
// works the one workspace. That closes the daemon's own trigger: the workspace read snapshots,
// and the daemon sends it with no order against an adoption.
//
// jj's working-copy lock is jj's (awaitWorkingCopy): the adoption takes it as jj does and lets it
// go at once, before its first jj command and again before the check after its change. jj holds
// that lock from the start of a snapshot until it has committed the snapshot's operation and
// written the working copy's new state, so a snapshot that holds it when the adoption asks has
// committed by the time the adoption gets it: the adoption then loads after it, or its check reads
// its operation. In a workspace that shares its tree with git, as an issue pod's does, jj also
// holds its git_import_export.lock across a snapshot, and the adoption's change takes that lock to
// write git's refs, so a change that loaded the repository before such a snapshot ends only after
// the snapshot has; there the wait before the check matters where the snapshot loaded the
// repository before the change and took its locks after it (jj 0.46.0-sami.20261127-111037,
// measured on 2026-11-28).

// workspaceLock is the file of the workspace lock, in the workspace's .jj directory: jj passes over
// a file there that it does not know, and git ignores .jj.
const workspaceLock = "legion-workspace.lock"

// workingCopyLock is jj's working-copy lock, under the workspace's directory: the working_copy.lock
// of the working copy's state directory, which jj takes around every snapshot and every write of
// the working copy.
var workingCopyLock = filepath.Join(".jj", "working_copy", "working_copy.lock")

// lockPoll is how long a wait for either lock sleeps between two tries.
const lockPoll = 10 * time.Millisecond

// takeLock takes the exclusive flock of the file at path as jj takes its own locks (FileLock::lock
// in lib/src/lock/unix.rs at jj v0.46.0-sami.20261127-111037): it creates the file or opens the
// one there, locks it, and tries again where the file it locked has no link left, which is a lock
// file its holder removed between the open and the lock. Each try is non-blocking, and a refused
// one is tried again every lockPoll until ctx ends: the caller's budget, built on the shim's
// command context, so the shim's own end ends the wait too. waiting runs once, at the first
// refused try. release lets the lock go as jj does (FileLock's Drop): it removes the file and then
// unlocks it, so a jj command blocked on that file finds it unlinked and takes a new one.
//
// A directory that does not exist holds no lock to take: the workspace is no jj workspace, or no
// working copy of jj's, and every jj command the caller runs there fails on that itself. takeLock
// then answers a release that does nothing.
func takeLock(ctx context.Context, path string, waiting func()) (release func(), err error) {
	refused := false
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if errors.Is(err, fs.ErrNotExist) {
			return func() {}, nil
		}
		if err != nil {
			return nil, err
		}
		fd := int(file.Fd())
		switch err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); {
		case err == nil:
			var stat syscall.Stat_t
			if err := syscall.Fstat(fd, &stat); err != nil {
				_ = file.Close()
				return nil, fmt.Errorf("fstat %s: %w", path, err)
			}
			if stat.Nlink == 0 {
				_ = file.Close()
				continue
			}
			return func() {
				_ = os.Remove(path)
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR):
			_ = file.Close()
		default:
			_ = file.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		if !refused {
			refused = true
			waiting()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// remaining is how long ctx has left before its deadline.
func remaining(ctx context.Context) time.Duration {
	deadline, _ := ctx.Deadline()
	return time.Until(deadline).Round(time.Millisecond)
}

// lockWorkspace takes the workspace lock for one frame, what ("adoption", "workspace read"), and
// answers the release the frame calls once its last jj command has ended. A frame that finds the
// lock held writes one line to the shim's log as it starts to wait. The wait is bounded by the
// frame's budget (opened.ctx), and one that the budget ends refuses the frame with a reason that
// names the lock and the wait, before the frame has run any jj command: the daemon sends a refused
// adoption again at its next sweep, and reads a refused workspace read as a read that failed.
func (s *shim) lockWorkspace(opened openedWorkspace, what string) (func(), error) {
	path := filepath.Join(opened.dir, ".jj", workspaceLock)
	began := time.Now()
	release, err := takeLock(opened.ctx, path, func() {
		s.log.Printf("[worker-shim] the %s waits for the workspace lock %s, which another adoption or workspace read of this workspace holds, in this role's shim or another role's: it waits at most %s",
			what, path, remaining(opened.ctx))
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("worker-shim: the workspace lock %s was still held when the %s had waited %s for it, the end of its budget of %s: another adoption or workspace read of this workspace held it all that time; the %s ran no jj command",
			path, what, time.Since(began).Round(time.Millisecond), opened.budget, what)
	case errors.Is(err, context.Canceled):
		return nil, fmt.Errorf("worker-shim: the shim ended while the %s waited for the workspace lock %s", what, path)
	case err != nil:
		return nil, fmt.Errorf("worker-shim: take the workspace lock %s for the %s: %w", path, what, err)
	}
	return release, nil
}

// awaitWorkingCopy waits until jj's working-copy lock of the workspace is free: it takes the lock
// and lets it go at once (takeLock), so a jj command that held it when the adoption asked has
// committed its operation and written the working copy by the time this returns. when says where
// in the adoption the wait is, for the one line a wait that finds the lock held writes to the
// shim's log. The wait is bounded by the adoption's budget, and one that the budget ends refuses
// the adoption with a reason that names the lock and the wait.
func (s *shim) awaitWorkingCopy(opened openedWorkspace, when string) error {
	path := filepath.Join(opened.dir, workingCopyLock)
	began := time.Now()
	release, err := takeLock(opened.ctx, path, func() {
		s.log.Printf("[worker-shim] the adoption waits for jj's working-copy lock %s, which a jj command that snapshots the workspace holds, %s: it waits at most %s",
			path, when, remaining(opened.ctx))
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("worker-shim: jj's working-copy lock %s was still held when the adoption had waited %s for it, %s, the end of its budget of %s: a jj command that snapshots the workspace held it all that time",
			path, time.Since(began).Round(time.Millisecond), when, opened.budget)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("worker-shim: the shim ended while the adoption waited for jj's working-copy lock %s, %s", path, when)
	case err != nil:
		return fmt.Errorf("worker-shim: take jj's working-copy lock %s, %s: %w", path, when, err)
	}
	release()
	return nil
}
