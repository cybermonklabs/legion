package testkernel

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// ProcessEnded is whether the process pid is gone, or has exited and waits only to be reaped: its
// state, the third field of /proc/<pid>/stat after the command in parentheses, reads Z. A child a
// command left behind is reaped by whatever it was reparented to, not by the run, and kill(pid, 0)
// answers nil for it until then. A process kill still finds is never read as ended from a /proc
// entry that cannot be read: the test fails there, naming /proc, since it has no other way to ask.
// kill is asked again after such a read, because a process that went between the two leaves no
// entry, and has ended. It calls t.Fatalf, so it must be called from the goroutine running the
// test.
func ProcessEnded(t testing.TB, pid int) bool {
	t.Helper()
	if pid <= 0 {
		return false
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	body, err := os.ReadFile(stat)
	if err != nil {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		t.Fatalf("%s cannot be read (%v) for process %d, which kill still finds: this row reads from /proc whether a process that answers kill has exited", stat, err, pid)
	}
	_, after, ok := strings.Cut(string(body), ") ")
	return ok && strings.HasPrefix(after, "Z")
}
