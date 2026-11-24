package memoryguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// holdInGroup starts a shell in a process group of its own that holds 32 MiB of anonymous memory
// and, where sleeper is set, a sleep it started beside it in that group, and returns once the
// memory is held.
func holdInGroup(t *testing.T, sleeper bool) *exec.Cmd {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	script := `held=$(head -c 33554432 /dev/zero | tr '\0' x); : > "$0"; sleep 60`
	if sleeper {
		script = `held=$(head -c 33554432 /dev/zero | tr '\0' x); sleep 60 & : > "$0"; wait`
	}
	holder := exec.Command("sh", "-c", script, ready)
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-holder.Process.Pid, syscall.SIGKILL)
		_ = holder.Wait()
	})
	testwait.Eventually(t, "the shell to hold its 32 MiB", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	return holder
}

// TestGroupReaderReadsTheGroupAlone: the reader sums the members of one process group, a child
// of the leader included, by the guard's size of each, and counts no process of another group. A
// shell holding 32 MiB in a group of its own, with a sleep beside it, reads 32 to 64 MiB over
// two processes while a second such shell holds 32 MiB in a group of its own: a read that
// counted that one too would read over 64 MiB. A second read reads the same, so the reader keeps
// nothing of one read in the next, and the other group reads its own 32 MiB.
func TestGroupReaderReadsTheGroupAlone(t *testing.T) {
	if _, err := os.Stat("/proc/self/smaps"); err != nil {
		t.Skipf("no /proc/self/smaps: %v", err)
	}
	read := holdInGroup(t, true)
	other := holdInGroup(t, false)
	r := NewGroupReader("/proc")
	defer r.Close()

	for _, pass := range []string{"first", "second"} {
		size, processes, err := r.Read(read.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if size < 32<<20 || size >= 64<<20 || processes != 2 {
			t.Fatalf("the %s read of the group read %d bytes over %d processes, want 32 to 64 MiB over the shell and its sleep alone", pass, size, processes)
		}
	}
	if size, processes, err := r.Read(other.Process.Pid); err != nil || size < 32<<20 || size >= 64<<20 || processes == 0 {
		t.Fatalf("the other group read %d bytes over %d processes (%v), want 32 to 64 MiB over its own processes", size, processes, err)
	}
}

// TestStatGroup: the process group is the third field after the command name's last `)`, whatever
// that name holds.
func TestStatGroup(t *testing.T) {
	for _, tc := range []struct {
		stat  string
		state byte
		pgrp  int
		ok    bool
	}{
		{"42 (node) S 7 4242 4242 0 -1", 'S', 4242, true},
		{"42 (x) S 1 2 3 (y) R 7 99 99 0", 'R', 99, true},
		{"42 (omp daemon brok) Z 7 12 12", 'Z', 12, true},
		{"42 (node) S 7", 0, 0, false},
		{"42 node S 7 4242", 0, 0, false},
		{"42 (node) S 7 -1 4242", 0, 0, false},
	} {
		state, pgrp, ok := statGroup([]byte(tc.stat))
		if state != tc.state || pgrp != tc.pgrp || ok != tc.ok {
			t.Errorf("statGroup(%q) = %q, %d, %v, want %q, %d, %v", tc.stat, state, pgrp, ok, tc.state, tc.pgrp, tc.ok)
		}
	}
}
