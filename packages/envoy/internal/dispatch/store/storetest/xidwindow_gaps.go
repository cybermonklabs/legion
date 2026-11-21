package storetest

import "time"

// XidGap is a stretch of a window - the time between two statements, or a statement's own - from
// one end to the other on this process's clock (Clock), read as well off the scheduler clocks of
// the thread that sent it (XidTracer.lockTransaction) - the CPU time it ran meanwhile (Ran) and the
// time it waited runnable on a run queue for a CPU (Runnable) - and off the execution trace of the
// goroutine that sent it: the time the Go runtime kept it runnable but not running (GoRunnable),
// the time it waited on something it asked for (GoWaiting), and the time it spent in a system call
// (GoSyscall). Read is false when the rig could not read the thread's clocks at both ends on the
// thread its transaction's goroutine was locked to, and Traced is false when the trace does not
// show that goroutine. Softirqs are how many times each softirq ran on SoftirqCPU, the CPU the
// thread is pinned to, over the gap, and Counted is false where the rig could not count them
// (xidwindow_softirq.go).
type XidGap struct {
	Clock      time.Duration
	Ran        time.Duration
	Runnable   time.Duration
	GoRunnable time.Duration
	GoWaiting  time.Duration
	GoSyscall  time.Duration
	Read       bool
	Traced     bool
	Softirqs   XidSoftirqs
	SoftirqCPU int
	Counted    bool
}

// xidGoStates is the time a goroutine spent over a stretch of the trace in each state the rig
// reads (xidTraceReading.goStates).
type xidGoStates struct {
	Runnable, Waiting, Syscall time.Duration
}

// Asleep is the time the gap's thread was off a CPU: neither running nor waiting for one.
func (g XidGap) Asleep() time.Duration { return max(g.Clock-g.Ran-g.Runnable, 0) }

// Made is the time the code made in the gap, read so that no time the code made is ever left out.
// The machine's part of a gap is the time its thread waited for a CPU outside the Go runtime's own
// waits - the kernel's run queue, CFS bandwidth throttling included, which the thread's run_delay
// counts - and the time the runtime kept its goroutine from running (GoRunnable: a preemption, a
// stop of the world, a system call that lost its P). The code's part is the rest: the time the
// thread ran or slept while its goroutine ran or was in a system call, and the time the goroutine
// waited on what it asked for - a lock, a channel, the network - whatever its thread did meanwhile.
//
// The two clocks are read only at the gap's ends, so they do not say where in the goroutine's
// states the thread's wait for a CPU and its sleep fell. Each of two readings holds whatever they
// did: the gap less the runtime's wait (Clock - GoRunnable), which counts any wait for a CPU outside
// it as the code's; and the thread's CPU and sleep with the goroutine's own waits (Ran + Asleep +
// GoWaiting), which counts any sleep inside the runtime's wait as the code's. Made is the lesser,
// so no sleep or wait the code made is ever taken out: where the thread slept through the runtime's
// wait and also waited for a CPU outside it, Made reads the lesser of those two as the code's too.
// A thread stopped by a debugger, a stop signal or a freezer is asleep to its clocks, and reads as
// the code's. Made never reads more than the gap's own time on the clock. A gap whose clocks were
// not read (Read) is its time on the clock.
func (g XidGap) Made() time.Duration {
	if !g.Read {
		return g.Clock
	}
	return min(g.Clock, max(min(g.Clock-g.GoRunnable, g.Ran+g.Asleep()+g.GoWaiting), 0))
}

// xidStretch reads the stretch from to to off thread's clocks, from's reading and to's, where both
// were read on it, and off the execution trace of the goroutine running on that thread, where the
// rig took one (reading).
func xidStretch(from, to time.Time, fromClock, toClock xidThreadClock, thread int, reading *xidTraceReading) XidGap {
	span := XidGap{Clock: max(to.Sub(from), 0)}
	if thread != 0 && fromClock.thread == thread && toClock.thread == thread {
		span.Read = true
		span.Ran = toClock.ran - fromClock.ran
		span.Runnable = toClock.waited - fromClock.waited
		if reading != nil {
			var states xidGoStates
			states, span.Traced = reading.goStates(thread, from, to)
			span.GoRunnable, span.GoWaiting, span.GoSyscall = states.Runnable, states.Waiting, states.Syscall
		}
	}
	return span
}

// xidGapBetween is the gap from before's end to after's start, read where both were sent from the
// thread their transaction's goroutine was locked to (xidStretch), with the softirqs its CPU ran
// meanwhile (countGap).
func xidGapBetween(before, after XidStatement, reading *xidTraceReading) XidGap {
	thread := before.locked
	if after.locked != thread {
		thread = 0
	}
	gap := xidStretch(before.End, after.Start, before.ended, after.started, thread, reading)
	countGap(&gap, before, after)
	return gap
}

// span is the statement's own stretch, from its Start to its End, read as a gap is (xidStretch).
func (s XidStatement) span(reading *xidTraceReading) XidGap {
	return xidStretch(s.Start, s.End, s.started, s.ended, s.locked, reading)
}

// Ran is the CPU time the statement's thread ran from its Start to its End, zero where the rig did
// not read the thread's clocks at both (xidStretch).
func (s XidStatement) Ran() time.Duration { return s.span(nil).Ran }
