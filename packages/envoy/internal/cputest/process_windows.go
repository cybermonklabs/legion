//go:build windows

package cputest

import (
	"fmt"
	"syscall"
	"time"
)

// Process is this process's total CPU time, user and system, since it started
// (GetProcessTimes on the current process, Windows' getrusage(RUSAGE_SELF)). It counts every
// goroutine, so a test that times one call by it runs no other measured work beside that call: no
// test in the packages that use it calls t.Parallel.
func Process() time.Duration {
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		panic(fmt.Sprintf("GetCurrentProcess: %v", err))
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(self, &creation, &exit, &kernel, &user); err != nil {
		panic(fmt.Sprintf("GetProcessTimes: %v", err))
	}
	return filetimeSpan(kernel) + filetimeSpan(user)
}

// filetimeSpan is a FILETIME that holds a span, not a moment, as a Duration: a count of 100 ns
// intervals. Filetime.Nanoseconds would take 1601's epoch off it, which a span has none of.
func filetimeSpan(span syscall.Filetime) time.Duration {
	return time.Duration(int64(span.HighDateTime)<<32|int64(span.LowDateTime)) * 100
}
