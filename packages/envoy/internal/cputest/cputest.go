// Package cputest reads this process's own CPU time, the measure every growth test in this module
// times a call by, and the work of a call's own code on the thread that runs it (Growth). A call's
// wall time counts every moment the scheduler leaves the process off a core, so a loaded machine
// stretches it on either side of a comparison without the call doing more work; CPU time leaves
// that out. It is not immune to load: a busy machine still slows the process's own CPU time
// through shared caches and clock frequency, so a comparison still samples each side more than
// once. pmdoc's linear-time tests and the api's save ladders both time calls this way, and the form
// of The Spec times its judgment on the thread, so it lives here once; it is an ordinary package
// rather than a _test.go file because Go cannot share test-only code across packages (see
// internal/stacktest's identical reasoning).
package cputest

import (
	"fmt"
	"runtime"
	"sync"
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

// onThread is how far clock, a clock of the thread that runs the caller, moves while call runs,
// and the thread at the reading before call and at the reading after it (threadID): it holds the
// calling goroutine on its thread (runtime.LockOSThread) from the one reading to the other, so both
// readings are of that thread.
func onThread(clock func() time.Duration, call func()) (took time.Duration, from, to int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	from = threadID()
	before := clock()
	call()
	took = clock() - before
	return took, from, threadID()
}

// MinWindow is the least time that each window of Growth lasts. The kernel splits a thread's time
// into user and system time by the ticks of the scheduler, so a window's time is off by up to about
// a tick, 1 ms to 4 ms, and a window of MinWindow or more holds that as a small part of it.
const MinWindow = 50 * time.Millisecond

// Growth is how many times the work of large grows over the work of small, where the input of
// large is scale times the input of small. Small runs scale times in one timed window and large
// once in another, so that for work in proportion to the input the two windows last about as long,
// and a load of the machine that comes and goes reaches both alike: a window that is much shorter
// than the other is more likely to fall between two bursts. Each window is timed samples times, the
// two in turn, each after a collection of the heap (runtime.GC), so that neither pays for the
// garbage of the other. A busy machine only adds to the time of a window, so the least of each is
// its work, and the answer holds the least of each window. The growth is scale times the work of
// the large window over the work of the small one: about scale for work in proportion to the
// input, and about scale times scale for work for each pair of units.
//
// small and large each build the input of one run, which is not the code under test, and return
// the run: Growth builds each run of a window before the window opens. A run that changes its input,
// as a write into a document does, gets a fresh input from each call, and Again is the small or
// large of a run that changes nothing.
//
// The work of a window is the work of the caller's own code: the user CPU time of the thread that
// runs the window (ThreadUser), on which onThread holds it. It leaves out the time that the kernel
// spends for that thread: its system calls, its page faults, and the reclaim of memory before each
// page that a machine or a cgroup short of memory gives, which grows with the memory that other
// processes hold and not with the caller's work. It leaves out a wait for a core, the collector's
// work on another thread, the work that the call hands to another goroutine, and the work of
// another process that the call waits for, such as a database server: a test of such a call bounds
// that work in another way, such as by the count of the statements that it sends to the server.
// The work that the call does in user mode on its own thread is in it, the share of the
// collector's work that an allocation does there among it.
//
// Growth refuses a reading that measures nothing, with an error that says why: a sample whose two
// readings are of two threads, and a window whose least is no time or shorter than MinWindow
// (ErrShortWindow), of which the split of the thread's time by the ticks of the scheduler is no
// small part.
func Growth(samples, scale int, small, large func() (run func())) (growth float64, smallWindow, largeWindow time.Duration, err error) {
	inputs := []func() func(){small, large}
	runs := []int{scale, 1}
	names := []string{"smaller", "larger"}
	least := make([]time.Duration, len(inputs))
	for sample := range samples {
		for index, input := range inputs {
			window := make([]func(), runs[index])
			for run := range window {
				window[run] = input()
			}
			runtime.GC()
			took, from, to := onThread(ThreadUser, func() {
				for _, run := range window {
					run()
				}
			})
			if from != to {
				return 0, 0, 0, fmt.Errorf("cputest: the %s window started on thread %d and ended on thread %d, so its two readings are of two threads",
					names[index], from, to)
			}
			if sample == 0 || took < least[index] {
				least[index] = took
			}
		}
	}
	for index, took := range least {
		switch {
		case took <= 0:
			return 0, least[0], least[1], fmt.Errorf("cputest: the %s window reads %v, no time, so it measures no work", names[index], took)
		case took < MinWindow:
			return 0, least[0], least[1], fmt.Errorf("cputest: the %s window lasts %v, %w", names[index], took, ErrShortWindow)
		}
	}
	return float64(scale) * float64(least[1]) / float64(least[0]), least[0], least[1], nil
}

// ErrShortWindow is Growth's refusal of a window shorter than MinWindow.
var ErrShortWindow = fmt.Errorf("under the %v that a window lasts so that the split of a thread's time by the ticks of the scheduler is a small part of it: give each window more work", MinWindow)

// Again is the small or large of Growth for run, a run that changes nothing that it reads: each
// run of a window is run itself.
func Again(run func()) func() func() {
	return func() func() { return run }
}

// resolution is the smallest step Process moves in on this machine, measured once: the fewest
// nanoseconds between two readings that differ, over resolutionSamples spins. A Linux kernel moves
// it in steps far under a millisecond; gVisor moves it in 10 ms steps, so there a call shorter than
// 10 ms reads as 0 or 10 ms.
var resolution = sync.OnceValue(func() time.Duration {
	var step time.Duration
	for range resolutionSamples {
		start := Process()
		next := Process()
		for next == start {
			next = Process()
		}
		if step == 0 || next-start < step {
			step = next - start
		}
	}
	return step
})

// resolutionSamples is how many changes of Process resolution waits for. A sample that also
// counts another goroutine's CPU time reads a larger step, never a smaller one, so the smallest
// of a few is the clock's own step.
const resolutionSamples = 5

// windowSteps is how many steps of resolution a timed window must last before Measure divides it
// by its runs. A reading is off by up to one step at each end, so this keeps that error near 4% of
// the reading, where a single call shorter than a step reads as 0 or as one whole step.
const windowSteps = 50

// Measure returns the CPU time one run of an operation takes. batch runs the operation runs times
// in one timed window and returns the window's CPU time (Process after less Process before); it
// builds every run's input before the window opens, a fresh one for each run of an operation
// that changes its input, and returns an error that makes the window's time meaningless. Measure
// calls batch with more runs until a window lasts windowSteps steps of resolution, then divides
// that window's time by its runs. A window shorter than that is discarded, never averaged in.
func Measure(batch func(runs int) (time.Duration, error)) (time.Duration, error) {
	want := windowSteps * resolution()
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
