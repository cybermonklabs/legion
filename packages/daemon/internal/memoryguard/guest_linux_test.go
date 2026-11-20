//go:build linux

package memoryguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// On a /proc of the test's own whose entries are links into this machine's /proc, the test process
// and a real child of it, one scan raises that child to 1000, a write to another process's
// oom_score_adj that needs no privilege, while the test process, which the classifier names the
// role's agent, keeps its score. The scan lists these two alone, so its writes depend on no
// process the test did not start.
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

	proc := t.TempDir()
	for name, target := range map[string]string{
		strconv.Itoa(os.Getpid()): filepath.Join("/proc", strconv.Itoa(os.Getpid())),
		strconv.Itoa(pid):         filepath.Join("/proc", strconv.Itoa(pid)),
		"self":                    filepath.Join("/proc", strconv.Itoa(os.Getpid())),
		"meminfo":                 "/proc/meminfo",
	} {
		if err := os.Symlink(target, filepath.Join(proc, name)); err != nil {
			t.Fatal(err)
		}
	}
	h := newGuestHarness(t, proc, newFakeCgroup(t, cgroupV2).dir, map[int]Class{
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
