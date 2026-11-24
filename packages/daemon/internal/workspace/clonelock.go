package workspace

import (
	"errors"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// CloneLock is the lock of the shared clone at clone across processes: an exclusive flock of the
// clone's own git directory (<clone>/.git). Each process that adds a working copy to the clone or
// writes its store holds it for that work: the tmux daemon's provisions and removals of every issue
// of the repository (its store lock takes this one after its own), and a pane's or a pod's shim
// adding the working copy of a task (ProvisionTask). Two of them that registered workspaces at one
// time would each rewrite the clone's operation heads and its git worktree entries under the
// other. A flock belongs to the open file it was taken through, so two holders in one process
// exclude each other as two processes do, and the kernel lets it go when its holder exits, so a
// killed process leaves nothing held.
//
// It locks the directory, never a file of its own inside it: a file there would be one more entry
// a tree agent can write, and one that Lock would have to create, which can fail. The directory is
// there exactly when the clone is. A clone with no git directory yet is one no other process can
// reach, since every working copy of it is made after it: the daemon makes it under its store lock,
// and no shim runs before its issue's working copy is there. So Lock holds no flock for it, and the
// process that makes the clone is alone with it. A git directory that cannot be opened as one (a
// file or a link in its place) holds no flock either: no git or jj command can use such a clone,
// and the first that runs under the lock reports why. Nor does a flock the kernel refuses (ENOLCK,
// its lock records spent), which sync.Locker has no way to report.
func CloneLock(clone string) *CloneLocker {
	return &CloneLocker{gitDir: filepath.Join(clone, ".git"), fd: -1}
}

// CloneLocker is CloneLock's lock: its in-process mutex orders the users of this one value, and fd
// is the descriptor its flock was taken through while it is held, or -1.
type CloneLocker struct {
	gitDir string
	mu     sync.Mutex
	fd     int
}

// Lock waits for the clone's lock and takes it.
func (l *CloneLocker) Lock() {
	l.mu.Lock()
	l.fd = l.flock(unix.LOCK_EX)
}

// TryLock takes the clone's lock when no one holds it, and reports whether it did, waiting for
// nothing.
func (l *CloneLocker) TryLock() bool {
	if !l.mu.TryLock() {
		return false
	}
	fd := l.flock(unix.LOCK_EX | unix.LOCK_NB)
	if fd == heldElsewhere {
		l.mu.Unlock()
		return false
	}
	l.fd = fd
	return true
}

// heldElsewhere is flock's answer to a lock that waits for nothing when another holder has it.
const heldElsewhere = -2

// flock opens the git directory and flocks it with how, and returns the descriptor it holds the
// flock through: -1 when it holds none (CloneLock says when), and heldElsewhere when how waits for
// nothing and another holder has the flock.
func (l *CloneLocker) flock(how int) int {
	fd, err := unix.Open(l.gitDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1
	}
	for {
		err = unix.Flock(fd, how)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err == nil {
		return fd
	}
	_ = unix.Close(fd)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return heldElsewhere
	}
	return -1
}

// Unlock closes the descriptor the flock was taken through, which lets the flock go.
func (l *CloneLocker) Unlock() {
	if l.fd >= 0 {
		_ = unix.Close(l.fd)
		l.fd = -1
	}
	l.mu.Unlock()
}
