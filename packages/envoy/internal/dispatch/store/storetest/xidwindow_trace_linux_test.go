//go:build linux

package storetest

import (
	"testing"
	"time"

	exptrace "golang.org/x/exp/trace"
)

// A gap's goroutine states are read off the trace for the goroutine that was running on the
// gap's thread when the gap began: its time runnable, waiting and in a system call, clipped to the
// gap, and the part of its runnable time its thread spent between a P's stop and its own start of
// that P - the run queue after a handoff. Another goroutine's states, the states before or after
// the gap, and a start of a P while the goroutine was not runnable are not the gap's.
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
		starts: map[int][]xidProcStart{
			7: {
				{at: at(4.5), stopped: at(1)},   // handed back 2.5 ms into the first runnable stretch
				{at: at(10), stopped: at(9.5)},  // handed back 0.5 ms after the P stopped
				{at: at(11.6), stopped: at(11)}, // while it ran: not a handoff of the gap
			},
		},
		changes: map[exptrace.GoID][]xidGoStateChange{
			1: {
				{at: at(0), state: exptrace.GoRunning},
				{at: at(2), state: exptrace.GoRunnable}, // preempted
				{at: at(5), state: exptrace.GoRunning},
				{at: at(6), state: exptrace.GoWaiting}, // a sleep it asked for
				{at: at(9), state: exptrace.GoRunnable},
				{at: at(10), state: exptrace.GoRunning},
				{at: at(11), state: exptrace.GoSyscall}, // a system call
				{at: at(11.5), state: exptrace.GoRunning},
				{at: at(20), state: exptrace.GoRunnable}, // after the gap
			},
			2: {
				{at: at(0), state: exptrace.GoRunning},
				{at: at(1), state: exptrace.GoWaiting},
			},
		},
	}
	gapFrom, gapTo := base.Add(time.Millisecond), base.Add(12*time.Millisecond)
	states, ok := reading.goStates(7, gapFrom, gapTo)
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	for _, want := range []struct {
		name      string
		got, want time.Duration
		why       string
	}{
		{"runnable", states.Runnable, 4 * time.Millisecond, "2 to 5 ms preempted and 9 to 10 ms woken"},
		{"waiting", states.Waiting, 3 * time.Millisecond, "6 to 9 ms asleep"},
		{"in a system call", states.Syscall, 500 * time.Microsecond, "11 to 11.5 ms"},
		{"on the run queue after a handoff", states.Handoff, 3 * time.Millisecond, "2 to 4.5 ms and 9.5 to 10 ms"},
	} {
		if want.got != want.want {
			t.Errorf("%s %s, want %s (%s)", want.name, want.got, want.want, want.why)
		}
	}
	if _, ok := reading.goStates(9, gapFrom, gapTo); ok {
		t.Error("the trace was read for thread 9, on which no goroutine ran")
	}
	if _, ok := reading.goStates(7, base.Add(-time.Millisecond), gapTo); ok {
		t.Error("the trace was read for a gap that began before any goroutine ran on thread 7")
	}
}

// A handoff whose wait spans a generation's start reads its run-queue wait from the P's real
// stop. At each generation's start the trace writes every P's and every goroutine's status as a
// transition from the state it is already in (x/exp/trace's order.go); here P 2 stops at 1 ms, its
// idle status comes at 5 ms, and thread 7 starts it at 6 ms while goroutine 1 is runnable from
// 1 ms, its runnable status also coming at 5 ms. The wait is 1 to 6 ms: 5 ms. A reader that took
// the idle status for a stop read it from 5 ms, 1 ms, and a gap that took that wait out of the
// goroutine's runnable time and the thread's run-queue time took the other 4 ms out twice.
func TestAGenerationsStatusEventsAreNoStateChange(t *testing.T) {
	base := time.Now()
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		changes:  map[exptrace.GoID][]xidGoStateChange{},
		runs:     map[int][]xidThreadRun{},
		starts:   map[int][]xidProcStart{},
	}
	stopped := map[exptrace.ProcID]exptrace.Time{}
	for _, event := range []struct {
		change exptrace.StateTransition
		at     float64
		thread exptrace.ThreadID
	}{
		{exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), 0, 7},
		{exptrace.MakeProcStateTransition(2, exptrace.ProcUndetermined, exptrace.ProcRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), 1, 7},
		{exptrace.MakeProcStateTransition(2, exptrace.ProcRunning, exptrace.ProcIdle), 1, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunnable), 5, exptrace.NoThread},
		{exptrace.MakeProcStateTransition(2, exptrace.ProcIdle, exptrace.ProcIdle), 5, exptrace.NoThread},
		{exptrace.MakeProcStateTransition(2, exptrace.ProcIdle, exptrace.ProcRunning), 6, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunning), 6, 7},
	} {
		reading.transition(event.change, at(event.at), event.thread, stopped)
	}
	if got := len(reading.changes[1]); got != 3 {
		t.Errorf("goroutine 1 changed state %d times, want 3: its status at the generation's start is no change (%+v)", got, reading.changes[1])
	}
	states, ok := reading.goStates(7, base.Add(500*time.Microsecond), base.Add(7*time.Millisecond))
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	if want := 5 * time.Millisecond; states.Runnable != want || states.Handoff != want {
		t.Errorf("runnable %s and on the run queue after the handoff %s, want %s each: P 2 stopped at 1 ms and thread 7 started it at 6 ms",
			states.Runnable, states.Handoff, want)
	}
}

// Made counts the CPU the thread ran, the waits the goroutine asked for and its system calls
// whatever the machine does, takes out what the machine did - the thread's wait for a CPU and the
// goroutine's wait for the runtime to run it again - counting once the run-queue wait after a
// handoff that both hold (Overlap), and never reads more than the gap's own time on the clock.
// The handoff row is the one the formula before Overlap read short: it took the handoff's 3 ms
// on the run queue out twice and left the system call out of its floor, so an 8 ms nanosleep read
// 6 ms made.
func TestAGapIsTheCodesTimeLessTheMachinesWaits(t *testing.T) {
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	for _, test := range []struct {
		name string
		gap  XidGap
		want time.Duration
	}{
		{"a preemption the runtime took 7 ms to undo", XidGap{Clock: ms(10), Ran: ms(1), Runnable: ms(2), GoRunnable: ms(7), Read: true, Traced: true}, ms(1)},
		{"an 8 ms nanosleep after a handoff whose thread waited 3 ms to run", XidGap{Clock: ms(18), Ran: ms(1), Runnable: ms(5), GoRunnable: ms(7), GoSyscall: ms(8), Overlap: ms(3), Read: true, Traced: true}, ms(9)},
		{"a sleep the code asked for", XidGap{Clock: ms(21), Ran: ms(0.2), GoRunnable: ms(0.5), GoWaiting: ms(20), Read: true, Traced: true}, ms(20.5)},
		{"a blocking system call", XidGap{Clock: ms(8), Ran: ms(0.5), GoSyscall: ms(7.5), Read: true, Traced: true}, ms(8)},
		{"a system call whose return waited 6 ms for a CPU", XidGap{Clock: ms(10), Runnable: ms(6), GoSyscall: ms(10), Read: true, Traced: true}, ms(4)},
		{"a handoff whose thread then waited for a CPU", XidGap{Clock: ms(30), Ran: ms(1), Runnable: ms(28), GoRunnable: ms(10), Overlap: ms(9), Read: true, Traced: true}, ms(1)},
		{"a gap whose clocks read more CPU than its clock", XidGap{Clock: ms(0.016), Ran: ms(0.025), Read: true, Traced: true}, ms(0.016)},
		{"a gap whose clocks were not read", XidGap{Clock: ms(9)}, ms(9)},
	} {
		if got := test.gap.Made(); got != test.want {
			t.Errorf("%s: Made() = %s, want %s", test.name, got, test.want)
		}
	}
}
