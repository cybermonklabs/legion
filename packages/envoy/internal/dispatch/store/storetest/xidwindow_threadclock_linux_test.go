//go:build linux

package storetest

import (
	"context"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A Linux build does not promise a schedstat file: gVisor exposes neither /proc/self/schedstat
// nor /proc/thread-self/schedstat. Nor does it promise that a thread can be held out of NUMA
// balancing: a container's seccomp profile may refuse set_mempolicy. Where the probe cannot read
// the clocks, or cannot hold the thread on a machine NUMA balancing runs on, the rig must follow
// its race build and judge window shape alone, rather than fail every Measure before it starts.
//
// The readable row proves a build whose probe reads the clocks and holds the thread still judges
// a planted gap. It fakes only the probe and judges a window built here, so it holds on a kernel
// whose real reader reads nothing too: Measure records each statement through the real
// readXidThreadClock, and a Measure run where that reader fails would judge the run's own unread
// gaps, not the probe.
//
// The unreadable and unholdable rows drive one real transaction through Measure and must not
// fail: the old compile-time `xidThreadClocks = true` makes the unreadable row call Fatal before
// it begins, so it is the control for this runtime probe.
func TestXidGapJudgmentNeedsReadableThreadClocks(t *testing.T) {
	if raceDetector {
		t.Skip("the race build judges window shape alone")
	}
	held := func() (func(), bool) { return func() {}, true }
	for _, test := range []struct {
		name    string
		read    func() xidThreadClock
		hold    func() (func(), bool)
		judge   bool
		measure bool
	}{
		{
			name:  "readable",
			read:  func() xidThreadClock { return xidThreadClock{thread: xidThread()} },
			hold:  held,
			judge: true,
		},
		{
			name:    "unreadable",
			read:    func() xidThreadClock { return xidThreadClock{} },
			hold:    held,
			measure: true,
		},
		{
			name:    "unholdable",
			read:    func() xidThreadClock { return xidThreadClock{thread: xidThread()} },
			hold:    func() (func(), bool) { return func() {}, false },
			measure: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := xidThreadClockAvailability
			xidThreadClockAvailability = &xidThreadClockProbe{read: test.read, hold: test.hold}
			t.Cleanup(func() { xidThreadClockAvailability = previous })

			if got := XidJudgesGaps(); got != test.judge {
				t.Fatalf("XidJudgesGaps() = %t, want %t when the thread-clock probe is %s", got, test.judge, test.name)
			}

			if test.measure {
				database := Open(t)
				tracer := NewXidTracer()
				traced := &store.Store{Pool: Traced(t, database.Pool, tracer, nil)}
				report := tracer.Measure(t, traced.Pool, func() {
					ctx := MarkXid(context.Background())
					tx, err := traced.Pool.Begin(ctx)
					if err != nil {
						t.Fatalf("begin the transaction: %v", err)
					}
					defer tx.Rollback(ctx)
					if _, err := tx.Exec(ctx, "select pg_current_xact_id()"); err != nil {
						t.Fatalf("take a transaction id: %v", err)
					}
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("commit the transaction: %v", err)
					}
				})
				AssertXidWindows(t, report, XidWindowOf(2, "the forced xid and its COMMIT"))
			}

			window := XidWindow{
				Statements:    []XidStatement{{SQL: "select pg_current_xact_id()"}},
				MaxGap:        2 * XidMaxGap,
				GapTime:       2 * XidMaxGap,
				StatementTime: time.Nanosecond,
			}
			if got := window.Violations(); !test.judge && len(got) != 0 {
				t.Fatalf("a thread-clock probe that is %s made the rig report gap violations %v, want shape alone", test.name, got)
			}
			if test.judge && len(window.Violations()) == 0 {
				t.Fatal("a readable thread clock judged no planted gap")
			}
		})
	}
}

// The rig reads a thread's clocks at both ends of every gap, and while the collector is held off
// an allocation there would be a first touch of a page the process never used: on a loaded
// machine, milliseconds of the thread's CPU in the kernel that the gap reads as the code's
// (runWithoutCollector). So a read allocates nothing. The reader before this rule, which read the
// file through os.ReadFile and split it with bytes.Fields, allocates on every read and fails here.
func TestReadingAThreadsClocksAllocatesNothing(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if readXidThreadClock().thread == 0 {
		t.Skip("this kernel shows no thread's scheduler clocks, so the rig reads none")
	}
	if allocs := testing.AllocsPerRun(100, func() { readXidThreadClock() }); allocs != 0 {
		t.Fatalf("a read of the thread's clocks allocates %v times, want none", allocs)
	}
}

// On a machine NUMA balancing runs on, a held thread allocates on its own node, as by default,
// but has no page moved for it while it is held, and gets back the policy it had when it is let
// go (xidHoldThread). On a machine NUMA balancing leaves alone the hold changes nothing.
func TestHoldingAThreadKeepsNUMABalancingOffItUntilItIsLetGo(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	before := threadMempolicy(t)
	release, held := xidHoldThread()
	if !held {
		t.Fatal("the rig could not hold this thread out of NUMA balancing")
	}
	during := threadMempolicy(t)
	release()
	after := threadMempolicy(t)
	want := before
	if xidNUMABalancing() {
		want = xidMempolicyLocal
	}
	if during != want {
		t.Errorf("a held thread's memory policy is mode %d, want %d", during, want)
	}
	if after != before {
		t.Errorf("a thread let go has memory policy mode %d, want the %d it had", after, before)
	}
}

// threadMempolicy is the calling thread's memory policy mode.
func threadMempolicy(t *testing.T) int {
	t.Helper()
	var mode int
	var nodes [16]uint64
	if _, _, errno := unix.Syscall6(unix.SYS_GET_MEMPOLICY, uintptr(unsafe.Pointer(&mode)),
		uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)*64), 0, 0, 0); errno != 0 {
		t.Fatalf("read this thread's memory policy: %v", errno)
	}
	return mode
}

// The window runs after a collection, with the collector already off (runWithoutCollector): with
// it off the heap only grows, so without that collection every allocation of the window would be
// the first touch of a page. The test reads the collector's cycle count inside the operation.
func TestTheWindowRunsAfterACollection(t *testing.T) {
	cycles := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(cycles)
	before := cycles[0].Value.Uint64()
	var during uint64
	var percent int
	runWithoutCollector(func() {
		metrics.Read(cycles)
		during = cycles[0].Value.Uint64()
		percent = debug.SetGCPercent(-1)
	})
	if during <= before {
		t.Errorf("the operation ran after %d collections, the same as before it: no collection ran first", during)
	}
	if percent != -1 {
		t.Errorf("the operation ran with GOGC %d, want the collector off", percent)
	}
}
