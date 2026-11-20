//go:build linux

package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime/trace"
	"slices"
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
// running - waiting for the runtime to run it again - and the time it spent waiting on something
// it asked for: a sleep, a lock, a channel, the network (XidGap.GoRunnable, XidGap.GoWaiting).

// xidTrace is an execution trace Measure is taking, and the moment it started on both the
// process's clock and the system's monotonic clock, which the trace's own clock is tied to.
type xidTrace struct {
	buf      bytes.Buffer
	baseTime time.Time
	baseMono int64
}

// startXidTrace starts the execution tracer. It stops the world to start, so Measure starts it
// before the operation and none of that pause is inside a window.
func startXidTrace() (*xidTrace, error) {
	x := &xidTrace{}
	if err := trace.Start(&x.buf); err != nil {
		return nil, err
	}
	var mono unix.Timespec
	x.baseTime = time.Now()
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		trace.Stop()
		return nil, err
	}
	x.baseMono = mono.Nano()
	return x, nil
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
// each thread's goroutines in the order they started running on it, and the tie between the
// trace's clock and the process's.
type xidTraceReading struct {
	changes  map[exptrace.GoID][]xidGoStateChange
	runs     map[int][]xidThreadRun
	baseTime time.Time
	baseMono int64
	snapshot exptrace.ClockSnapshot
}

// stop stops the tracer. It stops the world to stop, so Measure stops it once the operation has
// returned, after every window.
func (x *xidTrace) stop() {
	trace.Stop()
}

// read reads the stopped trace back.
func (x *xidTrace) read() (*xidTraceReading, error) {
	reader, err := exptrace.NewReader(&x.buf)
	if err != nil {
		return nil, fmt.Errorf("read the execution trace: %w", err)
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
			return nil, fmt.Errorf("read the execution trace: %w", err)
		}
		switch event.Kind() {
		case exptrace.EventSync:
			if snapshot := event.Sync().ClockSnapshot; snapshot != nil && !snapped {
				reading.snapshot = *snapshot
				snapped = true
			}
		case exptrace.EventStateTransition:
			transition := event.StateTransition()
			if transition.Resource.Kind != exptrace.ResourceGoroutine {
				continue
			}
			g := transition.Resource.Goroutine()
			_, to := transition.Goroutine()
			reading.changes[g] = append(reading.changes[g], xidGoStateChange{at: event.Time(), state: to})
			if to == exptrace.GoRunning && event.Thread() != exptrace.NoThread {
				thread := int(event.Thread())
				reading.runs[thread] = append(reading.runs[thread], xidThreadRun{at: event.Time(), g: g})
			}
		}
	}
	if !snapped {
		return nil, errors.New("the execution trace holds no clock snapshot to tie its clock to the process's")
	}
	return reading, nil
}

// traceTime is moment on the trace's clock.
func (r *xidTraceReading) traceTime(moment time.Time) exptrace.Time {
	mono := r.baseMono + int64(moment.Sub(r.baseTime))
	return r.snapshot.Trace + exptrace.Time(mono-int64(r.snapshot.Mono))
}

// goStates are the time the goroutine running on thread at from spent, until to, runnable but not
// running and waiting on something it asked for. ok is false when the trace shows no goroutine
// running on thread by from.
func (r *xidTraceReading) goStates(thread int, from, to time.Time) (runnable, waiting time.Duration, ok bool) {
	start, end := r.traceTime(from), r.traceTime(to)
	runs := r.runs[thread]
	last := slices.IndexFunc(runs, func(run xidThreadRun) bool { return run.at > start }) - 1
	if last == -2 {
		last = len(runs) - 1
	}
	if last < 0 {
		return 0, 0, false
	}
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
			runnable += segmentEnd.Sub(segmentStart)
		case exptrace.GoWaiting:
			waiting += segmentEnd.Sub(segmentStart)
		}
	}
	return runnable, waiting, true
}
