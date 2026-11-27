//go:build unix

package cmdtest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// makingDirPrefix begins the name a process's directory of builds has while it is being made and
// locked, which no sweep reads (removeBuildsOfEndedProcesses reads processDirPrefix's names alone).
const makingDirPrefix = ".cmdtest-making-"

// makeProcessDir makes this process's directory of builds, named processDirPrefix, the pid and a
// dash, and holds a flock on it until release: the kernel drops a flock when the process holding it
// ends, however it ends, go test's alarm too. A sweep that can take the lock knows the directory's
// process has ended; one that cannot knows it runs, in whatever PID namespace, since a lock is the
// directory's own and the pid in its name is read nowhere. The directory is made and locked under a
// name no sweep reads, then renamed, so no sweep finds it unlocked. A process ended between the
// make and the rename leaves a directory no sweep removes.
func makeProcessDir() (dir string, release func(), err error) {
	made, err := os.MkdirTemp("", makingDirPrefix)
	if err != nil {
		return "", nil, err
	}
	lock, err := os.Open(made)
	if err == nil {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err != nil {
			lock.Close()
		}
	}
	if err != nil {
		_ = os.Remove(made)
		return "", nil, fmt.Errorf("lock %s: %w", made, err)
	}
	dir = filepath.Join(filepath.Dir(made), processDirPrefix+strconv.Itoa(os.Getpid())+"-"+strings.TrimPrefix(filepath.Base(made), makingDirPrefix))
	if err := os.Rename(made, dir); err != nil {
		lock.Close()
		_ = os.Remove(made)
		return "", nil, err
	}
	return dir, func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
	}, nil
}

// removeBuildsOfEndedProcesses removes each directory of builds (processDirPrefix) in the
// temporary directory that this user owns, whose lock no process holds (makeProcessDir), and
// whose name's pid runs in no process of this PID namespace: its process has ended. It holds the
// lock while it removes the directory. A directory whose lock a process holds is kept, whatever
// pid its name carries: its process runs, in whatever PID namespace. So is one whose pid runs
// here, even with its lock free: this branch's own cmdtest from before the lock makes its directory
// with no lock, and a sweep must not remove it while its process builds into it. A directory of
// #5123's cmdtest, which names it os.MkdirTemp("", "cmdtest-"), that is cmdtest-<digits> with no
// dash after the digits, is never read: the name check (processOfBuildsDir) refuses it. A
// directory it cannot open or remove is kept too: it is someone else's to remove, or the next
// run's.
func removeBuildsOfEndedProcesses() {
	root := os.TempDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, ok := processOfBuildsDir(entry.Name())
		if !ok || !entry.IsDir() || processRuns(pid) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
			continue
		}
		removeIfEnded(filepath.Join(root, entry.Name()))
	}
}

// removeIfEnded removes the directory of builds at path where its lock can be taken, holding it
// while it removes. A lock it cannot take (another process holds it, EWOULDBLOCK) is that
// process's, which runs.
func removeIfEnded(path string) {
	lock, err := os.Open(path)
	if err != nil {
		return
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return
	}
	_ = os.RemoveAll(path)
}

// processOfBuildsDir is the pid a directory of builds named name was made by, and whether name is
// such a directory's name: processDirPrefix, the pid, a dash.
func processOfBuildsDir(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, processDirPrefix)
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	return pid, err == nil && pid > 0
}

// processRuns reports whether a process of pid runs in this PID namespace: one of another user,
// which may not be signalled, runs too.
func processRuns(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
