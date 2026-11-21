//go:build !race

package docs

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A window whose own goroutines take every P is not judged as passing: Measure's idle Ps are idle
// only while the operation leaves them so, and a wait on the runtime may be the code's own work as
// well as the machine's. Here, after the window's first statement, twice as many
// goroutines as the operation has Ps, and one more, spin for xidOwnPsSpin, and the transaction's
// goroutine gives up its turn to them meanwhile: it waits runnable on the runtime while they hold
// every P. The rig cannot tell whose wait that is, so the window is UNKNOWN, a rig problem, never a
// pass. Taking that wait out as the runtime's, the rig before this row read such a gap - a
// reviewer's forcing of 145 workers at 72 Ps, 50.125 ms on the clock and 1.826 ms of the thread's
// CPU - as 0.159 ms made, and the window kept D6 (2026-11-21). The yields cost the thread CPU of
// its own, 0.9 to 5.8 ms in 10 runs on a loaded machine: where that alone breaks D6, with the
// runtime's wait taken out, the window is a violation, which the rig names before the wait, and
// that is not a pass either.
func TestXidWindowCannotJudgeAWaitWhileTheOperationsOwnGoroutinesHoldEveryP(t *testing.T) {
	storetest.RequireXidGapJudgment(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(xidLoadProcs))
	_, database, tracer := xidService(t)
	var workers, ps int
	report := tracer.Measure(t, database.Pool, func() {
		ps = runtime.GOMAXPROCS(0)
		workers = 2*ps + 1
		release := make(chan struct{})
		var spinning sync.WaitGroup
		var stop atomic.Bool
		for range workers {
			spinning.Add(1)
			go func() {
				defer spinning.Done()
				<-release
				for !stop.Load() {
				}
			}()
		}
		defer spinning.Wait()
		defer stop.Store(true)
		runRigTransactionDoing(t, database, func() {
			close(release)
			for until := time.Now().Add(xidOwnPsSpin); time.Now().Before(until); {
				runtime.Gosched()
			}
			stop.Store(true)
		})
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
	if broken := window.Violations(); len(broken) > 0 {
		if err == nil || !strings.Contains(err.Error(), "XID WINDOW VIOLATION") {
			t.Fatalf("a window that breaks D6 with the runtime's wait taken out (%s): verdict %v, want that violation", strings.Join(broken, "; "), err)
		}
		t.Logf("the yields' own CPU broke D6 with the runtime's wait taken out: %s", strings.Join(broken, "; "))
		return
	}
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN: backend") || !strings.Contains(err.Error(), "keeps D6 only if its goroutine's waits on the runtime") {
		t.Fatalf("a gap whose goroutine waited while the operation's own goroutines held every P: verdict %v, want the window UNKNOWN", err)
	}
}

// xidOwnPsSpin is how long the operation's own goroutines spin in the gap.
const xidOwnPsSpin = 50 * time.Millisecond
