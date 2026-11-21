//go:build !linux

package cputest

import (
	"fmt"
	"syscall"
	"time"
)

// ThreadUser is the process's user CPU time (getrusage(RUSAGE_SELF), ru_utime) on a system whose
// getrusage counts no thread on its own: there a window that Growth times counts each goroutine of
// the process in user mode.
func ThreadUser() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_SELF): %v", err))
	}
	return time.Duration(usage.Utime.Nano())
}

// threadID is 0 on a system where ThreadUser reads the whole process, whose readings hold no
// thread of their own.
func threadID() int {
	return 0
}
