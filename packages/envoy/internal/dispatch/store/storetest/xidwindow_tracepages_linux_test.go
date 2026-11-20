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
	"testing"
	"time"
	"unsafe"

	exptrace "golang.org/x/exp/trace"
	"golang.org/x/sys/unix"
)

// xidBurstLogs is how many trace events the burst of TestTheTracersEventsInAWindowTouchNoFreshPage
// writes: each empty log is a few bytes of the thread's buffer, so these fill more than three of
// the runtime's 64 KiB trace buffers, and the thread takes a buffer from the pool at least three
// times.
const xidBurstLogs = 1 << 15

// xidBurstRuns is the most runs TestTheTracersEventsInAWindowTouchNoFreshPage makes. A run is made
// again only when the trace shows one of its generations beginning inside the burst: at each the
// runtime starts the trace's string and stack tables anew, on blocks it maps then.
const xidBurstRuns = 4

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
// starts.
func TestTheTracersEventsInAWindowTouchNoFreshPage(t *testing.T) {
	if trace.IsEnabled() {
		t.Fatal("an execution trace is already running - go test -trace takes one - and this test takes its own; run it without go test -trace")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx := context.Background()
	for run := 1; run <= xidBurstRuns; run++ {
		var traced *xidTrace
		var before, after unix.Rusage
		var from, to time.Time
		var absent []string
		var mapped, grown []xidTracerRange
		// As Measure does, with the collector held off, so none of its workers writes events.
		runWithoutCollector(func() {
			var err error
			if traced, err = startXidTrace(); err != nil {
				t.Fatalf("start the execution tracer: %v", err)
			}
			defer traced.stop()
			// One call site writes every event, so the first puts the burst's category and stack in
			// the trace's tables before the hold, as a window's statements before a gap do.
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
		start, end := reading.traceTime(from), reading.traceTime(to)
		if slices.ContainsFunc(reading.syncs, func(at exptrace.Time) bool { return at > start && at < end }) {
			t.Logf("run %d: a trace generation began inside the burst, and the runtime maps the trace's new tables then, so the run proves nothing and is made again", run)
			continue
		}
		faults := after.Minflt - before.Minflt
		if len(absent) > 0 || faults != 0 || !slices.Equal(grown, mapped) {
			t.Fatalf("run %d, with no trace generation beginning in it: after the hold %d pages of the tracer's memory were not present (%v); the thread took %d page faults writing %d trace events; the tracer's mappings were %v before the events and %v after", run, len(absent), absent, faults, xidBurstLogs, mapped, grown)
		}
		t.Logf("run %d: %d trace events, 0 page faults, the tracer's mappings %v unchanged", run, xidBurstLogs, mapped)
		return
	}
	t.Fatalf("in %d runs a trace generation began inside every burst, so no run could tell", xidBurstRuns)
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
