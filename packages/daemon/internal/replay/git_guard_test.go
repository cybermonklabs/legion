package replay

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// processGone says whether the process pid has ended: /proc holds it no more, or holds it a zombie
// no one has reaped yet, which holds no memory, or answers its read with ESRCH, which the kernel
// gives while it releases a process whose reaper has just reaped it: such a process has ended too.
func processGone(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	end := strings.LastIndexByte(string(stat), ')')
	return err == nil && end >= 0 && strings.HasPrefix(strings.TrimSpace(string(stat[end+1:])), "Z")
}

// The guard kills the whole process group of a command whose memory passes its cap, as its reader
// of a group's memory reads it: here a reader that reads 2 GiB of the command's group once the
// command's shell has started a child of its own, so the test needs no real memory. The shell and
// the child it left in the background are both ended long before the child's minute of sleep, the
// run is a memory-cap kill that did not exit by itself, its peak is the memory read, and the reader
// was asked of the group the shell leads. It fails where the guard kills the shell alone, reads
// another group, or lets the command run on.
func TestTheGitGuardKillsAProcessGroupAboveItsCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child")
	var mu sync.Mutex
	var groups []int
	guard := &gitGuard{limit: 1 << 30, rss: func(pgid int) int64 {
		mu.Lock()
		groups = append(groups, pgid)
		mu.Unlock()
		if _, err := os.Stat(childFile); err != nil {
			return 0
		}
		return 2 << 30
	}}
	cmd := exec.Command("sh", "-c", `echo $$; sleep 60 >/dev/null 2>&1 & echo $! > "$1.tmp"; mv "$1.tmp" "$1"; wait`, "sh", childFile)
	began := time.Now()
	run := guard.run(context.Background(), cmd, time.Minute, false)
	took := time.Since(began)
	if run.killed != GitMemoryCap || run.code != -1 || run.err != nil || run.peak != 2<<30 || guard.peak != 2<<30 || took > 20*time.Second {
		t.Fatalf("the guard's run = killed %q, exit %d, error %v, peak %d (the guard's %d), after %s; want a memory-cap kill, exit -1, peak %d, at once",
			run.killed, run.code, run.err, run.peak, guard.peak, took, 2<<30)
	}
	shell, err := strconv.Atoi(strings.TrimSpace(string(run.stdout)))
	if err != nil {
		t.Fatalf("the shell printed %q, not its pid", run.stdout)
	}
	data, err := os.ReadFile(childFile)
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("the child file holds %q, not a pid", data)
	}
	// The child is its group's last process: once its shell is gone, init reaps it whenever it
	// gets to it, so the test waits on both ending rather than on a time.
	shellGone, childGone := processGone(shell), processGone(child)
	for deadline := time.Now().Add(10 * time.Second); !(shellGone && childGone) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		shellGone, childGone = processGone(shell), processGone(child)
	}
	if !shellGone || !childGone {
		t.Errorf("the shell %d ended %v and its child %d ended %v; want the whole group ended", shell, shellGone, child, childGone)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, group := range groups {
		if group != shell {
			t.Errorf("the guard read the memory of the group %d, want the shell's own, %d", group, shell)
		}
	}
	if len(groups) == 0 {
		t.Error("the guard read no group's memory")
	}
}

// The guard kills a command past its time, as a timeout, and lets one under its cap and its time
// run to its own end: that one's exit code, stdout and stderr are its own, and nothing killed it.
func TestTheGitGuardKillsACommandPastItsTimeAndLetsOthersEnd(t *testing.T) {
	t.Parallel()
	guard := &gitGuard{limit: 1 << 30, rss: func(int) int64 { return 1 << 20 }}
	began := time.Now()
	run := guard.run(context.Background(), exec.Command("sleep", "60"), 300*time.Millisecond, false)
	if took := time.Since(began); run.killed != GitTimeout || run.code != -1 || took > 20*time.Second {
		t.Errorf("a minute's sleep under a time of 300ms = killed %q, exit %d, after %s; want a timeout kill at once", run.killed, run.code, took)
	}
	run = guard.run(context.Background(), exec.Command("sh", "-c", "echo out; echo err >&2; exit 3"), time.Minute, false)
	if run.killed != "" || run.err != nil || run.code != 3 || string(run.stdout) != "out\n" || string(run.stderr) != "err\n" || run.ok() {
		t.Errorf("a command that exits 3 = killed %q, error %v, exit %d, stdout %q, stderr %q; want its own exit 3 and output", run.killed, run.err,
			run.code, run.stdout, run.stderr)
	}
}

// The guard counts the time of a command it runs sinceOutput from the command's last write on
// stdout, not from its start, as a merge-tree --stdin's time is that of each merge: a command that
// writes a byte about every 10ms, a tenth of its time, for at least four of its times in all ends by
// itself with all it wrote; one that writes and then stalls is killed as a timeout, no sooner than
// its time, with what it wrote kept. A command run without sinceOutput is still killed once its
// whole run passes its time, though it writes all the while. It fails where the guard counts a
// sinceOutput command's time from its start, lets one that stalls run on or drops what it wrote, or
// counts another command's time from its writes.
func TestTheGitGuardCountsATimeFromTheLastOutputWhereAsked(t *testing.T) {
	// Not parallel: its first command writes about every 10ms under a time of 100ms (bound) and its
	// second writes as it starts under that time, steps the parallel tests' load could push past it on
	// a correct guard.
	guard := &gitGuard{limit: 1 << 30, rss: func(int) int64 { return 1 << 20 }}
	const bound = 100 * time.Millisecond
	writes := `i=0; while [ "$i" -lt "$1" ]; do printf x; sleep 0.01; i=$((i+1)); done`
	run := guard.run(context.Background(), exec.Command("sh", "-c", writes, "sh", "40"), bound, true)
	if run.killed != "" || run.err != nil || run.code != 0 || string(run.stdout) != strings.Repeat("x", 40) {
		t.Errorf("40 writes 10ms apart under a time of %s since the last = killed %q, error %v, exit %d, stdout %q; want its own exit 0 and its 40 bytes",
			bound, run.killed, run.err, run.code, run.stdout)
	}
	began := time.Now()
	run = guard.run(context.Background(), exec.Command("sh", "-c", "printf abc; exec sleep 60"), bound, true)
	if took := time.Since(began); run.killed != GitTimeout || run.code != -1 || string(run.stdout) != "abc" || took < bound || took > 20*time.Second {
		t.Errorf("a write, then a minute's sleep, under a time of %s since the last write = killed %q, exit %d, stdout %q, after %s; "+
			"want a timeout kill no sooner than its time, its write kept", bound, run.killed, run.code, run.stdout, took)
	}
	began = time.Now()
	run = guard.run(context.Background(), exec.Command("sh", "-c", writes, "sh", "600"), bound, false)
	if took := time.Since(began); run.killed != GitTimeout || run.code != -1 || took > 20*time.Second {
		t.Errorf("600 writes 10ms apart under a time of %s of the whole run = killed %q, exit %d, after %s; want a timeout kill once the run passes its time",
			bound, run.killed, run.code, took)
	}
}
