package testexec

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clockTick is the unit of /proc/<pid>/stat's starttime: USER_HZ, a hundredth of a second on every
// Linux, which is also how finely /proc/uptime counts. A process's starttime truncates to the tick.
// Linux's /proc/uptime truncates to it too, and gVisor's rounds to it, so each reading is off by
// less than a tick, and the bias is gVisor's: there an uptime reads half a tick later on average
// than a starttime of the same moment. One reading is sound to a tick.
// TestTheStartClockAgreesWithTheUptimeClock checks that a process this machine starts writes a
// starttime, by its own MarkStart, within a tick of the span from an uptime read before its Start
// to one read after its Wait.
const clockTick = 10 * time.Millisecond

// MarkStart is shell that writes, to the file the shell word to names, when the kernel made the
// process the shell runs in and when the shell reached this line: the starttime of /proc/self/stat,
// which for a command a runner starts is that start itself, and /proc/uptime. Both count from boot
// in hundredths of a second (clockTick says how closely they agree), and SinceBoot reads uptime.
// The shell reads both itself (`read` is a builtin), so /proc/self is its own process, the one its
// runner started, and it forks nothing. The first time is the kernel's own record of the start:
// nothing a loaded machine or the product does to the shell after its start moves it. The second
// is that time plus how long the machine took to bring the shell to this line.
//
// A stand-in's first act writes it, so a row can count a bound from a fact of the run — the
// stand-in's start — rather than from the call, and tell a start the machine held late from latency
// the product added after a timely start.
func MarkStart(to string) string {
	return `read -r up </proc/uptime; read -r stat </proc/self/stat; printf '%s\n%s\n' "$up" "$stat" >` + to
}

// MarkNow is shell that writes /proc/uptime, the time since boot now, to the file the shell word to
// names: a moment a stand-in records of its own run, such as the moment its signal trap ran or the
// moment just before it exits, on the clock MarkStart and SinceBoot count on.
func MarkNow(to string) string {
	return `read -r up </proc/uptime; printf '%s\n' "$up" >` + to
}

// ReadNowMark reads the time since boot that MarkNow wrote at path.
func ReadNowMark(path string) (time.Duration, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	at, err := uptimeOf(strings.TrimSuffix(string(raw), "\n"))
	if err != nil {
		return 0, fmt.Errorf("the mark %s: %w", path, err)
	}
	return at, nil
}

// startMark is what MarkStart wrote: when the kernel made the process, and when its shell reached
// the mark, each as a time since boot.
type startMark struct{ Started, Reached time.Duration }

// readStartMark reads the mark MarkStart wrote at path: its uptime line, then its stat line, each
// read as parseStartMark reads it.
func readStartMark(path string) (startMark, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return startMark{}, err
	}
	up, stat, ok := strings.Cut(strings.TrimSuffix(string(raw), "\n"), "\n")
	if !ok {
		return startMark{}, fmt.Errorf("the start mark %s holds no stat line: %q", path, raw)
	}
	mark, err := parseStartMark(up, stat)
	if err != nil {
		return startMark{}, fmt.Errorf("the start mark %s: %w", path, err)
	}
	return mark, nil
}

// parseStartMark is the start mark of the two readings a shell took of itself, as MarkStart takes
// them: uptime, the /proc/uptime line it read or that line's first field, and stat, its own
// /proc/<pid>/stat line whole (starttimeOf).
func parseStartMark(uptime, stat string) (startMark, error) {
	reached, err := uptimeOf(uptime)
	if err != nil {
		return startMark{}, err
	}
	started, err := starttimeOf(stat)
	if err != nil {
		return startMark{}, err
	}
	return startMark{Started: started, Reached: reached}, nil
}

// starttimeOf is the starttime a /proc/<pid>/stat line gives, as a time since boot. Its fields are
// counted from after the line's last ')', since the command's name in parentheses may hold one, and
// starttime is field 22 (proc(5)).
func starttimeOf(stat string) (time.Duration, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("no stat line: %q", stat)
	}
	const starttime = 22 - 3
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= starttime {
		return 0, fmt.Errorf("no starttime: %q", stat)
	}
	ticks, err := strconv.ParseInt(fields[starttime], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("starttime %q: %w", fields[starttime], err)
	}
	return time.Duration(ticks) * clockTick, nil
}

// uptimeOf is the time since boot that a /proc/uptime line gives first, to its hundredth of a
// second.
func uptimeOf(line string) (time.Duration, error) {
	seconds, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	value, err := strconv.ParseFloat(seconds, 64)
	if err != nil {
		return 0, fmt.Errorf("read the uptime %q: %w", line, err)
	}
	return time.Duration(math.Round(value*float64(time.Second/clockTick))) * clockTick, nil
}

// SinceBoot is the time since boot now, on the clock MarkStart's marks count on.
func SinceBoot(t testing.TB) time.Duration {
	t.Helper()
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatalf("read the time since boot: %v", err)
	}
	now, err := uptimeOf(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// clockSlack is how far apart a starttime and an uptime of one moment can read: each is off by
// less than a tick (clockTick).
const clockSlack = 2 * clockTick

// checkStarted fails a start the kernel dates more than clockSlack before called, a time since
// boot read just before the call that made it: no start can come before its call, so the two
// clocks are not one on this machine. It is nil for any other start.
func checkStarted(started, called time.Duration) error {
	if started >= called-clockSlack {
		return nil
	}
	return fmt.Errorf("the kernel made it %s before the call, which no start can be: this machine's /proc/uptime and its processes' start times are not one clock", called-started)
}

// StartedAfter is how long after called, a time since boot read just before a call, the kernel made
// the process that wrote the start mark at path. A start more than two ticks before the call is no
// start at all (checkStarted): the row cannot read when its command started, which it says rather
// than pass.
func StartedAfter(t testing.TB, path string, called time.Duration) time.Duration {
	t.Helper()
	mark, err := readStartMark(path)
	if err != nil {
		t.Fatalf("read the command's start mark: %v", err)
	}
	if err := checkStarted(mark.Started, called); err != nil {
		t.Fatalf("the command's start mark says %v, so this row cannot read when the command started", err)
	}
	return mark.Started - called
}
