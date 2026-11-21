// Package cputest reads this process's own CPU time, the measure every growth test in this module
// times a call by. A call's wall time counts every moment the scheduler leaves the process off a
// core, so a loaded machine stretches it on either side of a comparison without the call doing more
// work; CPU time leaves that out. It is not immune to load: a busy machine still slows the
// process's own CPU time through shared caches and clock frequency, so a comparison still samples
// each side more than once. pmdoc's linear-time tests, the api's save ladders and the docs
// package's chained-clients row all time calls this way, and pmdoc's tests and the docs row time
// them with the garbage collector stopped (TimedWithoutCollections), so both live here once; it is
// an ordinary package rather than a _test.go file because Go cannot share test-only code across
// packages (see internal/stacktest's identical reasoning).
package cputest

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
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

// TimedWithoutCollections is the CPU time this process spends running work (Process after less
// Process before), with the garbage collector settled before work starts and stopped while it
// runs, so the time is work's own.
//
// A collection inside the window adds CPU time that depends on when it lands rather than on work.
// Whether one lands depends on the heap goal the untimed setup and the tests before it leave
// behind, and how long it runs depends on everything the process holds, not on work: its mark
// workers run on every processor work leaves idle, and Process counts them. Stopping the collector
// hides nothing a growth test asks about: work that grows faster than its input does so on its own
// goroutines, inside the window, and allocation that grows faster still pays inside the window
// for the fresh memory it touches.
//
// limit is the most memory, in bytes, the runtime may hold while the collector is stopped, unless
// the process already runs under a lower memory limit, which is kept. Work that allocates without
// bound is then collected rather than killing the process, and its window comes back with an
// error naming its collections, since their time is in its reading. The collector's settings are
// restored when work returns.
//
// It reports through no *testing.T, so a caller may run it on a goroutine that outlives its test
// (testing panics the whole binary on a Fail from such a goroutine) and report the error on the
// test's own.
func TimedWithoutCollections(limit int64, work func()) (time.Duration, error) {
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	limit = min(debug.SetMemoryLimit(-1), limit)
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(limit))
	cycles := gcCycles()
	before := Process()
	work()
	took := Process() - before
	if collected := gcCycles() - cycles; collected != 0 {
		return took, fmt.Errorf("%d collections ran inside a timed window, which grew the memory the runtime holds to the %d MiB limit; their CPU time is in its %s", collected, limit>>20, took.Round(time.Millisecond))
	}
	return took, nil
}

// gcCycles is how many collections this process has completed.
func gcCycles() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}
