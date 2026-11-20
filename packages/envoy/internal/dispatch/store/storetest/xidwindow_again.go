package storetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A kernel that does not account interrupt time apart (CONFIG_IRQ_TIME_ACCOUNTING not set) charges
// the work a CPU does in an interrupt, and in the softirqs it runs on the interrupt's way out, to
// the thread that was running there. A gap's clocks then read that work as the CPU time its thread
// ran, which is the code's. Traced at a load of about 220 on a 288-CPU machine (2026-11-20): a
// forced gap read 5.054 ms made behind about 1 ms of code, and 39 of its 50 perf samples were the
// kernel's network receive (net_rx_action, on interrupt exit), the machine's own traffic. No clock
// a thread can read there tells the interrupt's part from the code's.
//
// What the rig can read is a count. It pins a marked transaction's thread to the CPU it is on at
// BEGIN, so the thread's work stays on that one CPU until COMMIT (XidTracer.lockTransaction), and
// reads that CPU's column of /proc/softirqs at each statement's two ends (readXidSoftirqs), so each
// gap knows how many times each softirq ran on its CPU meanwhile (XidGap.Softirqs). A count says a
// softirq ran on the CPU, not that it ran in the thread: on that machine a CPU serves a network
// receive every 2.5 to 8 ms, mostly in other threads. So a count never passes a window. It only
// sends a window that breaks D6 to be measured again (Again), and only when every rule the window
// breaks could be the counted interrupts' doing: the gap's time over the bound is within the CPU
// time its thread ran, the only part of a gap an interrupt is charged to, and its CPU ran a device
// softirq in it (xidExcusingVectors). A gap the code slept or waited past the bound is never the
// machine's, whatever the counts. And the last attempt is always judged (XidAttempts), so a gap the
// code makes on every attempt fails there.

// xidSoftirqVectors are the softirq vectors /proc/softirqs counts, in the kernel's order
// (include/linux/interrupt.h).
var xidSoftirqVectors = [...]string{"HI", "TIMER", "NET_TX", "NET_RX", "BLOCK", "IRQ_POLL", "TASKLET", "SCHED", "HRTIMER", "RCU"}

// XidSoftirqs is how many times each softirq vector's handler ran on one CPU over a stretch,
// indexed as xidSoftirqVectors.
type XidSoftirqs [len(xidSoftirqVectors)]int64

// xidExcusingVectors marks the vectors whose count can send a window to be measured again: the
// ones a machine's devices and network traffic raise (HI, NET_TX, NET_RX, BLOCK, IRQ_POLL,
// TASKLET). The tick and the scheduler raise TIMER, SCHED, HRTIMER and RCU on every CPU every few
// milliseconds - on the 288-CPU machine, SCHED about 18 and RCU about 11 times in 100 ms on the
// median CPU - so a count of them would excuse every gap long on a CPU, the code's own included
// (TestAGapLongOnACPUIsExcusedByDeviceSoftirqsAlone).
var xidExcusingVectors = [len(xidSoftirqVectors)]bool{true, false, true, true, true, true, true, false, false, false}

// excusing is how many times a vector of xidExcusingVectors ran.
func (s XidSoftirqs) excusing() int64 {
	var n int64
	for i, count := range s {
		if xidExcusingVectors[i] {
			n += count
		}
	}
	return n
}

// String names each vector that ran, with its count: "NET_RX +40, SCHED +2".
func (s XidSoftirqs) String() string {
	var moved []string
	for i, count := range s {
		if count != 0 {
			moved = append(moved, fmt.Sprintf("%s +%d", xidSoftirqVectors[i], count))
		}
	}
	if len(moved) == 0 {
		return "no softirq"
	}
	return strings.Join(moved, ", ")
}

// xidSoftirqReading is one reading of the softirq counts of the CPU a held thread is pinned to
// (readXidSoftirqs): the counts, the CPU, and the thread's CPU time the read took, which is the
// rig's own work inside a statement and not the statement's. ok is false where the thread is not
// held and pinned, or was not on its CPU.
type xidSoftirqReading struct {
	counts XidSoftirqs
	cpu    int
	cost   time.Duration
	ok     bool
}

// countGap fills in gap's softirq counts from before's end reading to after's start reading, where
// both were taken on one CPU of a gap read off its thread's clocks.
func countGap(gap *XidGap, before, after XidStatement) {
	from, to := before.endedIRQ, after.startedIRQ
	if !gap.Read || !from.ok || !to.ok || from.cpu != to.cpu {
		return
	}
	gap.Counted = true
	gap.SoftirqCPU = from.cpu
	for i := range gap.Softirqs {
		gap.Softirqs[i] = to.counts[i] - from.counts[i]
	}
}

// machineMayHold reports whether the machine's interrupts may hold over, the part of the gap's
// time a rule does not allow: its CPU ran a device softirq in it, and over is within the CPU time
// its thread ran. Time the code slept or waited is not that thread's CPU time, so no count can
// excuse it.
func (g XidGap) machineMayHold(over time.Duration) bool {
	return g.Counted && g.Softirqs.excusing() > 0 && over <= g.Ran
}

// Again is why the window is to be measured again, empty when it is not: it breaks D6, and every
// rule it breaks the machine's interrupts may hold (XidGap.machineMayHold). A window that breaks a
// rule no count can hold has no reason to be measured again, and fails at once.
func (w XidWindow) Again() string {
	if len(w.Violations()) == 0 {
		return ""
	}
	var reasons []string
	if w.MaxGap > XidMaxGap {
		for i, gap := range w.Gaps {
			over := gap.Made() - XidMaxGap
			if over <= 0 {
				continue
			}
			if !gap.machineMayHold(over) {
				return ""
			}
			reasons = append(reasons, fmt.Sprintf("the gap after statement %d made %s, %s over the %s bound, with its thread running %s of it while its CPU %d ran %s",
				i, XidMillis(gap.Made()), XidMillis(over), XidMillis(XidMaxGap), XidMillis(gap.Ran), gap.SoftirqCPU, gap.Softirqs))
		}
	}
	if w.GapTime > w.StatementTime {
		over := w.GapTime - w.StatementTime
		var ran time.Duration
		var counted []string
		for i, gap := range w.Gaps {
			if gap.Counted && gap.Softirqs.excusing() > 0 {
				ran += gap.Ran
				counted = append(counted, fmt.Sprintf("after statement %d, made %s, ran %s, CPU %d: %s", i, XidMillis(gap.Made()), XidMillis(gap.Ran), gap.SoftirqCPU, gap.Softirqs))
			}
		}
		if over > ran {
			return ""
		}
		reasons = append(reasons, fmt.Sprintf("its gaps take %s, %s over its statements' %s, and its thread ran %s in gaps whose CPU ran a device softirq (%s)",
			XidMillis(w.GapTime), XidMillis(over), XidMillis(w.StatementTime), XidMillis(ran), strings.Join(counted, "; ")))
	}
	return strings.Join(reasons, "; ")
}

// xidAgainError is a report's verdict when every window that breaks D6 is to be measured again
// (XidWindow.Again): the violations, which it reads as, and why. A test that measures under
// XidAttempts measures the window again for it (XidMeasureAgain), and one that repeats its own
// measurements finds it by its XidMeasureAgain method.
type xidAgainError struct {
	violations string
	reason     string
}

func (e *xidAgainError) Error() string {
	return fmt.Sprintf("%s  the machine's interrupts may hold every rule it breaks, so it is to be measured again: %s", e.violations, e.reason)
}

// XidMeasureAgain answers why the window is to be measured again.
func (e *xidAgainError) XidMeasureAgain() string { return e.reason }

// xidAttemptsMax is the most times XidAttempts measures. The last is judged as it is.
const xidAttemptsMax = 4

// xidAttempt is one of XidAttempts' measurements: whether it is the last, why the rig sent it to
// be measured again, and whether it was skipped.
type xidAttempt struct {
	last    bool
	again   string
	skipped bool
}

var (
	xidAttemptsMu sync.Mutex
	xidAttemptsOn = map[string]*xidAttempt{}
)

// xidAttemptOf is the attempt the test named name runs in: its own, or that of a test it is a
// subtest of.
func xidAttemptOf(name string) *xidAttempt {
	xidAttemptsMu.Lock()
	defer xidAttemptsMu.Unlock()
	for {
		if attempt, ok := xidAttemptsOn[name]; ok {
			return attempt
		}
		slash := strings.LastIndexByte(name, '/')
		if slash < 0 {
			return nil
		}
		name = name[:slash]
	}
}

// XidAttempts runs measure, which builds its fixture, measures and asserts, as a subtest, and runs
// it again while the rig sends its window to be measured again (XidMeasureAgain), at most
// xidAttemptsMax times; the last attempt is judged as it is. Each attempt builds a fixture of its
// own, since an operation the rig measured cannot be run again on the state it left. A count of
// the machine's interrupts is the only reason an attempt is made again, and each says so in the
// test's log, with the counts.
func XidAttempts(t *testing.T, measure func(t *testing.T)) {
	t.Helper()
	for n := 1; n <= xidAttemptsMax; n++ {
		attempt := &xidAttempt{last: n == xidAttemptsMax}
		passed := t.Run(fmt.Sprintf("attempt_%d", n), func(t *testing.T) {
			xidAttemptsMu.Lock()
			xidAttemptsOn[t.Name()] = attempt
			xidAttemptsMu.Unlock()
			defer func() {
				xidAttemptsMu.Lock()
				delete(xidAttemptsOn, t.Name())
				xidAttemptsMu.Unlock()
				attempt.skipped = t.Skipped()
			}()
			measure(t)
		})
		switch {
		case !passed:
			return
		case attempt.again != "":
			t.Logf("xid rig: attempt %d of %d measured again for the machine's interrupts: %s", n, xidAttemptsMax, attempt.again)
		case attempt.skipped:
			t.Logf("xid rig: attempt %d of %d skipped", n, xidAttemptsMax)
			return
		default:
			t.Logf("xid rig: judged on attempt %d of %d, after %d measured again for the machine's interrupts", n, xidAttemptsMax, n-1)
			return
		}
	}
}

// XidMeasureAgain ends the attempt t runs in when err is the rig's verdict that the window is to be
// measured again (XidWindow.Again), naming why, so XidAttempts measures it again. In the last
// attempt, and in a test that does not run under XidAttempts, it does nothing, and err is judged as
// it is: a re-measure never passes a window.
func XidMeasureAgain(t testing.TB, err error) {
	t.Helper()
	var again *xidAgainError
	if !errors.As(err, &again) {
		return
	}
	attempt := xidAttemptOf(t.Name())
	switch {
	case attempt == nil:
		t.Logf("xid rig: this test does not measure under storetest.XidAttempts, so its window is judged as it is: %s", again.reason)
	case attempt.last:
		t.Logf("xid rig: the last of %d attempts is judged as it is: %s", xidAttemptsMax, again.reason)
	default:
		attempt.again = again.reason
		t.Logf("xid rig: to be measured again: %s", again.reason)
		t.SkipNow()
	}
}
