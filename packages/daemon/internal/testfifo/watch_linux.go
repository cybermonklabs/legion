//go:build linux

package testfifo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// tasks and record are where the kernel keeps each thread's record. They are variables for
// the watch's own test, which points record at a name no thread has to stand for a kernel that
// keeps none.
var (
	tasks  = "/proc/self/task"
	record = "syscall"
)

// Opened answers, once per poll of the caller's, whether code of this process waits in a plain
// open of the fifo at Path. It answers true once a thread of this process has been asleep in an
// openat without O_NONBLOCK on two calls in a row and the fifo then has a reader, which an open
// for writing that does not wait reports by succeeding. Each alone is no answer: an open of any
// other file can sleep in the kernel for a moment, and code that opens the fifo with O_NONBLOCK,
// as a correct check of it does, is its reader without waiting. A plain open of the fifo stays
// asleep until a writer comes, so two calls in a row see it.
//
// When it answers true, its own open for writing has been the writer that open needed, so the
// code under test goes on. It answers ErrUnsupported where the kernel keeps no record, and
// another error when it could not read one: then the caller has no answer, not a clean one.
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

// supported answers nil where the kernel keeps each thread's record and ErrUnsupported where it
// keeps none.
func supported() error {
	_, err := plainOpenAsleep()
	return err
}

// plainOpenAsleep says whether a thread of this process is asleep in an openat whose flags carry
// no O_NONBLOCK. The kernel writes a thread's record as the call's number and its arguments in
// hex, the third argument being openat's flags, while the thread sleeps in a call, and "running"
// otherwise. The thread that reads its own record is running, so a kernel that keeps records
// always gives this at least one; one that gives none keeps none, which is ErrUnsupported.
func plainOpenAsleep() (bool, error) {
	entries, err := os.ReadDir(tasks)
	if errors.Is(err, fs.ErrNotExist) {
		return false, ErrUnsupported
	}
	if err != nil {
		return false, err
	}
	read := 0
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(tasks, entry.Name(), record))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue // no record: the kernel keeps none, or the thread has ended since the listing
		}
		if err != nil {
			return false, err
		}
		read++
		fields := strings.Fields(string(data))
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
		return false, ErrUnsupported
	}
	return false, nil
}
