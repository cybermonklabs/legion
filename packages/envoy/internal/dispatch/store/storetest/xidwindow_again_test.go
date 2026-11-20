//go:build !race

package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// xidJudgedWindow is a window of len(gaps)+1 statements with gaps between them and statements
// that took statementTime, its rules' readings worked out as xidWindowOf works them out.
func xidJudgedWindow(statementTime time.Duration, gaps ...XidGap) XidWindow {
	window := XidWindow{StatementTime: statementTime, Gaps: gaps}
	for i := range len(gaps) + 1 {
		window.Statements = append(window.Statements, XidStatement{SQL: "statement " + string(rune('a'+i))})
	}
	window.Transaction = XidTransaction{Statements: window.Statements, Committed: true}
	for i, gap := range gaps {
		window.GapTime += gap.Made()
		if gap.Made() > window.MaxGap {
			window.MaxGap, window.MaxGapAfter = gap.Made(), i
		}
	}
	return window
}

// xidSoftirqsOf is a count of each vector named, as XidSoftirqs.String names them.
func xidSoftirqsOf(counts map[string]int64) XidSoftirqs {
	var softirqs XidSoftirqs
	for i, vector := range xidSoftirqVectors {
		softirqs[i] = counts[vector]
	}
	return softirqs
}

// A window that breaks D6 is measured again only when every rule it breaks the machine's
// interrupts may hold: the gap's CPU ran a device softirq in it, and the gap's time over the bound
// is within the CPU time its thread ran. A gap uncounted, one whose CPU ran only the tick's
// softirqs, and one the code slept past the bound fail at once, as does a window beside them that
// could be measured again. With the excuse widened to the tick's vectors, the row of a CPU-heavy
// gap whose CPU ran only those is measured again, which is why they are left out: every gap long on
// a CPU meets them.
func TestAGapLongOnACPUIsExcusedByDeviceSoftirqsAlone(t *testing.T) {
	if reason := XidNoGapJudgment(); reason != "" {
		t.Skipf("this run judges no gap, so no window breaks D6: %s", reason)
	}
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	cpuGap := func(made float64, softirqs map[string]int64) XidGap {
		return XidGap{Clock: ms(made), Ran: ms(made), Read: true, Counted: softirqs != nil, SoftirqCPU: 3, Softirqs: xidSoftirqsOf(softirqs)}
	}
	sleepGap := XidGap{Clock: ms(9), Ran: ms(0.5), GoWaiting: ms(8.5), Read: true, Counted: true, SoftirqCPU: 3,
		Softirqs: xidSoftirqsOf(map[string]int64{"NET_RX": 3})}
	for _, test := range []struct {
		name   string
		window XidWindow
		again  []string
	}{
		{"a 6 ms gap on its CPU, which ran a network receive in it",
			xidJudgedWindow(ms(10), cpuGap(6, map[string]int64{"NET_RX": 3, "SCHED": 2})),
			[]string{"the gap after statement 0 made 6.000 ms, 1.000 ms over the 5.000 ms bound", "running 6.000 ms", "CPU 3 ran NET_RX +3, SCHED +2"}},
		{"the same gap uncounted", xidJudgedWindow(ms(10), cpuGap(6, nil)), nil},
		{"the same gap, its CPU running only the tick's softirqs",
			xidJudgedWindow(ms(10), cpuGap(6, map[string]int64{"TIMER": 4, "SCHED": 6, "HRTIMER": 1, "RCU": 2})), nil},
		{"a sleep past the bound, its CPU running a network receive", xidJudgedWindow(ms(10), sleepGap), nil},
		{"gaps over their statements, the counted gap's CPU time holding the excess",
			xidJudgedWindow(ms(2), cpuGap(0.1, nil), cpuGap(2.5, map[string]int64{"BLOCK": 1})),
			[]string{"its gaps take 2.600 ms, 0.600 ms over its statements' 2.000 ms", "its thread ran 2.500 ms in gaps whose CPU ran a device softirq", "CPU 3: BLOCK +1"}},
		{"gaps over their statements, the excess in uncounted gaps",
			xidJudgedWindow(ms(2), cpuGap(2.4, nil), cpuGap(0.2, map[string]int64{"NET_RX": 1})), nil},
		{"both rules broken, both by a gap whose CPU ran a network receive",
			xidJudgedWindow(ms(1), cpuGap(6, map[string]int64{"NET_RX": 1})),
			[]string{"over the 5.000 ms bound", "over its statements' 1.000 ms"}},
		{"both rules broken, one by a sleep", xidJudgedWindow(ms(1), cpuGap(6, map[string]int64{"NET_RX": 1}), sleepGap), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if len(test.window.Violations()) == 0 {
				t.Fatalf("the window breaks no rule, so the row proves nothing\n%s", test.window.Detail())
			}
			again := test.window.Again()
			if (again != "") != (test.again != nil) {
				t.Fatalf("Again() = %q, want it measured again: %t\n%s", again, test.again != nil, test.window.Detail())
			}
			for _, want := range test.again {
				if !strings.Contains(again, want) {
					t.Errorf("Again() = %q, want it to name %q", again, want)
				}
			}
		})
	}

	t.Run("the verdict", func(t *testing.T) {
		machine := xidJudgedWindow(ms(10), cpuGap(6, map[string]int64{"NET_RX": 3}))
		slept := xidJudgedWindow(ms(10), sleepGap)
		var again interface{ XidMeasureAgain() string }
		err := (&XidReport{Windows: []XidWindow{machine, machine}}).Err()
		if !errors.As(err, &again) || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") {
			t.Errorf("two windows the machine may hold: verdict %v, want a D6 violation to measure again", err)
		}
		err = (&XidReport{Windows: []XidWindow{machine, slept}}).Err()
		if err == nil || errors.As(err, &again) {
			t.Errorf("one window the machine may hold beside one the code slept in: verdict %v, want a plain D6 violation", err)
		}
	})
}

// XidAttempts measures again only while the rig says to, and judges the last attempt as it is: a
// re-measure never passes a window. A plain violation ends no attempt, and nor does a verdict to
// measure again in a test that does not run under XidAttempts.
func TestXidAttemptsJudgeTheLastAttemptAsItIs(t *testing.T) {
	again := &xidAgainError{violations: "XID WINDOW VIOLATION (planted)\n", reason: "the gap after statement 0 made 6.000 ms, its CPU 3 ran NET_RX +1"}
	attempts, judged := 0, 0
	XidAttempts(t, func(t *testing.T) {
		attempts++
		XidMeasureAgain(t, again)
		judged++
	})
	if attempts != xidAttemptsMax || judged != 1 {
		t.Fatalf("a window the rig said to measure again every time: %d attempts, %d judged, want %d attempts and the last judged", attempts, judged, xidAttemptsMax)
	}

	attempts, judged = 0, 0
	XidAttempts(t, func(t *testing.T) {
		attempts++
		XidMeasureAgain(t, errors.New("XID WINDOW VIOLATION (planted)"))
		judged++
	})
	if attempts != 1 || judged != 1 {
		t.Fatalf("a plain violation: %d attempts, %d judged, want the first judged", attempts, judged)
	}

	XidMeasureAgain(t, again)
	if t.Skipped() {
		t.Fatal("a test that does not measure under XidAttempts skipped for a verdict to measure again")
	}
}
