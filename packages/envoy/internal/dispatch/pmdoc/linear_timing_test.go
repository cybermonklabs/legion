package pmdoc

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// processCPUTime is this process's total CPU time, user and system, since it started
// (cputest.Process), not wall time: it leaves out the time the process waits for a core, though
// another process's load still slows its own CPU time through shared caches and clock frequency.
// Every test that measures a call through it is serial (it does not call t.Parallel), and go test
// starts a run's parallel tests only once every serial test of that run has ended, so ordinarily
// at most one measured call runs at a time and nothing else runs beside it. leakedCallGuard
// (below) is what keeps that true even once a call has missed its own deadline and gone on running
// in the background. Every measured call reads it through timedWindow.
func processCPUTime() time.Duration { return cputest.Process() }

// cpuTimeOf is the CPU time one run of an operation takes, for a test that holds one run to a
// budget of CPU time rather than to a growth (heavyBodyCPUBudget). prepare builds one run's input,
// untimed, and returns the run, as the small and large of cputest.Growth do. cputest.Measure picks
// how many runs one timed window holds, so the clock resolves the window, and divides by them;
// every run's input is built before its window opens (timedWindow), since the input is not the
// code under test.
func cpuTimeOf(prepare func() (run func())) (time.Duration, error) {
	return cputest.Measure(func(count int) (time.Duration, error) {
		runs := make([]func(), count)
		for i := range runs {
			runs[i] = prepare()
		}
		return timedWindow(func() {
			for _, run := range runs {
				run()
			}
		})
	})
}

// timedWindow is the CPU time this process spends running op, with the garbage collector settled
// before op starts and stopped while it runs, so the time is op's own work alone.
//
// A collection inside the window adds CPU time that depends on when it lands rather than on op.
// Whether one lands depends on the heap goal the untimed setup and the tests before it leave
// behind, so a small size's write can run with no collection at all while a full-size write runs
// one or two. A collection's own CPU time varies several times over between cycles, since its
// idle mark workers run on every processor the single-threaded write leaves idle and
// processCPUTime counts them. Either way the full size's time grows against the small size's
// without the work growing.
//
// Stopping the collector hides nothing a regression would show: work that grows quadratically
// does so on op's own goroutine, and allocation that grows quadratically still pays inside the
// window for the fresh memory it touches. The heap may grow to windowMemoryLimit while the
// collector is stopped, so a regression that allocates without bound is collected rather than
// killing the process; a window that reaches the limit comes back with an error naming its
// collections, since that collection would put its noise back into the time.
//
// timedWindow reports through no *testing.T. It runs inside the goroutine runWithDeadline starts,
// which a call that misses its deadline leaves running past its test's end, and testing panics the
// whole binary on a Fail from such a goroutine; the caller waiting on runWithDeadline reports the
// error on its test's own goroutine (timedWithDeadline). A call that misses its deadline may also
// never return to restore the collector, so runWithDeadline restores it as well.
func timedWindow(op func()) (time.Duration, error) {
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	limit := min(debug.SetMemoryLimit(-1), windowMemoryLimit)
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(limit))
	cycles := gcCycles()
	before := processCPUTime()
	op()
	took := processCPUTime() - before
	if collected := gcCycles() - cycles; collected != 0 {
		return took, fmt.Errorf("%d collections ran inside a timed window, which grew the heap to the %d MiB memory limit; their CPU time is in its %s", collected, limit>>20, took.Round(time.Millisecond))
	}
	return took, nil
}

// windowMemoryLimit is the most memory timedWindow lets a window grow the heap to with the
// collector stopped, unless the process already runs under a lower limit. A window times the parse
// of one body that a test holds to a budget (heavyBodyCPUBudget), or of many runs of a smaller one
// that hold about 50 of the clock's steps of work (cputest.Measure), around a second's even at
// gVisor's 10 ms step.
const windowMemoryLimit = 3 << 30

// gcCycles is how many collections this process has completed.
func gcCycles() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// collectorSettings is GOGC and the memory limit as they stand, read without changing either.
func collectorSettings() (percent int, limit int64) {
	sample := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(sample)
	return int(int64(sample[0].Value.Uint64())), debug.SetMemoryLimit(-1)
}

// leaking counts how many of each what (timedWithDeadline's label) are currently
// running past their own deadline - a count, not a single flag, so two concurrently leaking calls
// sharing one label cannot be mistaken for one clearing when either returns. Go cannot cancel a
// goroutine mid-computation, so this is the only way a later call can tell that measuring right
// now would share processCPUTime's process-wide counter with CPU a still-running earlier call
// keeps spending. runWithDeadline adds to it the instant a deadline misses and removes from it
// only once that call's own goroutine finally returns; leakedCallGuard reads it.
var leaking struct {
	mu   sync.Mutex
	what map[string]int
}

// leakedCallGuard reports what, if anything, is still running past its own deadline - the message
// timedWithDeadline fails a new call with before ever starting it, naming the call still leaking
// CPU into this process's RUSAGE_SELF rather than inventing a ratio out of a measurement that
// shared it. heavy_bodies_test.go's workOf refuses to count a call's bytes the same way, since the
// heap's allocations are the whole process's too. A pure function (no *testing.T) so a unit test
// can assert it directly: see TestLeakedCallBlocksTheNextMeasurement.
func leakedCallGuard(what string) string {
	leaking.mu.Lock()
	defer leaking.mu.Unlock()
	if len(leaking.what) == 0 {
		return ""
	}
	labels := make([]string, 0, len(leaking.what))
	for label, count := range leaking.what {
		if count > 1 {
			label = fmt.Sprintf("%s (x%d)", label, count)
		}
		labels = append(labels, label)
	}
	sort.Strings(labels) // a fixed order, not map iteration's, so the message is reproducible
	return fmt.Sprintf("%s: refusing to measure while %s is still running past its own deadline (this process's CPU time and the heap's allocations are the whole process's, so measuring now would attribute that call's ongoing work to this one)", what, strings.Join(labels, ", "))
}

// runWithDeadline runs op, labelled what, in its own goroutine and reports whether it returned
// within deadline. Go cannot cancel a goroutine mid-computation, so an op that does not return in
// time is left running - registered under what in leaking until it eventually returns and
// deregisters itself, so leakedCallGuard can refuse a later call that would otherwise share its
// ongoing CPU - rather than stopped; the point of the deadline is only to stop waiting for it and
// report that promptly, not to free its resources. A deregistration goroutine is spawned only on
// a deadline miss, and only then: on a call that truly never returns, it stays parked on result's
// receive, at no CPU cost, for as long as the test binary runs. cleared, non-nil only on a
// deadline miss, closes the instant that goroutine deregisters op's own entry - the signal
// waitForLeakToClear waits on, rather than polling. err is op's own, a timed window's verdict
// (timedWindow), handed back only with an op that returned in time: a late op's is dropped with it,
// since no test waits for it any more. It hands back the collector's settings as it found them on
// either return, since an op stopped inside timedWindow that misses its deadline would
// otherwise leave the collector stopped for every later test in the binary.
func runWithDeadline(what string, deadline time.Duration, op func() (time.Duration, error)) (took time.Duration, onTime bool, cleared <-chan struct{}, err error) {
	percent, limit := collectorSettings()
	defer debug.SetMemoryLimit(limit)
	defer debug.SetGCPercent(percent)
	type outcome struct {
		took time.Duration
		err  error
	}
	result := make(chan outcome, 1)
	go func() {
		took, err := op()
		result <- outcome{took, err}
	}()
	select {
	case got := <-result:
		return got.took, true, nil, got.err
	case <-time.After(deadline):
		leaking.mu.Lock()
		if leaking.what == nil {
			leaking.what = make(map[string]int)
		}
		leaking.what[what]++
		leaking.mu.Unlock()
		done := make(chan struct{})
		go func() {
			<-result
			leaking.mu.Lock()
			leaking.what[what]--
			if leaking.what[what] == 0 {
				delete(leaking.what, what)
			}
			leaking.mu.Unlock()
			close(done)
		}()
		return 0, false, done, nil
	}
}

// timedWithDeadline refuses to run op, naming whatever is still leaking (leakedCallGuard), then
// runs it under runWithDeadline and fails t, naming what and deadline, if it did not return in
// time, and reports op's error, naming what, if it did. Both are reported here, on t's own
// goroutine, never from inside op, which can outlive t.
func timedWithDeadline(t *testing.T, what string, deadline time.Duration, op func() (time.Duration, error)) time.Duration {
	t.Helper()
	if msg := leakedCallGuard(what); msg != "" {
		t.Fatal(msg)
	}
	took, onTime, _, err := runWithDeadline(what, deadline, op)
	if !onTime {
		t.Fatalf("%s did not return within %s", what, deadline)
	}
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
	return took
}

// TestRunWithDeadlineReportsAHangPromptly proves runWithDeadline's own guarantee directly: a
// synthetic op that blocks far longer than its deadline is reported late (onTime=false) promptly -
// close to the deadline itself, not close to how long op actually blocks - the property
// timedWithDeadline relies on to fail fast and name its call instead of waiting out go test's own
// default timeout with none attached.
func TestRunWithDeadlineReportsAHangPromptly(t *testing.T) {
	const deadline = 20 * time.Millisecond
	release := make(chan struct{})
	started := time.Now()
	_, onTime, cleared, _ := runWithDeadline("synthetic prompt hang", deadline, func() (time.Duration, error) {
		<-release // blocked until this test has its verdict, however the scheduler runs it
		return 0, nil
	})
	if elapsed := time.Since(started); elapsed > 10*deadline {
		t.Errorf("runWithDeadline took %s to report a hang against a %s deadline, want close to immediate", elapsed, deadline)
	}
	if onTime {
		t.Error("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestLeakedCallBlocksTheNextMeasurement proves leakedCallGuard directly: once a synthetic op has
// missed its own deadline and is still running, a second, otherwise-healthy call through
// timedWithDeadline refuses to start - naming the first call - rather than silently sharing its
// still-accruing CPU (processCPUTime is this whole process's, not per-goroutine). The op is held
// until the guard has answered, so it is still running when asked, however late the test runs.
func TestLeakedCallBlocksTheNextMeasurement(t *testing.T) {
	const deadline = 20 * time.Millisecond
	release := make(chan struct{})
	_, onTime, cleared, _ := runWithDeadline("synthetic leaked hang", deadline, func() (time.Duration, error) {
		<-release
		return 0, nil
	})
	if onTime {
		close(release)
		t.Fatal("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	if msg := leakedCallGuard("a second call"); msg == "" {
		t.Error("leakedCallGuard let a second call measure while the first was still registered as leaked, want it refused")
	} else {
		t.Logf("leakedCallGuard correctly refused: %s", msg)
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestAMissedDeadlineLeavesTheCollectorAsItWas proves runWithDeadline hands back the collector as
// it found it when a call misses its deadline inside timedWindow, where the collector is
// stopped: the call may never return to restore it, and a collector left stopped would hold every
// later test in this binary to the window's memory limit with no collection below it.
func TestAMissedDeadlineLeavesTheCollectorAsItWas(t *testing.T) {
	percent, limit := collectorSettings()
	stopped, release := make(chan struct{}), make(chan struct{})
	runtime.GC() // so the window's own collection takes no part of the deadline
	_, onTime, cleared, _ := runWithDeadline("synthetic hang inside a timed window", time.Second, func() (time.Duration, error) {
		return timedWindow(func() {
			close(stopped)
			<-release
		})
	})
	if onTime {
		close(release)
		t.Fatal("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	select {
	case <-stopped:
	default:
		close(release)
		waitForLeakToClear(t, cleared)
		t.Fatal("the op had not stopped the collector by its deadline, so this test proves nothing")
	}
	gotPercent, gotLimit := collectorSettings()
	if gotPercent != percent || gotLimit != limit {
		t.Errorf("after a missed deadline the collector reads GOGC %d and a %d-byte memory limit, want %d and %d as before the call", gotPercent, gotLimit, percent, limit)
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestALateCallFinishingAfterItsTestDoesNotPanic proves a timed call that misses its deadline and
// returns only after its test has ended, with a collection in its window, reports nothing through
// that test: testing panics the whole binary on a Fail from a goroutine whose test has completed,
// which would turn one missed deadline into a crash of every test in pmdoc. The call runs in a
// child process so that crash, if it happens, is this test's failure rather than the binary's; the
// child's own verdict is the missed deadline, reported on its test's goroutine.
func TestALateCallFinishingAfterItsTestDoesNotPanic(t *testing.T) {
	t.Parallel()
	child := exec.Command(os.Args[0], "-test.run", "^TestALateCallFinishingAfterItsTestDoesNotPanicHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), lateCallHelperEnv+"=1")
	output, _ := child.CombinedOutput() // the helper fails by design: its call misses its deadline
	if strings.Contains(string(output), "panic:") {
		t.Fatalf("a call that finished after its test had ended crashed the test binary:\n%s", output)
	}
	if !strings.Contains(string(output), lateCallWhat+" did not return within") || !strings.Contains(string(output), lateCallFinished) {
		t.Fatalf("the child process did not report the missed deadline and then outlive the late call:\n%s", output)
	}
}

const (
	// lateCallHelperEnv names the child process TestALateCallFinishingAfterItsTestDoesNotPanic
	// starts.
	lateCallHelperEnv = "PMDOC_LATE_CALL_HELPER"
	// lateCallWhat labels the child's call.
	lateCallWhat = "synthetic call finishing after its test"
	// lateCallFinished is what the child logs once the late call has returned.
	lateCallFinished = "the late call returned after its test had ended"
)

// TestALateCallFinishingAfterItsTestDoesNotPanicHelper is that child process: a subtest whose call
// misses its deadline through timedWithDeadline, as heavyBodyCPUBudget's measurement does, then the
// call's release once the subtest has ended, a collection inside its window, and a wait until it
// has returned.
func TestALateCallFinishingAfterItsTestDoesNotPanicHelper(t *testing.T) {
	if os.Getenv(lateCallHelperEnv) == "" {
		t.Skip("the child process of TestALateCallFinishingAfterItsTestDoesNotPanic")
	}
	stopped, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	runtime.GC() // so the window's own collection takes no part of the deadline
	t.Run("a call that misses its deadline", func(t *testing.T) {
		timedWithDeadline(t, lateCallWhat, time.Second, func() (time.Duration, error) {
			defer close(finished)
			return timedWindow(func() {
				close(stopped)
				<-release
				runtime.GC() // a collection inside the window, once its test has ended
			})
		})
	})
	select {
	case <-stopped:
	default:
		t.Fatal("the op had not opened its window by its deadline, so this test proves nothing")
	}
	close(release)
	select {
	case <-finished:
		t.Log(lateCallFinished)
	case <-time.After(10 * time.Second):
		t.Fatal("the late call never returned")
	}
}

// waitForLeakToClear blocks until cleared closes or fails t: the deregistration goroutine
// runWithDeadline spawned on a deadline miss closes cleared the instant it has removed its own
// entry from leaking, so a later test in this same binary never starts measuring while this
// test's own synthetic leak is still registered. 10s (packages/envoy/internal/cistore/burst_test.go's
// waitAll uses the same bound for the same kind of wait) is far more than the handoff needs; past
// it, something is wrong.
func waitForLeakToClear(t *testing.T, cleared <-chan struct{}) {
	t.Helper()
	select {
	case <-cleared:
	case <-time.After(10 * time.Second):
		t.Fatal("a synthetic leaked call never deregistered")
	}
}

// perCallReportMargin is what perCallDeadline leaves of the test binary's own deadline, so a
// measured call that hangs fails by name (timedWithDeadline) while go test's own timeout is still
// some way off.
const perCallReportMargin = 60 * time.Second

// perCallHangCap bounds a measured call when the run has no deadline to read (-timeout 0): a
// hang still has to be reported, and half an hour is far past any healthy call at these sizes.
const perCallHangCap = 30 * time.Minute

// perCallDeadline bounds a call that timedWithDeadline measures: what the test binary's own
// deadline leaves after perCallReportMargin (testwait.Remaining), capped at perCallHangCap. It is
// read from the deadline rather than fixed because each measured call is a CPU-time measurement
// (cpuTimeOf, cputest.Measure) that repeats its operation until a window reaches the measurement's
// resolution, so the wall time it takes is the CPU it needs multiplied by whatever contention the
// runner is under; a fixed wall-clock bound over a CPU-time budget fails healthy calls on a
// loaded shared runner, which is what this file's own history records. For scale: a healthy
// '<a b="x;a@b.cc ">' call at the 1 MiB tag shape, measured under -race in a two-CPU gVisor pod on
// 2026-11-03, took 50.26s - 84% of the 60s bound this replaced, which is why the bound had to go
// rather than be raised. A run whose deadline leaves no more than the margin fails once, by name,
// saying whether the -timeout was too short or the tests before used it up.
func perCallDeadline(t *testing.T) time.Duration {
	t.Helper()
	return min(perCallHangCap, testwait.Remaining(t, perCallReportMargin))
}

// sizeText is size, a count of bytes, in KiB or MiB.
func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}
