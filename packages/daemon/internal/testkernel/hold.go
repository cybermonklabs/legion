package testkernel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// A stand-in that a stop finds still running tells, from its own records, whether a loaded machine
// or the code under test kept it running that long. HoldStart is shell for its first act and
// HoldTrap for the first act of its SIGTERM trap, and ReadHold reads the span between the two. Over
// that span the stand-in ran, waited for a CPU, or slept, and the kernel counts the first two in
// the stand-in's own /proc/<pid>/schedstat line (the kernel's
// Documentation/scheduler/sched-stats.rst: its time on the CPU, then run_delay, each in
// nanoseconds). A stand-in that forks nothing in that span, and writes only to files and to pipes
// the code under test reads, has nothing of its own to sleep on there, and a loaded machine holds
// it as run-queue wait, not as sleep. So a span in which it slept HeldBound or more, past what its
// two moments can be off by, was the code's hold: an output pipe the code left full and unread, or
// a SIGSTOP. A machine that held it that long in sleep, a D-state I/O stall in one of its few
// writes, reads the same, and the row fails it. A kernel that keeps no schedstat line, as gVisor's,
// leaves the span unknown, and nothing is held.

// HeldBound is the sleep, past what a Hold's two moments can be off by, that only the code under
// test gives a stand-in that has nothing of its own to sleep on (Hold.Held).
const HeldBound = 100 * time.Millisecond

// HoldStart is shell for a stand-in's first act: it writes its own schedstat line, empty where the
// kernel keeps none, at the file the shell word at names with .waits added, and then the start
// mark testexec.MarkStart writes, at at. read, [ and printf are builtins, so the line is the
// shell's own and it forks nothing. The line is read before the mark's time, so the span counts a
// run or a wait just before that time as in it, which makes Slept less, never more; and it is
// written first, so a start mark that is there has its line.
func HoldStart(at string) string {
	return schedLine(at+`.waits`) + "; " + testexec.MarkStart(at)
}

// HoldTrap is shell for the first act of a stand-in's SIGTERM trap: it reads the time since boot,
// then its own schedstat line, and writes the line at the file the shell word at names with .waits
// added before it writes the time at at, as testexec.MarkNow writes one. The line is read after the
// time, for the reason HoldStart reads it before, and written first, so a time that is there has
// its line.
func HoldTrap(at string) string {
	return `read -r up </proc/uptime; ` + schedLine(at+`.waits`) + `; printf '%s\n' "$up" >` + at
}

// schedLine is shell that writes the shell's own /proc/<pid>/schedstat line at the file the shell
// word to names, or an empty line where the kernel keeps none.
func schedLine(to string) string {
	return `ss=; [ -r /proc/self/schedstat ] && read -r ss </proc/self/schedstat; printf '%s\n' "$ss" >` + to
}

// A Hold is what a stand-in recorded of itself from its first act to its SIGTERM trap (HoldStart,
// HoldTrap): the span between the two moments, and how much of it it ran and the kernel kept it
// waiting for a CPU. Known is false where the kernel keeps no schedstat line.
type Hold struct {
	Span, Ran, Waited time.Duration
	Known             bool
}

// Slept is the span less what the stand-in ran and waited for a CPU over it: the time it slept,
// give or take what the two moments can be off by.
func (h Hold) Slept() time.Duration { return h.Span - h.Ran - h.Waited }

// Held is whether the code under test held the stand-in: it slept HeldBound or more past what the
// two moments, each off by less than a tick (testexec.ClockTick), can be off by together. An
// unknown Hold is never held.
func (h Hold) Held() bool { return h.Known && h.Slept() >= 2*testexec.ClockTick+HeldBound }

// String says what the span shows of what kept the stand-in running.
func (h Hold) String() string {
	if !h.Known {
		return "this kernel keeps no /proc/<pid>/schedstat to tell a loaded machine's hold of it from the code's"
	}
	return fmt.Sprintf("over the %s from just before its first act to its SIGTERM trap the kernel kept it waiting %s for a CPU and it ran %s, so it slept %s", h.Span, h.Waited, h.Ran, h.Slept())
}

// ReadHold reads a stand-in's Hold from its start mark at start (HoldStart) and its trap's moment
// at trap (HoldTrap), and the schedstat lines beside them. It fails t on a record it cannot read,
// so a row reads it only where both moments are there. It calls t.Fatalf, so it must be called from
// the goroutine running the test.
func ReadHold(t testing.TB, start, trap string) Hold {
	t.Helper()
	first, err := testexec.ReadStartMark(start)
	if err != nil {
		t.Fatalf("read the stand-in's start mark: %v", err)
	}
	trapped, err := testexec.ReadNowMark(trap)
	if err != nil {
		t.Fatalf("read the moment the stand-in's SIGTERM trap recorded: %v", err)
	}
	before, after := schedFields(t, start+".waits"), schedFields(t, trap+".waits")
	h := Hold{Span: trapped - first.Reached}
	if before == nil || after == nil {
		return h
	}
	h.Ran, h.Waited, h.Known = after[0]-before[0], after[1]-before[1], true
	return h
}

// schedFields is the first two fields of the schedstat line at path, the time on the CPU and
// run_delay, or nil for the empty line a kernel with no such line leaves.
func schedFields(t testing.TB, path string) []time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stand-in's schedstat line: %v", err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return nil
	}
	if len(fields) < 2 {
		t.Fatalf("a schedstat line with no run_delay at %s: %q", path, raw)
	}
	read := make([]time.Duration, 2)
	for i := range read {
		ns, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil || ns < 0 {
			t.Fatalf("field %d of the schedstat line at %s, %q: %v", i+1, path, raw, err)
		}
		read[i] = time.Duration(ns)
	}
	return read
}

// A Leader is the records a row's command keeps of its own run, the command being the leader of
// the process group the runner gives it, so that of a run its budget's stop reached before the
// command had written its answer the row can tell a runner that held the command it started from
// a machine that kept it waiting for a CPU (Hold). From its first act to its last write to its
// output pipes the command forks nothing: anything it does past that point, such as starting a
// Child and waiting for it, a Hold cannot read, and HeldBeforeItWrote reads nothing there.
type Leader struct{ dir string }

// NewLeader is a Leader whose records are in a directory of t's own.
func NewLeader(t testing.TB) *Leader {
	t.Helper()
	return &Leader{dir: t.TempDir()}
}

// Begin is shell for the command's first lines: a SIGTERM trap whose first act records the Hold's
// end (HoldTrap) and which then exits 143, and the command's first act, its start mark (HoldStart)
// at Mark.
func (l *Leader) Begin() string {
	return `leaderterm() { ` + HoldTrap(shellprefix.Literal(l.path("trapped"))) + `; exit 143; }; trap leaderterm TERM; ` +
		HoldStart(shellprefix.Literal(l.Mark()))
}

// Wrote is shell for the line just after the command's last write to its output pipes.
func (l *Leader) Wrote() string {
	return `: >` + shellprefix.Literal(l.path("wrote"))
}

// Mark is where Begin writes the command's start mark (testexec.ReadStartMark,
// testexec.StartedAfter).
func (l *Leader) Mark() string { return l.path("started") }

// HeldBeforeItWrote reads, of a run that is over, whether a SIGTERM's trap ran in the command
// before its last write (Wrote), and where one did, the Hold from its start mark to that trap. ok
// is false where no trap ran, or where the command had written by then.
func (l *Leader) HeldBeforeItWrote(t testing.TB) (hold Hold, ok bool) {
	t.Helper()
	if _, err := os.Stat(l.path("trapped")); err != nil {
		return Hold{}, false
	}
	if _, err := os.Stat(l.path("wrote")); !errors.Is(err, os.ErrNotExist) {
		return Hold{}, false
	}
	return ReadHold(t, l.Mark(), l.path("trapped")), true
}

// path is the command's record named name.
func (l *Leader) path(name string) string { return filepath.Join(l.dir, name) }
