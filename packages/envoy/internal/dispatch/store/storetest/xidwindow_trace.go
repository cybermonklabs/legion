//go:build linux

package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"

	exptrace "golang.org/x/exp/trace"
	"golang.org/x/sys/unix"
)

// The Go runtime can keep a window's goroutine from running without the code asking it to: it
// preempts a goroutine that has run for 10 ms of the clock (sysmon's forcePreemptNS), and stops
// every goroutine for a stop of the world. A goroutine locked to its thread, as a marked
// transaction's is (XidTracer.lockTransaction), cannot be run again by the thread that was running
// it: the thread parks until another thread picks the goroutine up and hands its P back. That
// wait is the time another thread takes to be given a CPU - on a loaded machine, milliseconds -
// and the measured thread spends it asleep, which its scheduler clocks cannot tell from a sleep
// the code asked for (traced at a load of 250 to 350 on a 288-CPU machine, 2026-11-20: a thread
// parked 4.4 and 7.1 ms in runtime.stoplockedm after its goroutine was preempted). A gap the
// machine's load stretched past the runtime's 10 ms slice meets it every time.
//
// So the rig takes an execution trace of the operation (runtime/trace) and reads, for the
// goroutine that sends a window's statements, the time of each gap it spent runnable but not
// running - waiting for the runtime to run it again - the time it spent waiting on something it
// asked for (a sleep, a lock, a channel, the network) and the time it spent in a system call
// (XidGap.Made).

// xidTrace is an execution trace Measure is taking, and the moment it started on both the
// process's clock and the system's monotonic clock, which the trace's own clock is tied to.
type xidTrace struct {
	sink     xidTraceSink
	baseTime time.Time
	baseMono int64
}

// xidTraceSink is the trace's writer. It keeps the bytes the runtime's trace reader hands it, and
// closes a channel once it holds as many as a waiter asked for (arrived). The reader writes from a
// goroutine of its own while the trace runs; the trace is read back only after trace.Stop, which
// returns once every write has completed.
type xidTraceSink struct {
	mu      sync.Mutex
	data    bytes.Buffer
	want    int
	reached chan struct{}
}

func (s *xidTraceSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Write(p)
	if s.reached != nil && s.data.Len() >= s.want {
		close(s.reached)
		s.reached = nil
	}
	return len(p), nil
}

// arrived answers a channel closed once the sink holds at least n bytes.
func (s *xidTraceSink) arrived(n int) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	reached := make(chan struct{})
	if s.data.Len() >= n {
		close(reached)
		return reached
	}
	s.want, s.reached = n, reached
	return reached
}

// startXidTrace starts the execution tracer. It stops the world to start, so Measure starts it
// before the operation and none of that pause is inside a window. Right before the start it keeps
// every address range the process has mapped (xidMappedBeforeTrace), against which the hold finds
// the tracer's memory on a kernel that names no anonymous mapping (xidTracerRanges). Then it ties
// the process's clock to the system's monotonic clock (xidTieClocks) and fills the runtime's pool
// of trace buffers (warm).
func startXidTrace() (*xidTrace, error) {
	x := &xidTrace{}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return nil, fmt.Errorf("read /proc/self/maps, against which the hold finds the tracer's memory: %w", err)
	}
	before := xidMappedRanges(maps)
	xidMappedBeforeTrace.Store(&before)
	if err := trace.Start(&x.sink); err != nil {
		xidMappedBeforeTrace.Store(nil)
		return nil, err
	}
	if x.baseTime, x.baseMono, err = xidTieClocks(xidMonotonic, xidTraceNow); err != nil {
		x.stop()
		return nil, err
	}
	x.warm()
	return x, nil
}

// xidTraceNow reads the process's clock for the trace's calibration (xidTieClocks). A variable, so
// that the test of a stall in the calibration can plant one.
var xidTraceNow = time.Now

// xidMonotonic reads the system's monotonic clock, the one the trace's clock is tied to.
func xidMonotonic() (int64, error) {
	var mono unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono)
	return mono.Nano(), err
}

// xidClockTieBound is the most the two monotonic reads xidTieClocks takes around a read of the
// process's clock may lie apart for the pair to stand. Read back to back, the three reads take well
// under a microsecond; a thread the kernel switched out between them, or a stall of any kind, puts
// them far further apart. The midpoint of the two reads then stands for the moment of the process's
// clock's read to within half the bound, 10 us, so every moment the rig maps onto the trace is
// placed to within that.
const xidClockTieBound = 20 * time.Microsecond

// xidClockTieTries is how many times xidTieClocks reads the three clocks before it gives up.
const xidClockTieTries = 1000

// xidTieClocks ties the process's clock (now) to the system's monotonic clock (mono): it reads the
// monotonic clock, then the process's clock, then the monotonic clock again, and takes the pair
// only where the two monotonic reads lie within xidClockTieBound, with their midpoint as the
// monotonic moment of the process's read. A stall between two reads - the thread switched out, a
// debugger's stop - would otherwise shift every moment the rig maps onto the trace by as long as
// the stall: a 50 ms stall between a lone pair of reads shifted a gap's stretch of the trace onto
// a later one, which showed the goroutine runnable throughout, and a gap of 6.020 ms of the code's
// CPU read 0.000 ms made. It reads the three again where the bound is not met, and answers an error
// after xidClockTieTries such reads, naming the closest pair it read.
func xidTieClocks(mono func() (int64, error), now func() time.Time) (time.Time, int64, error) {
	closest := time.Duration(-1)
	for range xidClockTieTries {
		before, err := mono()
		if err != nil {
			return time.Time{}, 0, err
		}
		wall := now()
		after, err := mono()
		if err != nil {
			return time.Time{}, 0, err
		}
		spread := time.Duration(after - before)
		if spread >= 0 && spread <= xidClockTieBound {
			return wall, before + (after-before)/2, nil
		}
		if closest < 0 || (spread >= 0 && spread < closest) {
			closest = spread
		}
	}
	return time.Time{}, 0, fmt.Errorf("could not tie the process's clock to the monotonic clock: in %d reads the two monotonic reads around it never lay within %s, the closest %s apart",
		xidClockTieTries, xidClockTieBound, closest)
}

// xidTraceBatch is the size of one of the runtime's trace buffers, the largest batch the trace
// format allows (internal/trace/tracev2.MaxBatchSize, 64 KiB).
const xidTraceBatch = 64 << 10

// xidTracePool is how many used trace buffers warm leaves in the runtime's pool beyond one for
// each of the process's threads. Each thread that writes an event takes a buffer of its own from
// the pool, and a window's thread writes a few kilobytes of events in a window, so a window that
// fills its thread's buffer finds one to take even after every other thread has taken its own.
const xidTracePool = 8

// xidTraceWarmMessage is a trace log message of the most bytes the runtime writes of one
// (tracev2.MaxEventTrailerDataSize, 1 KiB), so warm fills a buffer in 64 logs.
var xidTraceWarmMessage = strings.Repeat("x", 1<<10)

// warm leaves at least one used trace buffer for each of the process's threads, and
// xidTracePool more, written whole, read, and back in the runtime's pool before the operation
// runs. When a thread fills its trace buffer, or writes its first event, the runtime takes a used
// buffer from that pool, and maps a new one only when the pool is empty (runtime/tracebuf.go,
// traceWriter.refill). A trace starts with an empty pool, since stopping the last one freed every
// buffer. Without warm, a window whose thread fills its buffer has the runtime map a new one
// inside a gap. The first write to each page of it is then a page fault inside that gap, and the
// fault takes the page table's lock, which another thread of the process may hold. On a loaded
// machine a fault waited 11.3 ms for it, on the thread's CPU, behind 0.25 ms of the code's work
// (traced at a load of about 260 on a 288-CPU machine, 2026-11-20), and the gap read 12.6 ms made.
// With a pool of 8 and no buffer for each thread, a thread writing three buffers of events found
// the pool empty in 28 of 200 runs with the collector running, and 1 of 300 with it held off
// (TestTheTracersEventsInAWindowTouchNoFreshPage); with a buffer for each thread, in 0 of 300.
//
// It logs messages that go whole into the trace's string buffers, enough to fill three more of
// them than the pool is to hold, and waits until the trace's reader has handed the sink more bytes
// than the pool and one buffer more hold. That many bytes come from at least two buffers more than
// the pool, and the runtime puts each buffer back in the pool once the reader reads the next
// (ReadTrace). The wait ends on those bytes arriving: the runtime flushes a buffer once it is full,
// and every buffer at the trace's next generation, a second after it starts.
func (x *xidTrace) warm() {
	pool := xidTracePool + xidThreadCount()
	reached := x.sink.arrived((pool + 2) * xidTraceBatch)
	ctx := context.Background()
	for range (pool + 4) * (xidTraceBatch / len(xidTraceWarmMessage)) {
		trace.Log(ctx, "xid rig", xidTraceWarmMessage)
	}
	<-reached
}

// xidThreadCount is how many threads the process has (/proc/self/task), or none when it cannot
// be read.
func xidThreadCount() int {
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return 0
	}
	return len(threads)
}

// xidGoStateChange is a goroutine's state from a moment of the trace on.
type xidGoStateChange struct {
	at    exptrace.Time
	state exptrace.GoState
}

// xidThreadRun is a goroutine starting to run on a thread, at a moment of the trace.
type xidThreadRun struct {
	at exptrace.Time
	g  exptrace.GoID
}

// xidTraceReading is the execution trace read back: each goroutine's state changes in time order,
// each thread's goroutines in the order they started running on it, how far the trace reaches,
// and the tie between the trace's clock and the process's.
type xidTraceReading struct {
	changes  map[exptrace.GoID][]xidGoStateChange
	runs     map[int][]xidThreadRun
	baseTime time.Time
	baseMono int64
	snapshot exptrace.ClockSnapshot
	// syncs are the moments each of the trace's generations began. At each the runtime flushes
	// every thread's buffer and starts its string and stack tables anew.
	syncs []exptrace.Time
	// last is the latest event's moment: the trace says nothing of any goroutine after it.
	last exptrace.Time
}

// stop stops the tracer. It stops the world to stop, so Measure stops it once the operation has
// returned and the capture has ended, after every window it records. A second stop does nothing.
func (x *xidTrace) stop() {
	trace.Stop()
	xidMappedBeforeTrace.Store(nil)
}

// read reads the stopped trace back. A trace the reader cannot read is named by its header and the
// toolchain that wrote it: golang.org/x/exp/trace reads the trace formats it was built to read,
// and a toolchain whose runtime writes a newer one needs x/exp moved on with it.
func (x *xidTrace) read() (*xidTraceReading, error) {
	header := bytes.Clone(x.sink.data.Bytes()[:min(x.sink.data.Len(), 16)])
	cannot := func(err error) error {
		return fmt.Errorf("read the execution trace, whose header is %q, written by %s, with golang.org/x/exp %s: %w; a toolchain whose trace format x/exp/trace does not read needs golang.org/x/exp moved to a version that reads it",
			bytes.TrimRight(header, "\x00"), runtime.Version(), xidExpVersion(), err)
	}
	reader, err := exptrace.NewReader(&x.sink.data)
	if err != nil {
		return nil, cannot(err)
	}
	reading := &xidTraceReading{
		changes:  map[exptrace.GoID][]xidGoStateChange{},
		runs:     map[int][]xidThreadRun{},
		baseTime: x.baseTime,
		baseMono: x.baseMono,
	}
	snapped := false
	for {
		event, err := reader.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, cannot(err)
		}
		reading.last = max(reading.last, event.Time())
		switch event.Kind() {
		case exptrace.EventSync:
			reading.syncs = append(reading.syncs, event.Time())
			if snapshot := event.Sync().ClockSnapshot; snapshot != nil && !snapped {
				reading.snapshot = *snapshot
				snapped = true
			}
		case exptrace.EventStateTransition:
			reading.transition(event.StateTransition(), event.Time(), event.Thread())
		}
	}
	if !snapped {
		return nil, errors.New("the execution trace holds no clock snapshot to tie its clock to the process's")
	}
	return reading, nil
}

// transition records one goroutine state transition of the trace, at, on thread: the goroutine's
// new state, and its start on thread when it starts running there. A transition from a state to
// that same state changes nothing and is passed over: at each generation's start the trace writes
// every goroutine's status as a transition from the state it was already in (x/exp/trace's
// order.go). A P's transitions are not read: the trace cannot show that an idle P was free to run
// a goroutine, since the runtime hands a P to a locked goroutine's thread (startlockedm) through a
// stop that reads as idle until that thread starts it, so no wait for a P is laid to the machine
// on a P's account (xidRuntimeWaitProblem).
func (r *xidTraceReading) transition(change exptrace.StateTransition, at exptrace.Time, thread exptrace.ThreadID) {
	r.last = max(r.last, at)
	if change.Resource.Kind != exptrace.ResourceGoroutine {
		return
	}
	g := change.Resource.Goroutine()
	from, to := change.Goroutine()
	if from == to {
		return
	}
	r.changes[g] = append(r.changes[g], xidGoStateChange{at: at, state: to})
	if to == exptrace.GoRunning && thread != exptrace.NoThread {
		r.runs[int(thread)] = append(r.runs[int(thread)], xidThreadRun{at: at, g: g})
	}
}

// pastEnd is how much of the stretch up to moment lies past the trace's last event, where the
// trace says nothing of any goroutine: zero when the trace reaches moment.
func (r *xidTraceReading) pastEnd(moment time.Time) time.Duration {
	return max(time.Duration(r.traceTime(moment)-r.last), 0)
}

// xidExpVersion is the golang.org/x/exp this binary was built with, as the build records it.
func xidExpVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, module := range info.Deps {
			if module.Path == "golang.org/x/exp" {
				return module.Version
			}
		}
	}
	return "(version unrecorded)"
}

// traceTime is moment on the trace's clock.
func (r *xidTraceReading) traceTime(moment time.Time) exptrace.Time {
	mono := r.baseMono + int64(moment.Sub(r.baseTime))
	return r.snapshot.Trace + exptrace.Time(mono-int64(r.snapshot.Mono))
}

// goStates are the states of the goroutine running on thread at from, over from to to (xidGoStates),
// read only as far as the trace reaches: a goroutine's last state is not carried past the trace's
// last event, where the trace says nothing (pastEnd). ok is false when the trace shows no
// goroutine running on thread by from.
func (r *xidTraceReading) goStates(thread int, from, to time.Time) (xidGoStates, bool) {
	start, end := r.traceTime(from), min(r.traceTime(to), r.last)
	runs := r.runs[thread]
	last := slices.IndexFunc(runs, func(run xidThreadRun) bool { return run.at > start }) - 1
	if last == -2 {
		last = len(runs) - 1
	}
	if last < 0 {
		return xidGoStates{}, false
	}
	var states xidGoStates
	changes := r.changes[runs[last].g]
	for i, change := range changes {
		segmentEnd := end
		if i+1 < len(changes) {
			segmentEnd = min(changes[i+1].at, end)
		}
		segmentStart := max(change.at, start)
		if segmentEnd <= segmentStart {
			continue
		}
		switch change.state {
		case exptrace.GoRunnable:
			states.Runnable += segmentEnd.Sub(segmentStart)
		case exptrace.GoWaiting:
			states.Waiting += segmentEnd.Sub(segmentStart)
		case exptrace.GoSyscall:
			states.Syscall += segmentEnd.Sub(segmentStart)
		}
	}
	return states, true
}
