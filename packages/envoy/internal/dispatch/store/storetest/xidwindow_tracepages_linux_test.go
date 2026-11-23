//go:build !race

package storetest

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	exptrace "golang.org/x/exp/trace"
	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// xidBurstLogs is how many trace events the burst of TestTheTracersEventsInAWindowTouchNoFreshPage
// writes: each empty log is a few bytes of the thread's buffer, so these fill more than three of
// the runtime's 64 KiB trace buffers, and the thread takes a buffer from the pool at least three
// times.
const xidBurstLogs = 1 << 15

// xidGenerationWait is the longest TestTheTracersEventsInAWindowTouchNoFreshPage waits for a trace
// generation to begin before its burst: the runtime begins one every second.
const xidGenerationWait = 10 * time.Second

// A window's thread writes trace events inside its gaps, and the first write to a page is a page
// fault that waits for the page table's lock, on a loaded machine for milliseconds of the thread's
// CPU (xidTrace.warm). So the rig leaves the runtime's pool of trace buffers used and full
// (xidTrace.warm), and makes every page of the tracer's memory present at each BEGIN
// (xidHoldMemory). This test does what a BEGIN does and then writes more than three buffers of
// events from the same thread: every page of the tracer's memory must be present at the burst's
// start, the thread must take no page fault in the burst, and the runtime must map no new trace
// buffer for it. The thread's faults are read with a raw system call, which writes no trace event.
// With the warm-up planted out, the runtime maps fresh buffers for the burst and the thread faults
// on them; with the populate planted out, the tracer's pages are not present when the burst
// starts. The burst starts right after a trace generation begins (xidAwaitGeneration), when the
// next is a second away, so no generation begins inside it and remakes the trace's tables on blocks
// the runtime maps then. The test runs once; a burst that a generation still begins inside fails,
// since it proves nothing about the hold.
func TestTheTracersEventsInAWindowTouchNoFreshPage(t *testing.T) {
	if trace.IsEnabled() {
		t.Fatal("an execution trace is already running - go test -trace takes one - and this test takes its own; run it without go test -trace")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx := context.Background()
	var traced *xidTrace
	var before, after unix.Rusage
	var warmed, from, to time.Time
	var absent []string
	var mapped, grown []xidTracerRange
	var waited time.Duration
	// As Measure does, with the collector held off, so none of its workers writes events.
	runWithoutCollector(func() {
		var err error
		if traced, err = startXidTrace(); err != nil {
			t.Fatalf("start the execution tracer: %v", err)
		}
		defer traced.stop()
		// The burst starts right after a generation begins, and one call site writes every event,
		// so the first event, written after the wait and in that generation, puts the burst's
		// category and stack in the generation's tables before the hold, as a window's statements
		// before a gap do. A generation that begins from that event on remakes the tables, so the
		// check below covers the event too.
		waited = xidAwaitGeneration(t, traced)
		warmed = time.Now()
		for i := range xidBurstLogs + 1 {
			if i == 1 {
				xidHoldMemory()
				mapped = xidTracerMappings(t)
				absent = xidAbsentPages(mapped)
				xidThreadRusage(&before)
				from = time.Now()
			}
			trace.Log(ctx, "xid rig burst", "")
		}
		to = time.Now()
		xidThreadRusage(&after)
		grown = xidTracerMappings(t)
	})
	reading, err := traced.read()
	if err != nil {
		t.Fatalf("read the execution trace: %v", err)
	}
	start, end := reading.traceTime(warmed), reading.traceTime(to)
	if slices.ContainsFunc(reading.syncs, func(at exptrace.Time) bool { return at > start && at < end }) {
		t.Fatalf("a trace generation began between the burst's first event and its end, %s, which began %s into the wait for one: the runtime remakes the trace's tables then, so this run proves nothing",
			to.Sub(warmed), waited)
	}
	faults := after.Minflt - before.Minflt
	if len(absent) > 0 || faults != 0 || !slices.Equal(grown, mapped) {
		t.Fatalf("after the hold %d pages of the tracer's memory were not present (%v); the thread took %d page faults writing %d trace events; the tracer's mappings were %v before the events and %v after",
			len(absent), absent, faults, xidBurstLogs, mapped, grown)
	}
	t.Logf("%d trace events in %s, begun %s into the wait for a generation: 0 page faults, the tracer's mappings %v unchanged", xidBurstLogs, to.Sub(from), waited, mapped)
}

// xidAwaitGeneration waits until the trace the sink holds shows a generation it did not show at
// the call, at most xidGenerationWait, and answers how long it waited. The runtime hands the sink
// a generation's batches as the next generation begins, so a generation that has just reached the
// sink is one that has just ended, and the next has just begun. It looks every 20 ms: the burst
// then starts within 20 ms of a generation's start, a second before the next.
func xidAwaitGeneration(t *testing.T, traced *xidTrace) time.Duration {
	t.Helper()
	started := time.Now()
	seen := xidGenerationsIn(traced)
	for xidGenerationsIn(traced) == seen {
		if time.Since(started) > xidGenerationWait {
			t.Fatalf("no trace generation ended in %s, so the burst could not start right after one", xidGenerationWait)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(started)
}

// xidGenerationsIn is how many generations' syncs the trace the sink holds so far shows: the reader
// reads a generation once the next one's first batch has arrived, and stops at the last one. It
// reads the sink's bytes in place, holding the sink's lock, which the runtime's trace reader waits
// on meanwhile.
func xidGenerationsIn(traced *xidTrace) int {
	traced.sink.mu.Lock()
	defer traced.sink.mu.Unlock()
	reader, err := exptrace.NewReader(bytes.NewReader(traced.sink.data.Bytes()))
	if err != nil {
		return 0
	}
	syncs := 0
	for {
		event, err := reader.ReadEvent()
		if err != nil {
			return syncs
		}
		if event.Kind() == exptrace.EventSync {
			syncs++
		}
	}
}

// xidTracerRange is one of the execution tracer's mappings.
type xidTracerRange struct{ start, end uintptr }

// xidTracerMappings lists the execution tracer's mappings (xidTracerMapping).
func xidTracerMappings(t *testing.T) []xidTracerRange {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read /proc/self/maps: %v", err)
	}
	var ranges []xidTracerRange
	for line := range bytes.Lines(maps) {
		if !xidTracerMapping(line) {
			continue
		}
		if start, end, ok := xidMappingRange(bytes.Fields(line)[0]); ok {
			ranges = append(ranges, xidTracerRange{start, end})
		}
	}
	return ranges
}

// xidAbsentPages names each page of ranges that is not present (mincore).
func xidAbsentPages(ranges []xidTracerRange) []string {
	page := uintptr(os.Getpagesize())
	var absent []string
	for _, r := range ranges {
		pages := make([]byte, (r.end-r.start+page-1)/page)
		if _, _, errno := unix.Syscall(unix.SYS_MINCORE, r.start, r.end-r.start, uintptr(unsafe.Pointer(&pages[0]))); errno != 0 {
			absent = append(absent, "mincore: "+errno.Error())
			continue
		}
		for i, resident := range pages {
			if resident&1 == 0 {
				absent = append(absent, "0x"+strconv.FormatUint(uint64(r.start+uintptr(i)*page), 16))
			}
		}
	}
	return absent
}

// xidThreadRusage reads the calling thread's resource usage with a raw system call, which the
// execution tracer writes no event for.
func xidThreadRusage(usage *unix.Rusage) {
	unix.RawSyscall(unix.SYS_GETRUSAGE, uintptr(unix.RUSAGE_THREAD), uintptr(unsafe.Pointer(usage)), 0)
}

// xidStallTheCalibration plants a 50 ms stall into the trace's calibration (xidTieClocks), once,
// right after its first read of the process's clock: as a thread switched out there, or a debugger,
// would stall it. A calibration that took that read would place every moment 50 ms late on the
// trace. A reviewer planted the same stall into the lone pair of reads the calibration took before
// it was bracketed, and a gap of 6.020 ms of the code's CPU then read 0.000 ms made: its stretch of
// the trace fell 60 ms late, where the goroutine was runnable on the runtime.
func xidStallTheCalibration(t *testing.T) {
	stalled := false
	xidTraceNow = func() time.Time {
		now := time.Now()
		if !stalled {
			stalled = true
			time.Sleep(50 * time.Millisecond)
		}
		return now
	}
	t.Cleanup(func() { xidTraceNow = time.Now })
}

// A stall inside the trace's calibration shifts no moment the rig maps onto the trace. With the
// stall planted (xidStallTheCalibration), a moment that two monotonic reads bracket, read after
// the trace started, maps onto the monotonic clock within the bracket and the calibration's bound.
// With the calibration planted to take its first read whatever the reads around it say, the moment
// maps about 50 ms late.
func TestAStallInTheTracesCalibrationShiftsNoMoment(t *testing.T) {
	if trace.IsEnabled() {
		t.Fatal("an execution trace is already running - go test -trace takes one - and this test takes its own; run it without go test -trace")
	}
	xidStallTheCalibration(t)
	traced, err := startXidTrace()
	if err != nil {
		t.Fatalf("start the execution tracer: %v", err)
	}
	defer traced.stop()
	lo, err := xidMonotonic()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	hi, err := xidMonotonic()
	if err != nil {
		t.Fatal(err)
	}
	mapped := traced.baseMono + int64(now.Sub(traced.baseTime))
	if slack := int64(xidClockTieBound); mapped < lo-slack || mapped > hi+slack {
		t.Fatalf("a moment between monotonic %d and %d ns mapped onto %d ns, %s after the later read: the stall shifted the trace's calibration",
			lo, hi, mapped, time.Duration(mapped-hi))
	}
}

// With the trace's calibration stalled 50 ms (xidStallTheCalibration), a window whose gap is 8.5 ms
// of the code's own CPU still breaks D6: a runtime wait the trace shows later in the operation
// cannot stand for the gap's time. Right after the COMMIT the operation holds its goroutine
// runnable on the runtime for 100 ms - one P, and a spinning goroutine it yields to again and
// again - on the thread that sent the window, which it keeps locked to its goroutine from before
// the BEGIN. A calibration that took the stalled read would place the gap's stretch of the trace
// 50 ms late, inside that wait, and read the gap as the runtime's. 8.5 ms is past the widest bound
// a gap's softirqs can set, 7.4 ms, and under the 10 ms the runtime lets a goroutine run before it
// preempts it.
func TestAStalledCalibrationLeavesAGapOfTheCodesCPUMade(t *testing.T) {
	RequireXidGapJudgment(t)
	xidStallTheCalibration(t)
	database := Open(t)
	tracer := NewXidTracer()
	traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
	const spin = 8500 * time.Microsecond
	report := tracer.Measure(t, traced.Pool, func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ctx := MarkXid(context.Background())
		tx, err := traced.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
			t.Fatalf("take a transaction id: %v", err)
		}
		var cpu unix.Timespec
		_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu)
		for from := cpu.Nano(); cpu.Nano()-from < int64(spin); {
			_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu)
		}
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			t.Fatalf("a second statement: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		var stop atomic.Bool
		go func() {
			for !stop.Load() {
			}
		}()
		for until := time.Now().Add(100 * time.Millisecond); time.Now().Before(until); {
			runtime.Gosched()
		}
		stop.Store(true)
	})
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") || !strings.Contains(err.Error(), "follows statement 0") {
		t.Fatalf("a gap of %s of the code's CPU after a stalled calibration: verdict %v, want a D6 violation after the first statement", spin, err)
	}
}

// xidOneGapWindow is a window of two statements and the one gap between them, its statements
// taking 3 ms, as analyzeXidWindows would build it from gap.
func xidOneGapWindow(gap XidGap) XidWindow {
	return XidWindow{
		Transaction:   XidTransaction{PID: 7},
		Statements:    []XidStatement{{SQL: "insert into projects"}, {SQL: "commit"}},
		Gaps:          []XidGap{gap},
		GapTime:       gap.Made(),
		StatementTime: 3 * time.Millisecond,
	}
}

// A window that keeps D6 only by taking its goroutine's wait on the runtime out is UNKNOWN: the gap
// is a reviewer's forcing of 145 of the operation's own goroutines at 72 Ps (2026-11-21), whose
// 49.966 ms runnable on the runtime, taken out, left 0.159 ms made while its thread slept 36 ms. A
// wait on the runtime the thread spent waiting for a CPU decides nothing, and the window keeps D6
// with it counted as the code's; a window that breaks D6 anyway is a violation, not UNKNOWN.
func TestAWindowThatKeepsD6OnlyOnARuntimeWaitIsUnknown(t *testing.T) {
	RequireXidGapJudgment(t)
	us := func(n int64) time.Duration { return time.Duration(n) * time.Microsecond }
	forced := XidGap{Clock: us(50125), Ran: us(1826), Runnable: us(12188), GoRunnable: us(49966), Read: true, Traced: true}
	if made := forced.Made(); made != us(159) {
		t.Fatalf("the forced gap made %s, want 0.159 ms", made)
	}
	if problem := xidRuntimeWaitProblem(xidOneGapWindow(forced)); !strings.Contains(problem, "UNKNOWN: backend 7's window") || !strings.Contains(problem, "49.966 ms in the gap after statement 0") {
		t.Errorf("the forced gap's window: problem %q, want it UNKNOWN, naming the wait", problem)
	}
	queued := XidGap{Clock: us(50000), Ran: us(200), Runnable: us(49700), GoRunnable: us(49000), Read: true, Traced: true}
	if problem := xidRuntimeWaitProblem(xidOneGapWindow(queued)); problem != "" {
		t.Errorf("a runtime wait its thread spent waiting for a CPU: problem %q, want none", problem)
	}
	slept := forced
	slept.GoRunnable = 0
	if problem := xidRuntimeWaitProblem(xidOneGapWindow(slept)); problem != "" || len(xidOneGapWindow(slept).Violations()) == 0 {
		t.Errorf("a gap that breaks D6 with the wait counted as the code's: problem %q, violations %v, want a violation and no problem", problem, xidOneGapWindow(slept).Violations())
	}
}

// An idle P in the trace is no proof that the window's goroutine had a P free: the runtime hands a
// P to a locked goroutine's thread (Go 1.27.3's startlockedm) by stopping it on the thread that
// found the goroutine, where it reads as idle until the locked thread starts it. Here goroutine 1,
// locked to thread 7, is runnable from 1 ms; thread 8 stops P 0 at 1.1 ms and hands it on, and
// thread 7 starts P 0 and runs the goroutine at 51 ms, while its clocks say it slept 38 ms. P 0 is
// idle in the trace that whole time, and the window is still UNKNOWN: a reviewer's control that
// laid such a wait to the machine read 49.966 ms runnable as 159 µs made (2026-11-21).
func TestAWaitOnAPHandedToALockedThreadDecidesNothing(t *testing.T) {
	RequireXidGapJudgment(t)
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
		{exptrace.MakeProcStateTransition(0, exptrace.ProcUndetermined, exptrace.ProcRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), 1, 7},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcRunning, exptrace.ProcIdle), 1.1, 8},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcIdle, exptrace.ProcRunning), 51, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunning), 51, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoWaiting), 60, 7},
	} {
		reading.transition(event.change, at(event.at), event.thread)
	}
	states, ok := reading.goStates(7, base.Add(500*time.Microsecond), base.Add(51500*time.Microsecond))
	if !ok {
		t.Fatal("the trace shows no goroutine on thread 7 by the gap's start")
	}
	if want := 50 * time.Millisecond; states.Runnable != want {
		t.Fatalf("runnable %s, want %s", states.Runnable, want)
	}
	gap := XidGap{Clock: 51 * time.Millisecond, Ran: time.Millisecond, Runnable: 12 * time.Millisecond, GoRunnable: states.Runnable, Read: true, Traced: true}
	if made := gap.Made(); made != time.Millisecond {
		t.Fatalf("the gap made %s, want 1 ms with the wait taken out", made)
	}
	if problem := xidRuntimeWaitProblem(xidOneGapWindow(gap)); !strings.Contains(problem, "UNKNOWN: backend 7's window") || !strings.Contains(problem, "50.000 ms in the gap after statement 0") {
		t.Errorf("a wait on a P handed to the goroutine's own thread: problem %q, want the window UNKNOWN", problem)
	}
}

// A reviewer's counterexample to the deduction 8a194a509e made and c9d41b9caa withdrew (Review round
// 14, item 4): the runtime's handoff of a P to a locked thread does not prove the thread slept. Here
// goroutine 1, locked to thread 7, goes runnable at 0.1 ms; thread 7 stops its P only at 10.1 ms -
// queued for a CPU till then, which its run_delay counts - thread 8 starts that P and stops it at
// 10.2 ms, handing it on, and thread 7 starts it and runs the goroutine at 10.3 ms; then the
// goroutine sleeps 6 ms in a system call. Over the 16.5 ms gap the thread ran 0.3 ms, queued
// 10.1 ms and slept 6.1 ms. The gap is the code's 6.3 ms, and the window breaks D6. This row checks
// Made's arithmetic on that shape - no wait on the runtime erases the sleep - not 8a194a509e's rule:
// that rule deducted only where it knew the process's Ps, which it read from the trace's
// gomaxprocs metric, and a reading built through transition() has none, so on 8a194a509e this
// input reads no deduction and the row stays green. Given the Ps, 8a194a509e read 10.1 ms handed
// off and 0.3 ms made, and the window passed (the reviewer's replay, 2026-11-21).
func TestASleepAfterARuntimeHandoffIsNotErasedByIt(t *testing.T) {
	RequireXidGapJudgment(t)
	base := time.Now()
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	at := func(v float64) exptrace.Time { return exptrace.Time(ms(v)) }
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
		{exptrace.MakeProcStateTransition(0, exptrace.ProcUndetermined, exptrace.ProcRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), 0, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), 0.1, 7},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcRunning, exptrace.ProcIdle), 10.1, 7},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcIdle, exptrace.ProcRunning), 10.15, 8},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcRunning, exptrace.ProcIdle), 10.2, 8},
		{exptrace.MakeProcStateTransition(0, exptrace.ProcIdle, exptrace.ProcRunning), 10.3, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunning), 10.3, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoSyscall), 10.4, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoSyscall, exptrace.GoRunning), 16.4, 7},
		{exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoWaiting), 17, 7},
	} {
		reading.transition(event.change, at(event.at), event.thread)
	}
	gap := xidStretch(base, base.Add(ms(16.5)),
		xidThreadClock{thread: 7, at: base},
		xidThreadClock{thread: 7, at: base.Add(ms(16.5)), ran: ms(0.3), waited: ms(10.1)}, 7, reading)
	window := xidOneGapWindow(gap)
	broken := strings.Join(window.Violations(), "; ")
	t.Logf("the gap made %s; violations: %s", gap.Made(), broken)
	if !strings.Contains(broken, "a statement-free gap of 6.300 ms") {
		t.Fatalf("a 6 ms sleep in a system call after a runtime handoff: violations %q, want a D6 violation of the 6.3 ms gap", broken)
	}
}

// xidLockedRunsFrom is a trace in which goroutine 1, locked to thread 7, is first seen running at
// from ms, and is runnable on the runtime, its thread asleep, over each of runnable's [start, end]
// stretches; it runs again after each, until it waits at 14 ms.
func xidLockedRunsFrom(base time.Time, from float64, runnable ...[2]float64) *xidTraceReading {
	at := func(ms float64) exptrace.Time { return exptrace.Time(ms * float64(time.Millisecond)) }
	reading := &xidTraceReading{
		baseTime: base,
		snapshot: exptrace.ClockSnapshot{Trace: 0, Mono: 0},
		changes:  map[exptrace.GoID][]xidGoStateChange{},
		runs:     map[int][]xidThreadRun{},
	}
	reading.transition(exptrace.MakeProcStateTransition(0, exptrace.ProcUndetermined, exptrace.ProcRunning), at(from), 7)
	reading.transition(exptrace.MakeGoStateTransition(1, exptrace.GoUndetermined, exptrace.GoRunning), at(from), 7)
	for _, stretch := range runnable {
		reading.transition(exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoRunnable), at(stretch[0]), 7)
		reading.transition(exptrace.MakeProcStateTransition(0, exptrace.ProcRunning, exptrace.ProcIdle), at(stretch[0]), 7)
		reading.transition(exptrace.MakeProcStateTransition(0, exptrace.ProcIdle, exptrace.ProcRunning), at(stretch[1]), 7)
		reading.transition(exptrace.MakeGoStateTransition(1, exptrace.GoRunnable, exptrace.GoRunning), at(stretch[1]), 7)
	}
	reading.transition(exptrace.MakeGoStateTransition(1, exptrace.GoRunning, exptrace.GoWaiting), at(14), 7)
	return reading
}

// Rule 2 weighs the time the code made in a window's gaps against the time it made in its
// statements (XidWindow.StatementTime), never against the statements' time on the clock. Here one
// window, the same code three times: its write's thread ran 1 ms, the gap after it 3 ms of the
// code's CPU and the COMMIT 0.1 ms, so rule 2 breaks, 3 ms of gaps against 1.1 ms of statements.
// In the second copy the write's thread also waited 9 ms for a CPU inside the statement, which its
// run_delay counts; in the third the runtime kept its goroutine runnable 9 ms inside it while its
// locked thread slept, which the trace shows. Both waits are the machine's, and rule 2 still
// breaks. With the statements read off the clock, as a tester planted at 15d679ca57, the loaded
// copies kept rule 2 on 10.1 ms of statements and every other xid row passed (2026-11-22); with the
// statements read without the trace, the third copy keeps it.
func TestRuleTwoReadsTheStatementsTheCodeMade(t *testing.T) {
	RequireXidGapJudgment(t)
	base := time.Now()
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	clock := func(at, ran, waited float64) xidThreadClock {
		return xidThreadClock{thread: 7, at: base.Add(ms(at)), ran: ms(ran), waited: ms(waited)}
	}
	for _, test := range []struct {
		name        string
		end, queued float64
		reading     *xidTraceReading
	}{
		{"the write ran 1 ms", 1, 0, nil},
		{"the write ran 1 ms and its thread waited 9 ms for a CPU", 10, 9, nil},
		{"the write ran 1 ms and the runtime kept its goroutine runnable 9 ms", 10, 0, xidLockedRunsFrom(base, 0, [2]float64{0.5, 9.5})},
	} {
		write := XidStatement{PID: 7, SQL: "insert into projects values (1)", Marked: true, Wrote: true, locked: 7,
			Start: base, End: base.Add(ms(test.end)), started: clock(0, 0, 0), ended: clock(test.end, 1, test.queued)}
		commit := XidStatement{PID: 7, SQL: "commit", Marked: true, locked: 7,
			Start: base.Add(ms(test.end + 3)), End: base.Add(ms(test.end + 3.1)),
			started: clock(test.end+3, 4, test.queued), ended: clock(test.end+3.1, 4.1, test.queued)}
		window := xidWindowOf(XidTransaction{PID: 7, Statements: []XidStatement{write, commit}, Committed: true}, test.reading)
		broken := strings.Join(window.Violations(), "; ")
		t.Logf("%s: statements %s, gaps %s; violations: %s", test.name, XidMillis(window.StatementTime), XidMillis(window.GapTime), broken)
		if !strings.Contains(broken, "its gaps take 3.000 ms, more than its statements' 1.100 ms") {
			t.Errorf("%s: violations %q, want rule 2 broken, 3 ms of the code's gaps against 1.1 ms of its statements", test.name, broken)
		}
	}
}

// A statement of a window that the rig could not read whole is a rig problem: rule 2 would weigh
// the gaps against its time on the clock, where its clocks were not read at both ends, or with the
// runtime's wait inside it, where the trace does not show its goroutine by its start - each of
// which a loaded machine stretches. A gap's own check covers the start of every statement but the
// first and the end of every statement but the last, so the first statement's start and the last
// one's end are this check's alone. In the first two cases the write's thread ran 1 ms of its
// 10 ms and slept the rest; in the third the write ran 1 ms and the COMMIT took 9 ms whose end was
// not read. The 3 ms gap between them is the code's CPU. Read so, the statements hold at least
// 10 ms, so the window keeps rule 2 and breaks no rule; the problem is all that stops the run
// passing on it. A tester's plant at 65c9bcb34f that left the last statement out of the check
// passed every xid row.
func TestAStatementTheRigCouldNotReadIsUnknown(t *testing.T) {
	RequireXidGapJudgment(t)
	base := time.Now()
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	clock := func(at, ran float64) xidThreadClock {
		return xidThreadClock{thread: 7, at: base.Add(ms(at)), ran: ms(ran)}
	}
	for _, test := range []struct {
		name                             string
		started                          xidThreadClock
		writeEnd, commitStart, commitEnd float64
		ended                            xidThreadClock
		reading                          *xidTraceReading
		want                             string
	}{
		{"its clocks unread at its start", xidThreadClock{}, 10.2, 13.2, 13.3, clock(13.3, 4.2), xidLockedRunsFrom(base, 0),
			"statement 0 of backend 7's window (insert into projects values (1)) was not read off one thread's clocks at its start and end"},
		{"its goroutine first traced after its start", clock(0.2, 0.1), 10.2, 13.2, 13.3, clock(13.3, 4.2), xidLockedRunsFrom(base, 5),
			"statement 0 of backend 7's window (insert into projects values (1)) was not read off the execution trace"},
		{"the COMMIT's clocks unread at its end", clock(0.2, 0.1), 1.2, 4.2, 13.2, xidThreadClock{}, xidLockedRunsFrom(base, 0),
			"statement 1 of backend 7's window (commit) was not read off one thread's clocks at its start and end"},
	} {
		statements := []XidStatement{
			{PID: 7, SQL: "begin", Marked: true, locked: 7, Start: base, End: base.Add(ms(0.1)), started: clock(0, 0), ended: clock(0.1, 0.1)},
			{PID: 7, SQL: "insert into projects values (1)", Marked: true, Wrote: true, locked: 7,
				Start: base.Add(ms(0.2)), End: base.Add(ms(test.writeEnd)), started: test.started, ended: clock(test.writeEnd, 1.1)},
			{PID: 7, SQL: "commit", Marked: true, locked: 7,
				Start: base.Add(ms(test.commitStart)), End: base.Add(ms(test.commitEnd)), started: clock(test.commitStart, 4.1), ended: test.ended},
		}
		report := analyzeXidWindows(statements, nil, 0, test.reading)
		if len(report.Windows) != 1 || len(report.Windows[0].Violations()) != 0 {
			t.Fatalf("%s: windows %d, want one that breaks no rule, or this row proves nothing", test.name, len(report.Windows))
		}
		t.Logf("%s: %s; problems: %q", test.name, report.Windows[0].Summary(), report.Problems)
		if len(report.Problems) != 1 || !strings.Contains(report.Problems[0], test.want) {
			t.Errorf("%s: problems %q, want only %q", test.name, report.Problems, test.want)
		}
		if err := report.Err(); err == nil || !strings.Contains(err.Error(), "XID RIG PROBLEM") {
			t.Errorf("%s: verdict %v, want a rig problem", test.name, err)
		}
	}
}

// A COMMIT that begins before the capture ends and ends after it is inside the trace the rig judges
// its window by: Measure copies the statements done by the capture's end before it stops the
// tracer, and marks the trace after the copy. Here a goroutine of the operation sends a marked
// transaction whose COMMIT runs a deferred trigger that sleeps 200 ms in the server, and the
// operation returns once the rig has begun that COMMIT's span; the hook xidCaptureEnded, between
// the capture's end and the copy, waits for the COMMIT to end. The window is judged whole and
// keeps D6. With the tracer stopped before the copy, as a broadcast's late COMMIT found it at
// fb9f413303, the window ends past the trace and reads UNKNOWN.
func TestACommitThatEndsAfterTheCaptureIsInsideTheTrace(t *testing.T) {
	RequireXidGapJudgment(t)
	database := Open(t)
	ctx := context.Background()
	for _, ddl := range []string{
		`create table xid_late_commit (id int)`,
		`create function xid_late_commit_wait() returns trigger language plpgsql as $$ begin perform pg_sleep(0.2); return null; end $$`,
		`create constraint trigger xid_late_commit_wait after insert on xid_late_commit deferrable initially deferred for each row execute function xid_late_commit_wait()`,
	} {
		if _, err := database.Pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	tracer := NewXidTracer()
	traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
	committed := make(chan error, 1)
	var captureEnded time.Time
	xidCaptureEnded = func() {
		captureEnded = time.Now()
		if err := <-committed; err != nil {
			t.Errorf("the late COMMIT: %v", err)
		}
	}
	t.Cleanup(func() { xidCaptureEnded = nil })
	commitBegun := func() bool {
		tracer.mu.Lock()
		defer tracer.mu.Unlock()
		for _, span := range tracer.spans {
			if span.ends {
				return true
			}
		}
		return false
	}
	report := tracer.Measure(t, traced.Pool, func() {
		opened := make(chan error, 1)
		go func() {
			ctx := MarkXid(context.Background())
			tx, err := traced.Pool.Begin(ctx)
			if err == nil {
				if _, err = tx.Exec(ctx, "insert into xid_late_commit values (1)"); err != nil {
					_ = tx.Rollback(ctx)
				}
			}
			opened <- err
			if err != nil {
				committed <- err
				return
			}
			committed <- tx.Commit(ctx)
		}()
		if err := <-opened; err != nil {
			t.Fatalf("open the transaction: %v", err)
		}
		for deadline := time.Now().Add(10 * time.Second); !commitBegun(); time.Sleep(100 * time.Microsecond) {
			if time.Now().After(deadline) {
				t.Fatal("the rig recorded no COMMIT within 10 s")
			}
		}
	})
	if err := report.Err(); err != nil {
		t.Fatalf("a COMMIT that ended after the capture: verdict %v, want its window judged whole within the trace", err)
	}
	if len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows, want the one transaction", len(report.Windows))
	}
	window := report.Windows[0]
	commit := window.Statements[len(window.Statements)-1]
	if xidVerb(commit.SQL) != "commit" || !commit.End.After(captureEnded) {
		t.Fatalf("the window ends with %q at %s, %s after the capture ended: want the COMMIT, ending after it, or this run proves nothing\n%s",
			commit.SQL, commit.End.Format(time.RFC3339Nano), commit.End.Sub(captureEnded), window.Detail())
	}
	t.Logf("the COMMIT ended %s after the capture did\n%s", commit.End.Sub(captureEnded), window.Detail())
}
