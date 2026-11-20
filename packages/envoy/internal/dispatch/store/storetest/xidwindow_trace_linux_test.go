//go:build linux

package storetest

import (
	"testing"
	"time"

	exptrace "golang.org/x/exp/trace"
)

// A gap's goroutine states are read off the trace for the goroutine that was running on the
// gap's thread when the gap began: its time runnable and its time waiting, clipped to the gap.
// Another goroutine's states, and the states before or after the gap, are not the gap's.
func TestAGapsGoroutineStatesAreReadForItsThreadAndItsSpan(t *testing.T) {
	base := time.Now()
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		baseMono: 0,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		runs: map[int][]xidThreadRun{
			7: {{at: at(0), g: 1}},
			8: {{at: at(0), g: 2}},
		},
		changes: map[exptrace.GoID][]xidGoStateChange{
			1: {
				{at: at(0), state: exptrace.GoRunning},
				{at: at(2), state: exptrace.GoRunnable}, // preempted
				{at: at(5), state: exptrace.GoRunning},
				{at: at(6), state: exptrace.GoWaiting}, // a sleep it asked for
				{at: at(9), state: exptrace.GoRunnable},
				{at: at(10), state: exptrace.GoRunning},
				{at: at(20), state: exptrace.GoRunnable}, // after the gap
			},
			2: {
				{at: at(0), state: exptrace.GoRunning},
				{at: at(1), state: exptrace.GoWaiting},
			},
		},
	}
	gapFrom, gapTo := base.Add(time.Millisecond), base.Add(12*time.Millisecond)
	runnable, waiting, ok := reading.goStates(7, gapFrom, gapTo)
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	if want := 4 * time.Millisecond; runnable != want {
		t.Errorf("runnable %s, want %s (2 to 5 ms preempted and 9 to 10 ms woken)", runnable, want)
	}
	if want := 3 * time.Millisecond; waiting != want {
		t.Errorf("waiting %s, want %s (6 to 9 ms asleep)", waiting, want)
	}
	if _, _, ok := reading.goStates(9, gapFrom, gapTo); ok {
		t.Error("the trace was read for thread 9, on which no goroutine ran")
	}
	if _, _, ok := reading.goStates(7, base.Add(-time.Millisecond), gapTo); ok {
		t.Error("the trace was read for a gap that began before any goroutine ran on thread 7")
	}
}

// Made counts the CPU the thread ran and the waits the goroutine asked for whatever the machine
// does, and takes out what the machine did: the thread's wait for a CPU and the goroutine's wait
// for the runtime to run it again.
func TestAGapIsTheCodesTimeLessTheMachinesWaits(t *testing.T) {
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	for _, test := range []struct {
		name string
		gap  XidGap
		want time.Duration
	}{
		{"a preemption the runtime took 7 ms to undo", XidGap{Clock: ms(10), Ran: ms(1), Runnable: ms(2), GoRunnable: ms(7), Read: true, Traced: true}, ms(1)},
		{"a sleep the code asked for", XidGap{Clock: ms(21), Ran: ms(0.2), GoRunnable: ms(0.5), GoWaiting: ms(20), Read: true, Traced: true}, ms(20.5)},
		{"a blocking system call", XidGap{Clock: ms(8), Ran: ms(0.5), Read: true, Traced: true}, ms(8)},
		{"a handoff whose thread then waited for a CPU", XidGap{Clock: ms(30), Ran: ms(1), Runnable: ms(25), GoRunnable: ms(10), Read: true, Traced: true}, ms(1)},
		{"a gap whose clocks were not read", XidGap{Clock: ms(9)}, ms(9)},
	} {
		if got := test.gap.Made(); got != test.want {
			t.Errorf("%s: Made() = %s, want %s", test.name, got, test.want)
		}
	}
}
