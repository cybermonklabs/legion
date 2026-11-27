//go:build !race

package docs

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A loaded machine can hold every P the process has: a goroutine's thread that waits its turn on a
// busy CPU still holds its P. A window's goroutine whose system call returns then finds no P, and
// its locked thread sleeps until one is handed to it, a sleep the thread's clocks read as the
// code's; Measure runs its operation with idle Ps beside it, so the return takes one at once. Here
// the process runs at xidLoadProcs Ps, and one goroutine more than that spins through the measured
// operation, each locked to a thread pinned to the CPU that crowdACPU's shells crowd, so their
// threads hold the Ps while they wait for that CPU; the window's gap gives up its turn there, in a
// system call each time. The window keeps D6. With Measure's added Ps planted out, 5 of 5 runs
// broke D6, the gap 26.8 to 137.7 ms made with its goroutine 45.7 to 152.0 ms runnable on the
// runtime (a 288-CPU machine at a load of about 600, 2026-11-21).
func TestXidWindowKeepsD6WhenTheLoadHoldsThePs(t *testing.T) {
	// Not parallel: it measures with the xid window rig (XidTracer.Measure), which sets the whole
	// process's GOMAXPROCS and collector for its operation and fails a test that called t.Parallel
	// (requireMeasuredAlone) It also sets GOMAXPROCS to xidLoadProcs itself.
	storetest.RequireXidGapJudgment(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(xidLoadProcs))
	_, database, tracer := xidService(t)
	crowded := crowdACPU(t)
	held := xidLoadProcs + 1
	var procs int
	var stop atomic.Bool
	var started atomic.Int32
	var spinners sync.WaitGroup
	failed := make(chan error, held)
	report := tracer.Measure(t, database.Pool, func() {
		procs = runtime.GOMAXPROCS(0)
		defer spinners.Wait()
		defer stop.Store(true)
		for range held {
			spinners.Add(1)
			go func() {
				defer spinners.Done()
				// Never unlocked: the goroutine ends locked, and the runtime ends its thread with it.
				runtime.LockOSThread()
				if err := crowded.pinThread(); err != nil {
					failed <- err
					return
				}
				started.Add(1)
				for !stop.Load() {
				}
			}()
		}
		for int(started.Load()) < held && len(failed) == 0 {
			runtime.Gosched()
		}
		defer crowded.hold(t)()
		runRigTransactionDoing(t, database, func() { crowded.run(t) })
	})
	crowded.stop()
	close(failed)
	for err := range failed {
		t.Fatal(err)
	}
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	if !strings.Contains(window.Statements[0].SQL, "insert into projects") {
		t.Fatalf("the window starts at %q, want the insert that gave the transaction its xid:\n%s", window.Statements[0].SQL, window.Detail())
	}
	gap := window.Gaps[0]
	t.Logf("%d Ps in the operation, %d goroutines of the load holding them; the gap made %s in %s on the clock, its goroutine %s runnable on the runtime",
		procs, held, storetest.XidMillis(gap.Made()), storetest.XidMillis(gap.Clock), storetest.XidMillis(gap.GoRunnable))
	if gap.Clock <= storetest.XidMaxGap || gap.Runnable <= 0 {
		t.Fatalf("the crowded gap took %s on the clock, %s of it waiting for a CPU: the crowding opened no gap past %s, so this run proves nothing\n%s",
			storetest.XidMillis(gap.Clock), storetest.XidMillis(gap.Runnable), storetest.XidMillis(storetest.XidMaxGap), window.Detail())
	}
	if err := report.Err(); err != nil {
		t.Fatalf("a gap whose system calls returned while the load held the process's Ps broke D6:\n%v", err)
	}
}

// xidLoadProcs is the process's Ps in TestXidWindowKeepsD6WhenTheLoadHoldsThePs, whatever the
// machine: as many as the forced runs use (forced.sh sets GOMAXPROCS=8).
const xidLoadProcs = 8
