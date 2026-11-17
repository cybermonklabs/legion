//go:build linux

package cputest

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// Thread is the CPU time, user and system, that the operating system thread which runs the caller
// has spent since it started (clock_gettime(CLOCK_THREAD_CPUTIME_ID)). It counts no other
// goroutine, so a call that OfCall times on it is timed alone while a test's server runs beside
// it. The clock moves in steps of a few hundred nanoseconds, where getrusage(RUSAGE_THREAD) moves
// in whole ticks of the scheduler, a millisecond on a kernel of 1,000 ticks a second.
func Thread() time.Duration {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &now); err != nil {
		panic(fmt.Sprintf("clock_gettime(CLOCK_THREAD_CPUTIME_ID): %v", err))
	}
	return time.Duration(now.Nano())
}
