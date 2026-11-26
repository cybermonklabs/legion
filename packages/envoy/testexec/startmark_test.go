package testexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The start helpers compare two clocks: /proc/uptime, which SinceBoot and MarkNow read, and a
// process's /proc/<pid>/stat starttime, which ProcStart and MarkStart read. A row that bounds a span
// from one to the other is only sound where they are one clock. This row checks that they agree on
// this machine: a process the row starts is made no sooner than a tick before the row's read before
// Start, and no later than its read after, both by ProcStart and by the process's own MarkStart.
func TestTheStartClockAgreesWithTheUptimeClock(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "mark")
	// The shell holds until the row closes its stdin, so it is alive when ProcStart reads it. read
	// returns 1 at that end of input, which is the row's release, not a failure of the shell.
	cmd := exec.Command("sh", "-c", MarkStart(`"$1"`)+`; read -r _ || :`, "sh", mark)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	before := SinceBoot(t)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started, procErr := ProcStart(cmd.Process.Pid)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	after := SinceBoot(t)
	if procErr != nil {
		t.Fatalf("ProcStart of a live process: %v", procErr)
	}
	if waitErr != nil {
		t.Fatalf("the marking shell: %v", waitErr)
	}
	if started < before-ClockTick || started > after+ClockTick {
		t.Errorf("ProcStart = %s since boot, outside the row's reads before Start (%s) and after Wait (%s): this machine's process start times and /proc/uptime are not one clock", started, before, after)
	}
	own, err := ReadStartMark(mark)
	if err != nil {
		t.Fatal(err)
	}
	if own.Started != started {
		t.Errorf("the process's own MarkStart says it started at %s since boot, ProcStart %s: one starttime read two ways", own.Started, started)
	}
	if own.Reached < own.Started-ClockTick || own.Reached > after+ClockTick {
		t.Errorf("the process's own mark says it reached its first line at %s since boot, outside its start (%s) and the row's read after Wait (%s)", own.Reached, own.Started, after)
	}
	if _, err := ProcStart(os.Getpid()); err != nil {
		t.Errorf("ProcStart of this test process: %v", err)
	}
}
