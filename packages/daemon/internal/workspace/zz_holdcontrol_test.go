package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// CONTROL (never merged): holdOnCPU0 starts hogs busy loops pinned to CPU 0, the machine's own
// load and no process of the runner's, and ends them once path exists, or at the test's end. It
// answers the path of a file it writes once every busy loop has been reaped: the hold is over
// then, and a row that passes its budget at that file, not at path, stops a command the hold no
// longer delays. A busy loop is a runnable process on CPU 0 until it dies, and it dies of its
// SIGKILL only once CPU 0 runs it, so the loops are all signalled before any is waited for.
func holdOnCPU0(t *testing.T, hogs int, path string) (ended string) {
	t.Helper()
	ended = filepath.Join(filepath.Dir(path), "hold-ended")
	var procs []*exec.Cmd
	for range hogs {
		cmd := exec.Command("taskset", "-c", "0", "sh", "-c", "while :; do :; done")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start a hog: %v", err)
		}
		procs = append(procs, cmd)
	}
	var once sync.Once
	end := func() {
		once.Do(func() {
			for _, cmd := range procs {
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
			for _, cmd := range procs {
				_ = cmd.Wait()
			}
			if err := os.WriteFile(ended, nil, 0o600); err != nil {
				t.Errorf("record that the hold is over: %v", err)
			}
		})
	}
	t.Cleanup(end)
	go func() {
		testwait.Holds(time.Minute, testwait.Poll, func() bool { _, err := os.Stat(path); return err == nil })
		end()
	}()
	return ended
}
