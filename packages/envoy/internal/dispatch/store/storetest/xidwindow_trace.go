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
// asked for (a sleep, a lock, a channel, the network) and the time it spent in a system call; and
// how much of its runnable time its thread spent woken but waiting on the kernel's run queue,
// which the thread's own clocks count as well (xidGoStates.Handoff).

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
// before the operation and none of that pause is inside a window. Then it fills the runtime's pool
// of trace buffers (warm).
func startXidTrace() (*xidTrace, error) {
	x := &xidTrace{}
	if err := trace.Start(&x.sink); err != nil {
		return nil, err
	}
	var mono unix.Timespec
	x.baseTime = time.Now()
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		trace.Stop()
		return nil, err
	}
	x.baseMono = mono.Nano()
	x.warm()
	return x, nil
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

// xidProcStart is a P starting to run on a thread, and when the P last stopped before it.
type xidProcStart struct {
	at, stopped exptrace.Time
}

// xidTraceReading is the execution trace read back: each goroutine's state changes in time order,
// each thread's goroutines in the order they started running on it, each thread's starts of a P,
// and the tie between the trace's clock and the process's.
type xidTraceReading struct {
	changes  map[exptrace.GoID][]xidGoStateChange
	runs     map[int][]xidThreadRun
	starts   map[int][]xidProcStart
	baseTime time.Time
	baseMono int64
	snapshot exptrace.ClockSnapshot
	// syncs are the moments each of the trace's generations began. At each the runtime flushes
	// every thread's buffer and starts its string and stack tables anew.
	syncs []exptrace.Time
}

// stop stops the tracer. It stops the world to stop, so Measure stops it once the operation has
// returned, after every window.
func (x *xidTrace) stop() {
	trace.Stop()
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
		starts:   map[int][]xidProcStart{},
		baseTime: x.baseTime,
		baseMono: x.baseMono,
	}
	stopped := map[exptrace.ProcID]exptrace.Time{}
	snapped := false
	for {
		event, err := reader.ReadEvent()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, cannot(err)
		}
		switch event.Kind() {
		case exptrace.EventSync:
			reading.syncs = append(reading.syncs, event.Time())
			if snapshot := event.Sync().ClockSnapshot; snapshot != nil && !snapped {
				reading.snapshot = *snapshot
				snapped = true
			}
		case exptrace.EventStateTransition:
			reading.transition(event.StateTransition(), event.Time(), event.Thread(), stopped)
		}
	}
	if !snapped {
		return nil, errors.New("the execution trace holds no clock snapshot to tie its clock to the process's")
	}
	return reading, nil
}

// transition records one state transition of the trace, at, on thread: a goroutine's new state,
// and its start on thread when it starts running there; a P's stop, kept in stopped, and its
// start on thread, with the moment it last stopped. A transition from a state to that same state
// changes nothing and is passed over: at each generation's start the trace writes every
// goroutine's and every P's status as a transition from the state it was already in (x/exp/trace's
// order.go), so an idle P's status there would otherwise read as a new stop, and a handoff that
// spans a generation's start would read its run-queue wait (Handoff) from that moment rather than
// from the P's real stop, and the gap would take that wait out twice.
func (r *xidTraceReading) transition(change exptrace.StateTransition, at exptrace.Time, thread exptrace.ThreadID, stopped map[exptrace.ProcID]exptrace.Time) {
	switch change.Resource.Kind {
	case exptrace.ResourceGoroutine:
		g := change.Resource.Goroutine()
		from, to := change.Goroutine()
		if from == to {
			return
		}
		r.changes[g] = append(r.changes[g], xidGoStateChange{at: at, state: to})
		if to == exptrace.GoRunning && thread != exptrace.NoThread {
			r.runs[int(thread)] = append(r.runs[int(thread)], xidThreadRun{at: at, g: g})
		}
	case exptrace.ResourceProc:
		p := change.Resource.Proc()
		from, to := change.Proc()
		if from == to {
			return
		}
		switch to {
		case exptrace.ProcIdle:
			stopped[p] = at
		case exptrace.ProcRunning:
			if last, ok := stopped[p]; ok && thread != exptrace.NoThread {
				r.starts[int(thread)] = append(r.starts[int(thread)], xidProcStart{at: at, stopped: last})
			}
		}
	}
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

// goStates are the states of the goroutine running on thread at from, over from to to (xidGoStates).
// ok is false when the trace shows no goroutine running on thread by from.
//
// Handoff is read off the thread's starts of a P. A goroutine locked to a thread is run again only
// by that thread, after another thread hands it a P (runtime.startlockedm), which stops the P and
// wakes the thread; the thread starts the P once the kernel gives it a CPU. So the time from that
// P's stop to the thread's start of it, while the goroutine was runnable, is the thread waiting to
// be run after the handoff: inside the goroutine's runnable time, and on the kernel's run queue,
// where the thread's own clocks count it too.
func (r *xidTraceReading) goStates(thread int, from, to time.Time) (xidGoStates, bool) {
	start, end := r.traceTime(from), r.traceTime(to)
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
			for _, started := range r.starts[thread] {
				if started.at > segmentStart && started.at <= segmentEnd {
					states.Handoff += started.at.Sub(max(started.stopped, segmentStart))
				}
			}
		case exptrace.GoWaiting:
			states.Waiting += segmentEnd.Sub(segmentStart)
		case exptrace.GoSyscall:
			states.Syscall += segmentEnd.Sub(segmentStart)
		}
	}
	return states, true
}
