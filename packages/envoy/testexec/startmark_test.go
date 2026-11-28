package testexec

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The start helpers compare two clocks: /proc/uptime, which SinceBoot and MarkNow read, and a
// process's /proc/<pid>/stat starttime, which MarkStart reads. A row that bounds a span from one to
// the other is only sound where they are one clock. This row checks that they agree on this
// machine: a process the row starts says, by its own MarkStart, that it was made no sooner than a
// tick before the row's read before Start, and no later than its read after Wait.
func TestTheStartClockAgreesWithTheUptimeClock(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "mark")
	cmd := exec.Command("sh", "-c", MarkStart(`"$1"`), "sh", mark)
	before := SinceBoot(t)
	if err := cmd.Run(); err != nil {
		t.Fatalf("the marking shell: %v", err)
	}
	after := SinceBoot(t)
	own, err := readStartMark(mark)
	if err != nil {
		t.Fatal(err)
	}
	if own.Started < before-clockTick || own.Started > after+clockTick {
		t.Errorf("the process's own MarkStart says it started at %s since boot, outside the row's reads before Start (%s) and after Wait (%s): this machine's process start times and /proc/uptime are not one clock", own.Started, before, after)
	}
	if own.Reached < own.Started-clockTick || own.Reached > after+clockTick {
		t.Errorf("the process's own mark says it reached its first line at %s since boot, outside its start (%s) and the row's read after Wait (%s)", own.Reached, own.Started, after)
	}
}

// parseStartMark reads starttime as field 22 of the stat line counted from after its last ')', so
// a command's name that holds one moves nothing, and the uptime from its line's first field, and
// fails a stat line cut short of starttime, one with no ')', and an uptime that is no number.
func TestParseStartMarkReadsTheTwoReadings(t *testing.T) {
	stat := "4242 (a) (b) S" + strings.Repeat(" 0", 18) + " 12345 77 88"
	want := startMark{Started: 123450 * time.Millisecond, Reached: 130500 * time.Millisecond}
	for _, uptime := range []string{"130.50 999.99", "130.50"} {
		if mark, err := parseStartMark(uptime, stat); err != nil || mark != want {
			t.Errorf("parseStartMark(%q, %q) = %+v, %v; want %+v", uptime, stat, mark, err, want)
		}
	}
	for _, c := range []struct{ uptime, stat string }{
		{"130.50", "4242 (a) S 0 0"},
		{"130.50", "4242 a S" + strings.Repeat(" 0", 18) + " 12345"},
		{"up", stat},
	} {
		if mark, err := parseStartMark(c.uptime, c.stat); err == nil {
			t.Errorf("parseStartMark(%q, %q) = %+v; want an error", c.uptime, c.stat, mark)
		}
	}
}
