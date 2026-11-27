package testexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// ParseStartMark reads starttime as field 22 of the stat line counted from after its last ')', so
// a command's name that holds one moves nothing, and the uptime from its line's first field, and
// fails a stat line cut short of starttime, one with no ')', and an uptime that is no number.
func TestParseStartMarkReadsTheTwoReadings(t *testing.T) {
	stat := "4242 (a) (b) S" + strings.Repeat(" 0", 18) + " 12345 77 88"
	want := StartMark{Started: 123450 * time.Millisecond, Reached: 130500 * time.Millisecond}
	for _, uptime := range []string{"130.50 999.99", "130.50"} {
		if mark, err := ParseStartMark(uptime, stat); err != nil || mark != want {
			t.Errorf("ParseStartMark(%q, %q) = %+v, %v; want %+v", uptime, stat, mark, err, want)
		}
	}
	for _, c := range []struct{ uptime, stat string }{
		{"130.50", "4242 (a) S 0 0"},
		{"130.50", "4242 a S" + strings.Repeat(" 0", 18) + " 12345"},
		{"up", stat},
	} {
		if mark, err := ParseStartMark(c.uptime, c.stat); err == nil {
			t.Errorf("ParseStartMark(%q, %q) = %+v; want an error", c.uptime, c.stat, mark)
		}
	}
}

// RunFromStart kills a run still going bound after its start and says it overran, and leaves a
// run that exits on its own to that exit, which is no overrun, whatever its exit status.
func TestRunFromStartKillsOnlyARunPastItsBound(t *testing.T) {
	overran, err := RunFromStart(t, exec.Command("sleep", "60"), 100*time.Millisecond)
	if !overran || err == nil {
		t.Errorf("a sleep of 60 s bounded to 100 ms: overran %t, %v; want it killed as an overrun", overran, err)
	}
	overran, err = RunFromStart(t, exec.Command("sh", "-c", "exit 3"), time.Minute)
	var exit *exec.ExitError
	if overran || !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("a shell that exits 3, bounded to a minute: overran %t, %v; want exit 3 and no overrun", overran, err)
	}
}
