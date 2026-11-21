//go:build linux

package cputest

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// threadCPU is the CPU time, user and system, that the operating system thread which runs the
// caller has spent since it started (clock_gettime(CLOCK_THREAD_CPUTIME_ID)). The clock moves in
// steps of a few hundred nanoseconds, where getrusage(RUSAGE_THREAD) splits the same time into user
// and system time by whole ticks of the scheduler (ThreadUser).
func threadCPU() time.Duration {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &now); err != nil {
		panic(fmt.Sprintf("clock_gettime(CLOCK_THREAD_CPUTIME_ID): %v", err))
	}
	return time.Duration(now.Nano())
}

// ThreadUser is the CPU time that the operating system thread which runs the caller has spent in
// user mode since it started (getrusage(RUSAGE_THREAD), ru_utime): the work of the program's own
// code on that thread. The time that the kernel spends for the thread is not in it: its system
// calls, its page faults, and the reclaim of memory before a page is given when the machine or
// the cgroup is short of memory. The kernel splits the thread's run time, which it keeps to the
// nanosecond, into user and system time by the ticks of the scheduler that land in each, so a
// difference of two readings is off by up to about a tick: 1 ms on a kernel of 1,000 ticks a
// second, 4 ms on one of 250.
func ThreadUser() time.Duration {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_THREAD, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_THREAD): %v", err))
	}
	return time.Duration(usage.Utime.Nano())
}

// threadID is the id of the operating system thread that runs the caller (gettid).
func threadID() int {
	return unix.Gettid()
}
