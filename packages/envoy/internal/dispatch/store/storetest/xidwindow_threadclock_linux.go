//go:build linux

package storetest

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// xidThreadClocksAvailable reports whether the calling process can read an OS thread's scheduler
// clocks. Linux does not promise schedstat: gVisor, for example, exposes no
// /proc/thread-self/schedstat. The first caller probes once while locked to an OS thread, as a
// marked transaction's goroutine is; every later caller reads that same fact, so a kernel that
// cannot show the clocks follows the race build and judges window shape alone.
//
// xidThreadClockAvailability is a pointer so its reader and its once can be replaced together by
// the test that proves an unreadable clock judges no gap. Tests in this package do not run in
// parallel.
var xidThreadClockAvailability = &xidThreadClockProbe{read: readXidThreadClock}

type xidThreadClockProbe struct {
	once      sync.Once
	read      func() xidThreadClock
	available bool
}

func xidThreadClocksAvailable() bool {
	probe := xidThreadClockAvailability
	probe.once.Do(func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		probe.available = probe.read().thread != 0
	})
	return probe.available
}

// xidThread is the calling OS thread's id.
func xidThread() int { return unix.Gettid() }

// readXidThreadClock reads the calling OS thread's scheduler clocks: the CPU time it has run
// (CLOCK_THREAD_CPUTIME_ID, which the kernel brings up to date as it is read), and the time it has
// waited runnable on a run queue for a CPU (run_delay, the second field of
// /proc/thread-self/schedstat). The kernel adds a wait to run_delay when the thread gets a CPU
// back, so a reading the thread takes while it runs holds every wait it has had. It answers the
// zero clock when either cannot be read, or when the goroutine moved to another thread between
// the two reads.
func readXidThreadClock() xidThreadClock {
	thread := unix.Gettid()
	var cpu unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &cpu); err != nil {
		return xidThreadClock{}
	}
	raw, err := os.ReadFile("/proc/thread-self/schedstat")
	if err != nil || unix.Gettid() != thread {
		return xidThreadClock{}
	}
	fields := bytes.Fields(raw)
	if len(fields) < 2 {
		return xidThreadClock{}
	}
	waited, err := strconv.ParseInt(string(fields[1]), 10, 64)
	if err != nil {
		return xidThreadClock{}
	}
	return xidThreadClock{thread: thread, ran: time.Duration(cpu.Nano()), waited: time.Duration(waited)}
}
