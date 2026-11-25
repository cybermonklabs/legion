//go:build !race

package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testcleanup"
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
		softirqs[i] = counts[vector.name]
	}
	return softirqs
}

// A gap whose CPU ran a device softirq, on a kernel that charges interrupt time to the thread it
// lands on, is held to D6 widened by the time those softirqs may have put on its thread: 0.5 ms
// for each counted run, 2.4 ms at most for the gap, and never more than the CPU time its thread
// ran (XidGap.SoftirqHold). So one network receive never covers more than half a millisecond of
// a gap, a gap of the code's own network sends - hundreds counted - is still held to 7.4 ms, and a
// gap the code slept past the bound breaks it whatever its CPU ran. A gap uncounted, one whose CPU
// ran only the tick's softirqs, and one on a kernel that accounts interrupt time apart keep the
// 5 ms bound. Rule 2 holds the gaps to the statements and the time all the window's gaps' softirqs
// may hold, which is at most 2.4 ms for the window, however many of its gaps counted one: with
// that cap planted out two gaps of the code's own sends hold 4.8 ms, and with each gap's hold
// counted twice a 0.6 ms excess passes on a 0.5 ms hold.
func TestAGapsDeviceSoftirqsHoldOnlyTheirMeasuredTime(t *testing.T) {
	RequireXidGapJudgment(t)
	charged := true
	xidInterruptTimeCharged = func() bool { return charged }
	testcleanup.Add(t, func() { xidInterruptTimeCharged = xidReadInterruptTimeCharged })
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	cpuGap := func(made float64, softirqs map[string]int64) XidGap {
		return XidGap{Clock: ms(made), Ran: ms(made), Read: true, Counted: softirqs != nil, SoftirqCPU: 3, Softirqs: xidSoftirqsOf(softirqs)}
	}
	sleepGap := XidGap{Clock: ms(6.5), Ran: ms(0.5), GoWaiting: ms(6), Read: true, Counted: true, SoftirqCPU: 3,
		Softirqs: xidSoftirqsOf(map[string]int64{"NET_RX": 600})}
	own := map[string]int64{"NET_RX": 600}
	for _, test := range []struct {
		name    string
		window  XidWindow
		hold    time.Duration
		broken  string
		charged bool
	}{
		{"a 5.4 ms gap on its CPU, which ran one network receive",
			xidJudgedWindow(ms(10), cpuGap(5.4, map[string]int64{"NET_RX": 1, "SCHED": 2})), ms(0.5), "", true},
		{"a 5.6 ms gap on its CPU, which ran one network receive",
			xidJudgedWindow(ms(10), cpuGap(5.6, map[string]int64{"NET_RX": 1})), ms(0.5),
			"a statement-free gap of 5.600 ms (over 5.500 ms: the 5.000 ms bound and 0.500 ms its CPU's device softirqs may hold)", true},
		{"a 7.3 ms gap of the code's own network sends",
			xidJudgedWindow(ms(10), cpuGap(7.3, own)), ms(2.4), "", true},
		{"a 7.5 ms gap of the code's own network sends",
			xidJudgedWindow(ms(10), cpuGap(7.5, own)), ms(2.4), "a statement-free gap of 7.500 ms (over 7.400 ms", true},
		{"a 5.4 ms gap uncounted", xidJudgedWindow(ms(10), cpuGap(5.4, nil)), 0,
			"a statement-free gap of 5.400 ms (over 5.000 ms)", true},
		{"a 5.4 ms gap, its CPU running only the tick's softirqs",
			xidJudgedWindow(ms(10), cpuGap(5.4, map[string]int64{"TIMER": 4, "SCHED": 6, "HRTIMER": 1, "RCU": 2})), 0,
			"a statement-free gap of 5.400 ms (over 5.000 ms)", true},
		{"a 6 ms sleep, its CPU running 600 network receives", xidJudgedWindow(ms(10), sleepGap), ms(0.5),
			"a statement-free gap of 6.500 ms (over 5.500 ms", true},
		{"a 5.4 ms gap with a network receive, on a kernel that accounts interrupt time apart",
			xidJudgedWindow(ms(10), cpuGap(5.4, map[string]int64{"NET_RX": 1})), 0,
			"a statement-free gap of 5.400 ms (over 5.000 ms)", false},
		{"gaps 0.6 ms over their statements, a counted gap holding 1 ms",
			xidJudgedWindow(ms(2), cpuGap(0.1, nil), cpuGap(2.5, map[string]int64{"BLOCK": 2})), ms(1), "", true},
		{"gaps 0.6 ms over their statements, the counted gap holding 0.2 ms",
			xidJudgedWindow(ms(2), cpuGap(2.4, nil), cpuGap(0.2, map[string]int64{"NET_RX": 1})), ms(0.2),
			"its gaps take 2.600 ms, more than its statements' 2.000 ms and the 0.200 ms their CPU's device softirqs may hold", true},
		{"gaps 0.6 ms over their statements, a counted gap holding 0.5 ms",
			xidJudgedWindow(ms(2), cpuGap(2.1, nil), cpuGap(0.5, map[string]int64{"NET_RX": 1})), ms(0.5),
			"its gaps take 2.600 ms, more than its statements' 2.000 ms and the 0.500 ms their CPU's device softirqs may hold", true},
		{"two gaps of the code's own sends, each holding 2.4 ms, the window 2.4 ms",
			xidJudgedWindow(ms(2), cpuGap(3, own), cpuGap(3, own)), ms(4.8),
			"its gaps take 6.000 ms, more than its statements' 2.000 ms and the 2.400 ms their CPU's device softirqs may hold", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			charged = test.charged
			var hold time.Duration
			for _, gap := range test.window.Gaps {
				hold += gap.SoftirqHold()
			}
			if hold != test.hold {
				t.Errorf("its gaps' softirqs hold %s, want %s\n%s", hold, test.hold, test.window.Detail())
			}
			broken := strings.Join(test.window.Violations(), "\n")
			switch {
			case test.broken == "" && broken != "":
				t.Errorf("the window breaks D6, want it kept:\n%s\n%s", broken, test.window.Detail())
			case test.broken != "" && !strings.Contains(broken, test.broken):
				t.Errorf("the window breaks %q, want %q\n%s", broken, test.broken, test.window.Detail())
			}
		})
	}
}

// The kernel keeps each softirq count in 32 bits and prints it unsigned, so a count that wrapped
// inside a gap reads less after the gap than before it. The gap still counts the runs between:
// 4294967290 before and 5 after is 11 runs, not a negative count that would hold no time.
func TestASoftirqCountThatWrappedInAGapReadsItsRuns(t *testing.T) {
	var before, after XidStatement
	before.endedIRQ = xidSoftirqReading{counts: xidSoftirqsOf(map[string]int64{"NET_RX": 4294967290, "TIMER": 7}), cpu: 3, ok: true}
	after.startedIRQ = xidSoftirqReading{counts: xidSoftirqsOf(map[string]int64{"NET_RX": 5, "TIMER": 9}), cpu: 3, ok: true}
	gap := XidGap{Read: true}
	countGap(&gap, before, after)
	if want := xidSoftirqsOf(map[string]int64{"NET_RX": 11, "TIMER": 2}); !gap.Counted || gap.Softirqs != want {
		t.Fatalf("a NET_RX count that wrapped from 4294967290 to 5 read %s (counted: %t), want %s", gap.Softirqs, gap.Counted, want)
	}
}
