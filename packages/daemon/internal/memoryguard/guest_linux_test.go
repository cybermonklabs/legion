//go:build linux

package memoryguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// On this machine's real /proc, one scan raises a real child of the test process to 1000, a write
// to another process's oom_score_adj that needs no privilege, while the test process, which the
// classifier names the role's agent, keeps its score. The classifier names no other process, so the
// scan writes no score but these two's.
func TestGuestRaisesARealChild(t *testing.T) {
	self := filepath.Join("/proc", "self", "oom_score_adj")
	start, err := readScore(self, make([]byte, 64))
	if err != nil {
		t.Skipf("no readable %s: %v", self, err)
	}
	if start >= raisedScore {
		t.Skipf("the test process runs at oom_score_adj %d, so its child starts at the score a raise would set", start)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	childScore := filepath.Join("/proc", strconv.Itoa(pid), "oom_score_adj")
	if before, err := readScore(childScore, make([]byte, 64)); err != nil || before != start {
		t.Fatalf("the child %d reads oom_score_adj %d (%v) before any scan, want its parent's %d", pid, before, err, start)
	}

	h := newGuestHarness(t, "/proc", newFakeCgroup(t, cgroupV2).dir, map[int]Class{
		os.Getpid(): {Role: "implementer", Protected: true},
		pid:         {Role: "implementer"},
	}).run()
	if got, err := readScore(childScore, make([]byte, 64)); err != nil || got != raisedScore {
		t.Fatalf("the child %d reads oom_score_adj %d (%v) after one scan, want %d; the log:\n%s", pid, got, err, raisedScore, h.logged())
	}
	if got, err := readScore(self, make([]byte, 64)); err != nil || got != start {
		t.Fatalf("the test process reads oom_score_adj %d (%v) after one scan, want its starting %d", got, err, start)
	}
	if failed := h.lines("score failed"); len(failed) != 0 {
		t.Fatalf("the watch logged %q, want no failed score write", failed)
	}
}
