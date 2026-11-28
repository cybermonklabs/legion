//go:build !race

package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// loopbackSendsFor sends loopback datagrams from this thread until it has run cpu of CPU time
// (storetest.XidLoopbackSends): the code's own network sends, whose receives the kernel runs on
// this thread's CPU, in its own system calls, and counts as that CPU's NET_RX.
func loopbackSendsFor(t *testing.T, cpu time.Duration) {
	t.Helper()
	from := threadCPU(t)
	storetest.XidLoopbackSends(t, func(int) bool { return threadCPU(t)-from < cpu })
}

// xidOwnSendsGap is the CPU time of the code's own network sends the capped gap runs: past the
// bound and all the time a gap's softirqs may hold (storetest.XidSoftirqGapHold), by 1.6 ms.
const xidOwnSendsGap = storetest.XidMaxGap + storetest.XidSoftirqGapHold + 1600*time.Microsecond

// xidSleptGapSends and xidSleptGap are the code's own network sends and the sleep after them in
// the slept gap: 6 ms, of which its thread runs 0.5 ms, so the most its softirqs may hold is
// 0.5 ms and the gap breaks a 5.5 ms bound; were the hold not held to the thread's CPU time, it
// would be 2.4 ms and the gap would pass under 7.4 ms.
const (
	xidSleptGapSends = storetest.XidMaxGap / 10
	xidSleptGap      = storetest.XidMaxGap + storetest.XidMaxGap/10
)

// A gap the code slept past the bound is the code's whatever its CPU ran meanwhile: a gap's
// softirqs may hold only CPU time its thread ran. Here the gap's CPU runs network receives for the
// sends before the sleep, and the gap still breaks rule 1. The rig judges it as on a kernel that
// charges interrupt time to the thread it lands on (storetest.XidChargeInterruptTime), so the
// hold is in play on every kernel. With the hold not held to the thread's CPU time, the gap keeps
// rule 1 under the 7.4 ms bound the gap's cap (storetest.XidSoftirqGapHold) sets, only rule 2
// breaks, and the test fails. With the softirq read planted to read nothing, the gap counts no
// receive and the run stops as one that proves nothing.
func TestXidWindowRigNeverExcusesASleepForSoftirqs(t *testing.T) {
	// Not parallel: it measures with the xid window rig (XidTracer.Measure), which sets the whole
	// process's GOMAXPROCS and collector for its operation and fails a test that called t.Parallel
	// (requireMeasuredAlone) It also makes the rig charge interrupt time to the thread for the whole
	// process (storetest.XidChargeInterruptTime).
	storetest.RequireXidGapJudgment(t)
	storetest.XidChargeInterruptTime(t)
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func() {
			loopbackSendsFor(t, xidSleptGapSends)
			time.Sleep(xidSleptGap)
		})
	})
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	gap := window.Gaps[0]
	if !gap.Counted || !strings.Contains(gap.Softirqs.String(), "NET_RX") || gap.SoftirqHold() == 0 {
		t.Fatalf("the gap's CPU ran %s (counted: %t), holding %s: no network receive was counted and held in it, so this run proves nothing\n%s",
			gap.Softirqs, gap.Counted, storetest.XidMillis(gap.SoftirqHold()), window.Detail())
	}
	if err := report.Err(); err == nil || !strings.Contains(err.Error(), "follows statement 0: insert into projects") {
		t.Fatalf("a %s sleep after loopback sends: verdict %v, want a D6 violation after the insert\n%s", xidSleptGap, err, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}

// However many device softirqs a gap's CPU runs, they hold no more than
// storetest.XidSoftirqGapHold of it. Here the gap is the code's own network sends, hundreds of
// them, each counted as a NET_RX on the gap's CPU, for 1.6 ms of CPU past the bound and that cap:
// the gap breaks rule 1, over the 7.4 ms bound the cap sets. The rig judges it as on a kernel that
// charges interrupt time to the thread it lands on (storetest.XidChargeInterruptTime), so the cap
// is in play on every kernel. The run proves something only where the gap counted enough device
// runs that their holds reach the cap, five at 0.5 ms each; one that counted fewer stops as one
// that proves nothing. With the gap's cap planted out, its counts hold all the CPU it ran, its
// bound passes 7.4 ms, and the test fails at its check of the bound. With the softirq read planted
// to read nothing, the gap counts no receive and the run stops as one that proves nothing.
func TestXidWindowRigHoldsAGapsSoftirqsToTheirCap(t *testing.T) {
	// Not parallel: it measures with the xid window rig (XidTracer.Measure), which sets the whole
	// process's GOMAXPROCS and collector for its operation and fails a test that called t.Parallel
	// (requireMeasuredAlone) It also makes the rig charge interrupt time to the thread for the whole
	// process (storetest.XidChargeInterruptTime).
	storetest.RequireXidGapJudgment(t)
	storetest.XidChargeInterruptTime(t)
	_, database, tracer := xidService(t)
	report := tracer.Measure(t, database.Pool, func() {
		runRigTransactionDoing(t, database, func() { loopbackSendsFor(t, xidOwnSendsGap) })
	})
	if len(report.Problems) > 0 || len(report.Windows) != 1 {
		t.Fatalf("the rig judged %d windows with problems %v, want the one transaction judged", len(report.Windows), report.Problems)
	}
	window := report.Windows[0]
	gap := window.Gaps[0]
	if runs := gap.Softirqs.Device(); !gap.Counted || time.Duration(runs)*storetest.XidSoftirqRunHold < storetest.XidSoftirqGapHold || gap.Ran < xidOwnSendsGap {
		t.Fatalf("the gap ran %s of CPU and its CPU %s, %d device runs (counted: %t): want %s of sends and device runs enough to hold %s, so this run proves nothing\n%s",
			storetest.XidMillis(gap.Ran), gap.Softirqs, runs, gap.Counted, storetest.XidMillis(xidOwnSendsGap), storetest.XidMillis(storetest.XidSoftirqGapHold), window.Detail())
	}
	if want := storetest.XidMaxGap + storetest.XidSoftirqGapHold; gap.Bound() != want {
		t.Fatalf("the gap's bound is %s, want %s: its device softirqs held more than the gap's cap, storetest.XidSoftirqGapHold\n%s",
			storetest.XidMillis(gap.Bound()), storetest.XidMillis(want), window.Detail())
	}
	if err := report.Err(); err == nil || !strings.Contains(err.Error(), "follows statement 0: insert into projects") ||
		!strings.Contains(err.Error(), "(over 7.400 ms") {
		t.Fatalf("%s of the code's own network sends in a gap: verdict %v, want a D6 violation after the insert, over the 7.400 ms bound\n%s",
			storetest.XidMillis(xidOwnSendsGap), err, window.Detail())
	}
	t.Logf("%s\n%s", window.Summary(), window.Detail())
}
