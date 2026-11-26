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

// ClockTick is the unit of /proc/<pid>/stat's starttime: USER_HZ, a hundredth of a second on every
// Linux, which is also how finely /proc/uptime counts. The two agree to within one tick
// (TestTheStartClockAgreesWithTheUptimeClock, 20/20 gVisor and native), but not without bias:
// /proc/uptime rounds to the tick and a starttime truncates, so each StartMark.Late() runs up to
// half a tick high on average (about +5 ms a reading under gVisor). A single reading is sound to one
// tick; a bound that SUMS Late() over a number of readings the product chooses drifts by that half
// tick each, so it must sum a fixed set, not a count the product controls.
const ClockTick = 10 * time.Millisecond

// MarkStart is shell that writes, to the file the shell word to names, when the kernel made the
// process the shell runs in and when the shell reached this line: the starttime of /proc/self/stat,
// which for a command a runner starts is that start itself, and /proc/uptime. Both count from boot
// in hundredths of a second (ClockTick, above, on how closely they agree), and SinceBoot reads uptime. The shell reads
// both itself (`read` is a builtin), so /proc/self is its own process, the one its runner started, and
// it forks nothing. The first time is the kernel's own record of the start: nothing a loaded machine
// or the product does to the shell after its start moves it. The second is that time plus how long
// the machine took to bring the shell to this line (StartMark.Late).
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

// StartMark is what MarkStart wrote: when the kernel made the process, and when its shell reached the
// mark, each as a time since boot.
type StartMark struct{ Started, Reached time.Duration }

// Late is how long the machine took to bring the shell from its start to the mark.
func (m StartMark) Late() time.Duration { return m.Reached - m.Started }

// ReadStartMark reads the mark MarkStart wrote at path. The stat line's fields are counted from after
// its last ')', since the command's name in parentheses may hold one, and starttime is its field 22
// (proc(5)).
func ReadStartMark(path string) (StartMark, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return StartMark{}, err
	}
	up, stat, ok := strings.Cut(strings.TrimSuffix(string(raw), "\n"), "\n")
	if !ok {
		return StartMark{}, fmt.Errorf("the start mark %s holds no stat line: %q", path, raw)
	}
	reached, err := uptimeOf(up)
	if err != nil {
		return StartMark{}, fmt.Errorf("the start mark %s: %w", path, err)
	}
	started, err := starttimeOf(stat)
	if err != nil {
		return StartMark{}, fmt.Errorf("the start mark %s: %w", path, err)
	}
	return StartMark{Started: started, Reached: reached}, nil
}

// ProcStart is when the kernel made the live process pid, as a time since boot, read from its
// /proc/<pid>/stat starttime. It is for a binary a row starts itself, which has no shell to write
// MarkStart: the row reads it right after Start, while the process still lives.
func ProcStart(pid int) (time.Duration, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	started, err := starttimeOf(strings.TrimSuffix(string(raw), "\n"))
	if err != nil {
		return 0, fmt.Errorf("process %d: %w", pid, err)
	}
	return started, nil
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
	return time.Duration(ticks) * ClockTick, nil
}

// uptimeOf is the time since boot that a /proc/uptime line gives first, to its hundredth of a second.
func uptimeOf(line string) (time.Duration, error) {
	seconds, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	value, err := strconv.ParseFloat(seconds, 64)
	if err != nil {
		return 0, fmt.Errorf("read the uptime %q: %w", line, err)
	}
	return time.Duration(math.Round(value*float64(time.Second/ClockTick))) * ClockTick, nil
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

// StartedAfter is how long after called, a time since boot read just before a call, the kernel made
// the process that wrote the start mark at path. Each of the two times can read up to a tick early, so
// a start more than two ticks before the call is no start at all: the two clocks are not one on this
// machine, and the row cannot read when its command started, which it says rather than pass.
func StartedAfter(t testing.TB, path string, called time.Duration) time.Duration {
	t.Helper()
	mark, err := ReadStartMark(path)
	if err != nil {
		t.Fatalf("read the command's start mark: %v", err)
	}
	began := mark.Started - called
	if began < -2*ClockTick {
		t.Fatalf("the command's start mark says the kernel made it %s before the call, which no start can be: this machine's /proc/uptime and its processes' start times are not one clock, so this row cannot read when the command started", -began)
	}
	return began
}
