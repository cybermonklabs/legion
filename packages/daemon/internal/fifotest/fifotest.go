// Package fifotest tells a test whether code it runs waits in a plain open of a fifo: the open
// without O_NONBLOCK that a fifo's reader makes and that waits for a writer for as long as none
// comes. A row whose defect is such an open reads this from the kernel instead of timing the code
// under test, so a correct tree that is only slow is never failed for it.
package fifotest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Watch watches for a plain open of the fifo at Path. Its zero value with Path set is ready.
type Watch struct {
	Path string
	// seen says the last call found a thread of this process asleep in a plain open.
	seen bool
}

// Opened answers, once per poll of the caller's, whether code of this process waits in a plain
// open of the fifo at Path. It answers true once a thread of this process has been asleep in an
// openat without O_NONBLOCK (/proc/self/task/<tid>/syscall) on two calls in a row and the fifo
// then has a reader, which an open for writing that does not wait reports by succeeding. Each
// alone is no answer: an open of any other file can sleep in the kernel for a moment, and code
// that opens the fifo with O_NONBLOCK, as a correct check of it does, is its reader without waiting.
// A plain open of the fifo stays asleep until a writer comes, so two calls in a row see it.
//
// When it answers true, its own open for writing has been the writer that open needed, so the
// code under test goes on. It answers an error when it could read no thread's record: then the
// caller has no answer, not a clean one.
func (w *Watch) Opened() (bool, error) {
	asleep, err := plainOpenAsleep()
	if err != nil {
		return false, err
	}
	if !asleep {
		w.seen = false
		return false, nil
	}
	if !w.seen {
		w.seen = true
		return false, nil
	}
	if info, err := os.Lstat(w.Path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return false, nil
	}
	fd, err := syscall.Open(w.Path, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		// ENXIO: the fifo has no reader, so the plain open asleep was of another file.
		return false, nil
	}
	_ = syscall.Close(fd)
	return true, nil
}

// plainOpenAsleep says whether a thread of this process is asleep in an openat whose flags carry
// no O_NONBLOCK. The kernel writes a thread's record as the call's number and its arguments in
// hex, the third argument being openat's flags, while the thread sleeps in a call, and "running"
// otherwise.
func plainOpenAsleep() (bool, error) {
	const tasks = "/proc/self/task"
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return false, err
	}
	read := 0
	for _, entry := range entries {
		record, err := os.ReadFile(filepath.Join(tasks, entry.Name(), "syscall"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // the thread has ended since the listing
		}
		if err != nil {
			return false, err
		}
		read++
		fields := strings.Fields(string(record))
		if len(fields) < 4 {
			continue
		}
		if number, err := strconv.ParseInt(fields[0], 10, 64); err != nil || number != syscall.SYS_OPENAT {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(fields[3], "0x"), 16, 64)
		if err == nil && flags&syscall.O_NONBLOCK == 0 {
			return true, nil
		}
	}
	if read == 0 {
		return false, errors.New("fifotest: read no thread's record under " + tasks)
	}
	return false, nil
}
