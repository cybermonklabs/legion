package storetest

import "time"

// XidGap is a stretch of a window - the time between two statements, or a statement's own - from
// one end to the other on this process's clock (Clock), read as well off the scheduler clocks of
// the thread that sent it (XidTracer.lockTransaction) - the CPU time it ran meanwhile (Ran) and the
// time it waited runnable on a run queue for a CPU (Runnable) - and off the execution trace of the
// goroutine that sent it: the time the Go runtime kept it runnable but not running (GoRunnable),
// the time it waited on something it asked for (GoWaiting), the time it spent in a system call
// (GoSyscall), and how much of its runnable time its thread spent woken but on the run queue after
// another thread handed it a P (Overlap, counted in both Runnable and GoRunnable). Read is false
// when the rig could not read the thread's clocks at both ends on the thread its transaction's
// goroutine was locked to, and Traced is false when the trace does not show that goroutine.
// Softirqs are how many times each softirq ran on SoftirqCPU, the CPU the thread is pinned to,
// over the gap, and Counted is false where the rig could not count them (xidwindow_softirq.go).
type XidGap struct {
	Clock      time.Duration
	Ran        time.Duration
	Runnable   time.Duration
	GoRunnable time.Duration
	GoWaiting  time.Duration
	GoSyscall  time.Duration
	Overlap    time.Duration
	Read       bool
	Traced     bool
	Softirqs   XidSoftirqs
	SoftirqCPU int
	Counted    bool
}

// xidGoStates is the time a goroutine spent over a stretch of the trace in each state the rig
// reads, and the part of its runnable time its locked thread spent on the run queue after a
// handoff (xidTraceReading.goStates).
type xidGoStates struct {
	Runnable, Waiting, Syscall, Handoff time.Duration
}

// Made is the time the code made in the gap. The machine's part of a gap is the time its thread
// waited runnable for a CPU (Runnable) and the time the Go runtime kept its goroutine from running
// (GoRunnable: a preemption, a stop of the world, a syscall that lost its P), counted once where
// they overlap: a goroutine handed back to its locked thread stays runnable until that thread,
// woken, gets a CPU, and that wait is in both (Overlap). The rest is the code's: the time the
// thread ran, and the time it was off a CPU of its own accord - asleep, in a system call, or
// waiting on a lock, a channel or the network. Made never reads less than the CPU the thread ran
// and the waits the goroutine asked for (GoWaiting), nor less than its waits and its system calls
// beyond the run queue's part of them - the code's whatever the machine does - and never more than
// the gap's own time on the clock. A gap whose clocks were not read (Read) is its time on the
// clock.
func (g XidGap) Made() time.Duration {
	if !g.Read {
		return g.Clock
	}
	queued := g.Runnable - g.Overlap
	return min(g.Clock, max(g.Clock-g.GoRunnable-queued, g.Ran+g.GoWaiting, g.GoWaiting+g.GoSyscall-queued))
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
			span.Overlap = min(states.Handoff, span.Runnable, span.GoRunnable)
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
