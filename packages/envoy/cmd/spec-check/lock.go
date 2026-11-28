package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// errLockHeld is tryLock's answer when another open file of the directory holds its lock.
var errLockHeld = errors.New("the lock is held")

// lockPoll is how often lockSpecDir tries the lock again while another run holds it.
const lockPoll = 20 * time.Millisecond

// specDirLock is the hold of one run of print on its directory: the open directory, whose flock is
// the lock, its inode, each directory that the run made to write into, the deepest first, each
// other open directory whose flock the run holds (the new directory that writeDir builds, which
// takes the place of the directory), and fsys, through which the run makes each change.
type specDirLock struct {
	dir  *os.File
	ino  uint64
	made []string
	also []*os.File
	fsys fileSystem
}

// lockSpecDir takes the lock of print on dir: an exclusive flock on the directory itself, which
// the kernel releases when the process ends, so a run that is killed leaves no lock. Git keeps no
// empty directory, so print writes the first spec file of a repository into a directory that it
// makes: lockSpecDir makes dir and each missing directory above it first. When another run holds
// the lock, it tries again every lockPoll until wait ends, then refuses. A run puts a new
// directory in the place of dir (writeDir), and another can have removed dir and made it again,
// so the lock counts only when the open directory is still the directory that dir names; a run
// holds the flock of its new directory too from before the exchange, so a run that opens dir after
// the exchange waits for it. Each directory that it makes or removes, and each flock, goes
// through fsys. A run that opened dir before the exchange takes the flock of the directory that
// the exchange moved out of the place of dir, finds that dir names another directory, and, while
// its wait has not ended, opens dir again, so it waits for the flock of the directory that dir
// names.
func lockSpecDir(fsys fileSystem, dir string, wait time.Duration) (*specDirLock, error) {
	deadline := time.Now().Add(wait)
	for {
		made, err := makeDirs(fsys, dir)
		if err != nil {
			return nil, err
		}
		f, err := os.Open(dir)
		if err != nil {
			removeDirs(fsys, made)
			return nil, err
		}
		for err = fsys.lock(f); errors.Is(err, errLockHeld) && time.Now().Before(deadline); err = fsys.lock(f) {
			time.Sleep(lockPoll)
		}
		if err != nil {
			f.Close()
			if errors.Is(err, errLockHeld) {
				return nil, fmt.Errorf("another run of print holds the lock of the directory, and this run waited %s for it; run print again when that run ends", wait)
			}
			removeDirs(fsys, made)
			return nil, fmt.Errorf("cannot lock the directory: %w", err)
		}
		held, err := f.Stat()
		if err != nil {
			unlock(f)
			f.Close()
			return nil, err
		}
		if now, err := os.Stat(dir); err == nil && os.SameFile(held, now) {
			return &specDirLock{dir: f, ino: inode(held), made: made, fsys: fsys}, nil
		}
		unlock(f)
		f.Close()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another run of print removed and made the directory again while this run waited %s for its lock; run print again", wait)
		}
	}
}

// hold adds f, an open directory whose flock the run takes, to the hold, so that release closes it.
func (l *specDirLock) hold(f *os.File) {
	l.also = append(l.also, f)
}

// release ends the hold. When the run wrote nothing (kept is false), it first removes each
// directory that it made and that is still empty, so a run that print refuses leaves no directory.
// It ends each flock before it closes the descriptor: a child that the process forked and that has
// not started its program holds a copy of the descriptor, which shares the flock, so the close of
// this one alone would leave the lock held and refuse the next run.
func (l *specDirLock) release(kept bool) {
	if !kept {
		removeDirs(l.fsys, l.made)
	}
	for _, f := range l.also {
		unlock(f)
		f.Close()
	}
	unlock(l.dir)
	l.dir.Close()
}

// makeDirs makes dir and each directory above it that does not exist, and answers the ones that it
// made, the deepest first. A directory that another process makes at the same time is not one of
// them.
func makeDirs(fsys fileSystem, dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var made []string
	for i := len(missing) - 1; i >= 0; i-- {
		switch err := fsys.mkdir(missing[i], 0o755); {
		case err == nil:
			made = append([]string{missing[i]}, made...)
		case !errors.Is(err, fs.ErrExist):
			removeDirs(fsys, made)
			return nil, err
		}
	}
	return made, nil
}

// removeDirs removes each directory of dirs, the deepest first, while each is empty.
func removeDirs(fsys fileSystem, dirs []string) {
	for _, d := range dirs {
		if fsys.remove(d) != nil {
			return
		}
	}
}
