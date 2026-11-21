//go:build linux

package storetest

import (
	"strings"
	"testing"
	"time"

	exptrace "golang.org/x/exp/trace"
)

// A gap's goroutine states are read off the trace for the goroutine that was running on the
// gap's thread when the gap began: its time runnable, waiting and in a system call, clipped to the
// gap. Another goroutine's states, and the states before or after the gap, are not the gap's.
func TestAGapsGoroutineStatesAreReadForItsThreadAndItsSpan(t *testing.T) {
	base := time.Now()
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		baseMono: 0,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		last:     at(20),
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

// At each generation's start the trace writes every goroutine's status as a transition from the
// state it is already in (x/exp/trace's order.go). That is no change of state: here goroutine 1
// is runnable from 1 ms, its runnable status comes again at 5 ms, and it runs at 6 ms, so it was
// runnable 5 ms in one stretch, which a reader that took the status for a change would split.
func TestAGenerationsStatusEventsAreNoStateChange(t *testing.T) {
	base := time.Now()
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		changes:  map[exptrace.GoID][]xidGoStateChange{},
		runs:     map[int][]xidThreadRun{},
	}
	for _, event := range []struct {
		change exptrace.StateTransition
		at     float64
		thread exptrace.ThreadID
	}{
		{exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), 1, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunnable), 5, exptrace.NoThread},
		{exptrace.MakeProcStateTransition(2, exptrace.ProcIdle, exptrace.ProcIdle), 5, exptrace.NoThread},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunning), 6, 7},
	} {
		reading.transition(event.change, at(event.at), event.thread)
	}
	if got := len(reading.changes[1]); got != 3 {
		t.Errorf("goroutine 1 changed state %d times, want 3: its status at the generation's start is no change (%+v)", got, reading.changes[1])
	}
	states, ok := reading.goStates(7, base.Add(500*time.Microsecond), base.Add(7*time.Millisecond))
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	if want := 5 * time.Millisecond; states.Runnable != want {
		t.Errorf("runnable %s, want %s", states.Runnable, want)
	}
}

// A trace that stops before a gap does says nothing of the gap's end: goStates carries no state
// past the trace's last event, and pastEnd says how much of the gap the trace does not reach. Here
// goroutine 1 runs on thread 7 and is runnable from 1 ms, and the trace's last event is at 2 ms,
// a P's transition, which is read for the trace's reach alone. A gap from 0.5 to 21 ms reads 1 ms
// runnable, not 20, and 19 ms past the trace; one that ends at 1.5 ms is all inside it. A
// reviewer's forcing stopped the trace while the window's goroutine was runnable and then ran
// 20 ms of the thread's CPU: carried to the gap's end, that state read 21.8 ms runnable and the gap
// 7.9 µs made, and the window kept D6 (2026-11-21).
func TestATracesStatesStopAtItsLastEvent(t *testing.T) {
	base := time.Now()
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		changes:  map[exptrace.GoID][]xidGoStateChange{},
		runs:     map[int][]xidThreadRun{},
	}
	for _, event := range []struct {
		change exptrace.StateTransition
		at     float64
		thread exptrace.ThreadID
	}{
		{exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), 1, 7},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcRunning, exptrace.ProcIdle), 2, 7},
	} {
		reading.transition(event.change, at(event.at), event.thread)
	}
	states, ok := reading.goStates(7, base.Add(500*time.Microsecond), base.Add(21*time.Millisecond))
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	if want := time.Millisecond; states.Runnable != want {
		t.Errorf("runnable %s, want %s: the goroutine's last state carried past the trace's end", states.Runnable, want)
	}
	if past, want := reading.pastEnd(base.Add(21*time.Millisecond)), 19*time.Millisecond; past != want {
		t.Errorf("a gap ending at 21 ms lies %s past the trace, want %s", past, want)
	}
	if past := reading.pastEnd(base.Add(1500 * time.Microsecond)); past != 0 {
		t.Errorf("a gap ending at 1.5 ms lies %s past the trace, want none", past)
	}
	window := XidWindow{Transaction: XidTransaction{PID: 7}, Statements: []XidStatement{{SQL: "insert into projects"}}, End: base.Add(21 * time.Millisecond)}
	if problem := xidPastTraceProblem(window, reading); !strings.Contains(problem, "UNKNOWN: backend 7's window") || !strings.Contains(problem, "ends 19.000 ms past the execution trace's last event") {
		t.Errorf("a window ending 19 ms past the trace: problem %q, want it UNKNOWN, naming how far", problem)
	}
	window.End = base.Add(1500 * time.Microsecond)
	if problem := xidPastTraceProblem(window, reading); problem != "" {
		t.Errorf("a window the trace reaches: problem %q, want none", problem)
	}
}

// Made is the lesser of the gap less the runtime's wait and the thread's CPU and sleep with the
// goroutine's own waits, so that no sleep or wait of the code's is ever taken out, wherever in the
// goroutine's states the thread's clocks put it; and it never reads more than the gap's own time on
// the clock. The first row is a tester's run of the handoff self-test at niceness 19 (2026-11-21):
// the goroutine preempted on its way into the runtime's handoff, its thread queued while the
// runtime held it, and then an 8 ms nanosleep of the code's, with the thread queued again after
// it. The formula before took the run-queue wait the trace tied to no handoff out of the system
// call and read that gap at 0.225 ms made, a D6 break unseen; here it reads 8.284 ms.
func TestAGapIsTheCodesTimeLessTheMachinesWaits(t *testing.T) {
	ms := func(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }
	us := func(n int64) time.Duration { return time.Duration(n) * time.Microsecond }
	for _, test := range []struct {
		name string
		gap  XidGap
		want time.Duration
	}{
		{"an 8 ms nanosleep after a 1,014 ms runtime wait spent queued", XidGap{Clock: us(1034020), Ran: us(225), Runnable: us(1025736), GoRunnable: us(1014255), GoSyscall: us(17883), Read: true, Traced: true}, us(8284)},
		{"a preemption the runtime took 7 ms to undo, the thread asleep", XidGap{Clock: ms(10), Ran: ms(1), GoRunnable: ms(7), Read: true, Traced: true}, ms(3)},
		{"a preemption the runtime took 7 ms to undo, 2 ms of it queued", XidGap{Clock: ms(10), Ran: ms(1), Runnable: ms(2), GoRunnable: ms(7), Read: true, Traced: true}, ms(3)},
		{"a handoff whose thread then waited 28 ms for a CPU", XidGap{Clock: ms(30), Ran: ms(1), Runnable: ms(28), GoRunnable: ms(10), Read: true, Traced: true}, ms(2)},
		{"an 8 ms nanosleep after a runtime wait", XidGap{Clock: ms(18), Ran: ms(1), Runnable: ms(5), GoRunnable: ms(7), GoSyscall: ms(8), Read: true, Traced: true}, ms(11)},
		{"a sleep the code asked for", XidGap{Clock: ms(21), Ran: ms(0.2), GoRunnable: ms(0.5), GoWaiting: ms(20), Read: true, Traced: true}, ms(20.5)},
		{"a blocking system call", XidGap{Clock: ms(8), Ran: ms(0.5), GoSyscall: ms(7.5), Read: true, Traced: true}, ms(8)},
		{"a system call whose return waited 6 ms for a CPU, a CFS quota's throttling included", XidGap{Clock: ms(10), Runnable: ms(6), GoSyscall: ms(10), Read: true, Traced: true}, ms(4)},
		{"a gap whose clocks read more CPU than its clock", XidGap{Clock: ms(0.016), Ran: ms(0.025), Read: true, Traced: true}, ms(0.016)},
		{"a gap whose clocks were not read", XidGap{Clock: ms(9)}, ms(9)},
	} {
		if got := test.gap.Made(); got != test.want {
			t.Errorf("%s: Made() = %s, want %s", test.name, got, test.want)
		}
	}
}

// The process's clock is tied to the monotonic clock only by a read of it that two monotonic reads
// bracket within xidClockTieBound: a stall between the reads - here 50 ms between the process's
// read and the second monotonic read, as a reviewer planted it into the lone pair of reads before
// this - leaves the pair untaken, and the next three reads tie the clocks. A tie that took the
// stalled pair would place every moment 50 ms late on the trace. A machine on which no three reads
// ever fall within the bound answers an error naming the closest, not a tie.
func TestTheClocksAreTiedOnlyByAReadNoStallSplits(t *testing.T) {
	ms := func(n float64) int64 { return int64(n * float64(time.Millisecond)) }
	wall := time.Now()
	readings := []int64{ms(0), ms(50.002), ms(60), ms(60.004)}
	walls := []time.Time{wall, wall.Add(60 * time.Millisecond)}
	monoReads, wallReads := 0, 0
	mono := func() (int64, error) { monoReads++; return readings[monoReads-1], nil }
	now := func() time.Time { wallReads++; return walls[wallReads-1] }
	tiedWall, tiedMono, err := xidTieClocks(mono, now)
	if err != nil {
		t.Fatalf("tie the clocks: %v", err)
	}
	if !tiedWall.Equal(walls[1]) || tiedMono != ms(60.002) {
		t.Fatalf("the clocks were tied at %s and monotonic %d ns, want the second read, %s and %d ns: the stalled pair was taken",
			tiedWall.Sub(wall), tiedMono, walls[1].Sub(wall), ms(60.002))
	}

	stalled := func() (int64, error) { monoReads++; return int64(monoReads) * ms(1), nil }
	monoReads = 0
	if _, _, err := xidTieClocks(stalled, time.Now); err == nil || !strings.Contains(err.Error(), "never lay within 20µs, the closest 1ms apart") {
		t.Fatalf("clocks whose reads always lie 1 ms apart: %v, want an error naming the closest pair", err)
	}
}
