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
