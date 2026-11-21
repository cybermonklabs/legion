//go:build !race

package docs

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A window whose own goroutines take every P is not judged as passing: Measure's idle Ps are idle
// only while the operation leaves them so, and a wait on the runtime may be the code's own work as
// well as the machine's. Here, after the window's first statement, xidOwnPsPerP goroutines for
// each P the operation has, and one more, spin, and the transaction's goroutine gives up its turn
// to them until xidOwnPsSpin has passed: it waits runnable on the runtime while they hold every P.
// The rig cannot tell whose wait that is, so the window is UNKNOWN, a rig problem, never a pass.
// Taking that wait out as the runtime's, the rig before this row read such a gap - a reviewer's
// forcing of 145 workers at 72 Ps, 50.125 ms on the clock and 1.826 ms of the thread's CPU - as
// 0.159 ms made, and the window kept D6 (2026-11-21).
//
// The spinners run on threads of their own kept off the CPU the window's thread is pinned to. On
// that CPU they made the window's thread queue for it outside the runtime's wait, where the rig
// cannot place a queue and reads it as the code's (storetest.XidGap.Made): 2 of 20 runs broke D6
// so, the thread queued up to 12.0 ms; kept off it, 0 of 20, at most 1.6 ms (a box load of
// about 970, 2026-11-21).
func TestXidWindowCannotJudgeAWaitWhileTheOperationsOwnGoroutinesHoldEveryP(t *testing.T) {
	storetest.RequireXidGapJudgment(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(xidLoadProcs))
	_, database, tracer := xidService(t)
	var workers, ps int
	report := tracer.Measure(t, database.Pool, func() {
		ps = runtime.GOMAXPROCS(0)
		workers = xidOwnPsPerP*ps + 1
		release := make(chan struct{})
		var spinning sync.WaitGroup
		var stop atomic.Bool
		var failed atomic.Value
		var away unix.CPUSet
		for range workers {
			spinning.Add(1)
			go func() {
				defer spinning.Done()
				<-release
				// Never unlocked: the goroutine ends locked, and the runtime ends its thread with it.
				runtime.LockOSThread()
				if err := unix.SchedSetaffinity(0, &away); err != nil {
					failed.Store(err)
				}
				for !stop.Load() {
				}
			}()
		}
		defer spinning.Wait()
		defer stop.Store(true)
		runRigTransactionDoing(t, database, func() {
			var mine unix.CPUSet
			if err := unix.SchedGetaffinity(0, &mine); err != nil {
				failed.Store(err)
			}
			for cpu := range len(mine) * 64 {
				if !mine.IsSet(cpu) {
					away.Set(cpu)
				}
			}
			close(release)
			for until := time.Now().Add(xidOwnPsSpin); time.Now().Before(until); {
				runtime.Gosched()
			}
			stop.Store(true)
		})
		if err, _ := failed.Load().(error); err != nil {
			t.Fatalf("keep the spinners off the window's CPU: %v", err)
		}
	})
	if len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	gap := window.Gaps[0]
	// The log reads no field the rig before this row lacks, so this row runs against that rig too,
	// and fails there.
	t.Logf("%d goroutines spun at %d Ps; the gap took %s on the clock, its goroutine %s runnable on the runtime\n%s",
		workers, ps, storetest.XidMillis(gap.Clock), storetest.XidMillis(gap.GoRunnable), window.Detail())
	if gap.GoRunnable < xidOwnPsSpin/2 {
		t.Fatalf("the transaction's goroutine waited %s runnable on the runtime, not half of the %s its own goroutines spun: they did not hold the Ps, so this run proves nothing\n%s",
			storetest.XidMillis(gap.GoRunnable), xidOwnPsSpin, window.Detail())
	}
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN: backend") || !strings.Contains(err.Error(), "keeps D6 only if its goroutine's waits on the runtime") {
		t.Fatalf("a gap whose goroutine waited while the operation's own goroutines held every P: verdict %v, want the window UNKNOWN", err)
	}
}

// xidOwnPsSpin is how long the transaction's goroutine gives up its turn in the gap, and
// xidOwnPsPerP how many of the operation's own goroutines spin for each P meanwhile. Each yield
// puts the goroutine at the back of the runtime's global queue, behind those that wait for a P,
// and costs its thread CPU of its own - a P handed off and taken back - which the rig reads as the
// code's. At 2 per P, the goroutine came back after a fraction of a millisecond and yielded 79 to
// 2,434 times in 50 ms, and the thread ran 0.7 to 6.7 ms of CPU (10 runs); in another 10 runs, one
// broke D6 on that alone, the gap 4.151 ms made against statements of 3.179 ms. At 8 per P, it
// waited 73 to 254 ms behind the queue, so it yielded once, and the thread ran 0.22 to 0.45 ms
// (10 runs, 72 Ps, 2026-11-21), under the window's first statement alone, 1.75 ms or more in those
// runs.
const (
	xidOwnPsSpin = 50 * time.Millisecond
	xidOwnPsPerP = 8
)
