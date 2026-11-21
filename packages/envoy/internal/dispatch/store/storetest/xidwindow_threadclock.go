package storetest

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
)

// XidRequireGapsEnv names the environment variable that makes a run that cannot judge a window's
// statement-free gaps fail instead of judging the window's shape alone (Measure). The plain CI jobs
// that judge gaps, envoy-go and envoy-go-docs-finalizers, set it, so a runner whose kernel stops
// showing a thread's clocks turns those jobs red with the reason rather than green on shape alone.
const XidRequireGapsEnv = "DISPATCH_XID_REQUIRE_GAP_JUDGMENT"

// xidThreadClockAvailability is what this process can give the rig to judge gaps with: whether
// it can read an OS thread's scheduler clocks (read), and whether it can hold a thread, and the
// memory every thread touches, out of NUMA balancing (hold, xidHoldMemory). Linux does not promise
// either: gVisor, for example, exposes no /proc/thread-self/schedstat, and a container's seccomp
// profile may refuse set_mempolicy and mbind. The first caller probes once while locked to an OS
// thread, as a marked transaction's goroutine is; every later caller reads that same fact, so a
// kernel that cannot give the rig either follows the race build and judges window shape alone.
//
// It is a pointer so its readers and its once can be replaced together by the test that proves an
// unreadable clock, or a thread NUMA balancing cannot be kept off, judges no gap. Tests in this
// package do not run in parallel.
var xidThreadClockAvailability = &xidThreadClockProbe{read: readXidThreadClock, hold: xidHoldThread, memory: xidHoldMemory}

type xidThreadClockProbe struct {
	once   sync.Once
	read   func() xidThreadClock
	hold   func() (release func(), held bool)
	memory func() bool
	// unavailable is why the probe found gaps cannot be judged, empty when they can.
	unavailable string
}

// xidThreadClocksUnavailable is why this process cannot judge a window's gaps, empty when it can.
func xidThreadClocksUnavailable() string {
	probe := xidThreadClockAvailability
	probe.once.Do(func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		switch {
		case probe.read().thread == 0:
			probe.unavailable = "this thread's scheduler clocks cannot be read (CLOCK_THREAD_CPUTIME_ID and the run_delay of /proc/thread-self/schedstat; this build reads them on Linux only)"
		case !xidHoldOnce(probe.hold):
			probe.unavailable = "this thread cannot be held out of NUMA balancing (set_mempolicy MPOL_LOCAL was refused)"
		case !probe.memory():
			probe.unavailable = "the process's memory cannot be held out of NUMA balancing (mbind MPOL_LOCAL was refused)"
		}
	})
	return probe.unavailable
}

// xidHoldOnce holds the calling thread with hold and puts it back, answering whether it could.
func xidHoldOnce(hold func() (release func(), held bool)) bool {
	release, held := hold()
	release()
	return held
}

// XidNoGapJudgment is why this build judges no window's statement-free gaps, empty when it judges
// them (XidJudgesGaps): the race detector, or what the thread-clock probe could not do.
func XidNoGapJudgment() string {
	if raceDetector {
		return "the race detector is on, and its scheduling stretches a gap past any bound, so the race build judges window shape alone"
	}
	return xidThreadClocksUnavailable()
}

// xidGapsRequired reports whether this run must judge gaps (XidRequireGapsEnv).
func xidGapsRequired() bool { return os.Getenv(XidRequireGapsEnv) != "" }

// xidUnjudgedGapsProblem is the rig problem of a run that sets XidRequireGapsEnv and cannot judge
// gaps, naming why (XidNoGapJudgment); empty when the run judges gaps or does not require them.
func xidUnjudgedGapsProblem() string {
	reason := XidNoGapJudgment()
	if reason == "" || !xidGapsRequired() {
		return ""
	}
	return fmt.Sprintf("XID RIG PROBLEM (not a finding about the code): %s is set, so this run must judge statement-free gaps, and it cannot: %s", XidRequireGapsEnv, reason)
}

// RequireXidGapJudgment stops a test that holds nothing unless the run judges statement-free
// gaps - a rig self-test whose fault is a gap, or a test of what the rig reads to judge one - in a
// run that judges none (XidNoGapJudgment): it fails, naming why, where the run sets
// XidRequireGapsEnv, and skips, naming why, everywhere else. Every such test calls it, and Measure
// fails as it does under XidRequireGapsEnv, so a run that cannot judge gaps reads the same way in
// every test.
func RequireXidGapJudgment(t testing.TB) {
	t.Helper()
	if problem := xidUnjudgedGapsProblem(); problem != "" {
		t.Fatal(problem)
	}
	if reason := XidNoGapJudgment(); reason != "" {
		t.Skipf("this run judges window shape alone, not statement-free gaps: %s", reason)
	}
}
