//go:build unix

package cputest

import (
	"fmt"
	"syscall"
	"time"
)

// Process is this process's total CPU time, user and system, since it started
// (getrusage(RUSAGE_SELF)). It counts every goroutine, so a test that times one call by it runs
// no other measured work beside that call: no test in the packages that use it calls t.Parallel.
func Process() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_SELF): %v", err))
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
