//go:build linux

package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
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
// running - waiting for the runtime to run it again - the time it spent waiting on something it
// asked for (a sleep, a lock, a channel, the network) and the time it spent in a system call; and
// how much of its runnable time its thread spent woken but waiting on the kernel's run queue,
// which the thread's own clocks count as well (xidGoStates.Handoff).

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
	header := bytes.Clone(x.buf.Bytes()[:min(x.buf.Len(), 16)])
	cannot := func(err error) error {
		return fmt.Errorf("read the execution trace, whose header is %q, written by %s, with golang.org/x/exp %s: %w; a toolchain whose trace format x/exp/trace does not read needs golang.org/x/exp moved to a version that reads it",
			bytes.TrimRight(header, "\x00"), runtime.Version(), xidExpVersion(), err)
	}
	reader, err := exptrace.NewReader(&x.buf)
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
			if snapshot := event.Sync().ClockSnapshot; snapshot != nil && !snapped {
				reading.snapshot = *snapshot
				snapped = true
			}
		case exptrace.EventStateTransition:
			transition := event.StateTransition()
			switch transition.Resource.Kind {
			case exptrace.ResourceGoroutine:
				g := transition.Resource.Goroutine()
				_, to := transition.Goroutine()
				reading.changes[g] = append(reading.changes[g], xidGoStateChange{at: event.Time(), state: to})
				if to == exptrace.GoRunning && event.Thread() != exptrace.NoThread {
					thread := int(event.Thread())
					reading.runs[thread] = append(reading.runs[thread], xidThreadRun{at: event.Time(), g: g})
				}
			case exptrace.ResourceProc:
				p := transition.Resource.Proc()
				switch _, to := transition.Proc(); to {
				case exptrace.ProcIdle:
					stopped[p] = event.Time()
				case exptrace.ProcRunning:
					if at, ok := stopped[p]; ok && event.Thread() != exptrace.NoThread {
						thread := int(event.Thread())
						reading.starts[thread] = append(reading.starts[thread], xidProcStart{at: event.Time(), stopped: at})
					}
				}
			}
		}
	}
	if !snapped {
		return nil, errors.New("the execution trace holds no clock snapshot to tie its clock to the process's")
	}
	return reading, nil
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
