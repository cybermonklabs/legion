package workspace

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// CONTROL (never merged): holdOnCPU0 starts hogs busy loops pinned to CPU 0, the machine's own
// load and no process of the runner's, and ends them once path exists, or at the test's end.
func holdOnCPU0(t *testing.T, hogs int, path string) {
	t.Helper()
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
				_ = cmd.Wait()
			}
		})
	}
	t.Cleanup(end)
	go func() {
		testwait.Holds(time.Minute, testwait.Poll, func() bool { _, err := os.Stat(path); return err == nil })
		end()
	}()
}
