// Package cputest reads CPU time, the measure every growth test in this module times a call by. A
// call's wall time counts every moment the scheduler leaves the process off a core, so a loaded
// machine stretches it on either side of a comparison without the call doing more work; CPU time
// leaves that out. It is not immune to load: a busy machine still slows the process's own CPU time
// through shared caches and clock frequency, so a comparison still samples each side more than
// once. pmdoc's linear-time tests and the api's save ladders both time calls this way, so it lives
// here once; it is an ordinary package rather than a _test.go file because Go cannot share
// test-only code across packages (see internal/stacktest's identical reasoning).
//
// It reads two clocks. Process is the whole process's, every goroutine's and the garbage
// collector's. OnThread is the clock of the one OS thread a call runs on, which counts that call's
// goroutine alone.
package cputest

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Process is this process's total CPU time, user and system, since it started
// (getrusage(RUSAGE_SELF)). It counts every goroutine, so a test that times one call by it runs
// no other measured work beside that call: such a test does not call t.Parallel, and go test
// starts a run's parallel tests only once every serial test of that run has ended, so none of them
// runs beside it.
func Process() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_SELF): %v", err))
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// OnThread runs op with the calling goroutine locked to its OS thread and returns the CPU time,
// user and system, that thread spent while op ran (clock_gettime(CLOCK_THREAD_CPUTIME_ID)). It
// counts op's own goroutine alone. Every other goroutine runs on other threads and is left out:
// the garbage collector's mark workers, which mark the whole process's heap whenever a collection
// falls due during op, and whatever other work the process runs. The collector work the runtime
// does on op's own goroutine stays in: the mark assists its allocation is charged, the sweeping
// it pays for as it allocates, the sweep that finishes the last cycle when its allocation starts a
// collection, and the end of a mark phase that one of its assists completes. So does every page
// fault its allocation takes. op must do its work on its own goroutine: work it hands to another
// goroutine is not counted.
func OnThread(op func()) time.Duration {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	start := thread()
	op()
	return thread() - start
}

// thread is the CPU time of the OS thread the calling goroutine runs on; a caller that reads it
// twice and subtracts holds the goroutine on that thread between the readings (OnThread).
func thread() time.Duration {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &now); err != nil {
		panic(fmt.Sprintf("clock_gettime(CLOCK_THREAD_CPUTIME_ID): %v", err))
	}
	return time.Duration(now.Nano())
}

// resolution and threadResolution are the smallest steps Process and the thread clock move in on
// this machine, each measured once (smallestStep). A Linux kernel moves them in steps far under a
// millisecond; gVisor moves both in whole ticks of 10 ms, so there a call shorter than 10 ms reads
// as 0 or 10 ms on either clock.
var (
	resolution       = sync.OnceValue(func() time.Duration { return smallestStep(Process) })
	threadResolution = sync.OnceValue(func() time.Duration {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		return smallestStep(thread)
	})
)

// smallestStep is the fewest nanoseconds between two readings of clock that differ, over
// resolutionSamples spins.
func smallestStep(clock func() time.Duration) time.Duration {
	var step time.Duration
	for range resolutionSamples {
		start := clock()
		next := clock()
		for next == start {
			next = clock()
		}
		if step == 0 || next-start < step {
			step = next - start
		}
	}
	return step
}

// resolutionSamples is how many changes of a clock smallestStep waits for. A sample that also
// counts another goroutine's CPU time reads a larger step, never a smaller one, so the smallest
// of a few is the clock's own step.
const resolutionSamples = 5

// windowSteps is how many steps of its clock's resolution a timed window must last before
// Measure or MeasureThread divides it by its runs. A reading is off by up to one step at each end,
// so this keeps that error near 4% of the reading, where a single call shorter than a step reads
// as 0 or as one whole step.
const windowSteps = 50

// Measure returns the CPU time one run of an operation takes. batch runs the operation runs times
// in one timed window and returns the window's CPU time (Process after less Process before); it
// builds every run's input before the window opens, a fresh one for each run of an operation
// that changes its input, and returns an error that makes the window's time meaningless. Measure
// calls batch with more runs until a window lasts windowSteps steps of resolution, then divides
// that window's time by its runs. A window shorter than that is discarded, never averaged in.
func Measure(batch func(runs int) (time.Duration, error)) (time.Duration, error) {
	return measure(resolution(), batch)
}

// MeasureThread is Measure on the thread clock (OnThread): prepare builds the input of runs runs
// of an operation, untimed, and returns window, which runs them all on the calling goroutine.
// MeasureThread times window on its thread and calls prepare with more runs until a window lasts
// windowSteps steps of the thread clock's resolution, then divides that window's time by its runs.
func MeasureThread(prepare func(runs int) (window func())) time.Duration {
	took, _ := measure(threadResolution(), func(runs int) (time.Duration, error) { // OnThread has no error to report
		return OnThread(prepare(runs)), nil
	})
	return took
}

// measure is Measure and MeasureThread, on a clock whose resolution is resolution.
func measure(resolution time.Duration, batch func(runs int) (time.Duration, error)) (time.Duration, error) {
	want := windowSteps * resolution
	for runs := 1; ; {
		took, err := batch(runs)
		if err != nil {
			return took / time.Duration(runs), err
		}
		if took >= want {
			return took / time.Duration(runs), nil
		}
		if took <= 0 {
			runs *= 10
			continue
		}
		// Aim a quarter past the window, so a run that takes a little longer than this one's
		// average still lands the next window past it.
		runs = max(runs*2, int(int64(runs)*int64(want)*5/(int64(took)*4))+1)
	}
}
