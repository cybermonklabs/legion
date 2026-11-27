package testwait

import (
	"testing"
	"time"
)

// A wait bounded only by the test binary's deadline costs the PACKAGE, not the test that waits.
// Its fact never comes, so it holds until the margin before the deadline, fails there by name as
// the margin intends, and Go's alarm then panics the binary with every test still running - the
// ones behind it in the parallel queue above all - so each of those loses its verdict without
// having done anything wrong. Measured: daemon-go-race leg 3, job 140311446907 at f1db1daf, one
// wait of 1,729.51 s, 47 top-level tests and 9 subtests lost, FAIL .../internal/daemon 1800.156s.
// One verdict earned, 56 lost.
//
// The margin alone cannot prevent it, because the margin is a span (30 s) while the budget is the
// -timeout: at CI's 30 minutes a hung wait held 29 min 30 s, 98.3% of the budget, and at the
// 15-minute legs 14 min 30 s, 96.7%.
//
// So a deadline-bound wait takes a SHARE of what is left rather than all of it: Eventually and
// Until (poll, through shared), Remaining, and Bounded's clamp all give up at what is left of the
// budget less the margin, divided by Share. The cost is asymmetric, which is why a fraction needs no
// perfect value: it only has to beat "all of it". A wait that legitimately needs the whole of what
// is left says so by name, WholeRemaining, and is counted; WithinPolling, Holds and ReceiveWithin
// take the caller's own bound with no clamp to the deadline and are untouched.
//
// Bounded(t, 1<<60, spare) is not that escape hatch. Before this rule 48 lines of this module wrote
// it - workspace_cache_test.go and gh_test.go in cmd/legion among them, claudebridge's deadlineWait
// and launcher's cleanupBeforeDeadline - and it names no ceiling of its own: it is Remaining written
// out by hand, the 98.3% fault in another spelling, so under this rule it gets the share like any
// other wait that names none.

// Share is how many waits' worth of the remaining budget one deadline-bound wait may take: a wait
// takes a quarter of what is left. Four is chosen from what the budget must cover rather than from
// taste: a test binary runs tests in parallel, so after one wait has hung and given up, what is
// left must still cover the tests queued behind it. A quarter of CI's 30-minute race budget is
// 7 min 30 s, which is 47 times the 9.57 s of the slowest honest wait measured among the callers
// (TestARoleMessageThePauseHeldForARoleItStoppedIsDeliveredOnceAfterTheResume, whose own declared
// ceiling is 6 minutes), and it leaves 22 min 30 s for everything behind it. A smaller share buys
// more room for the rest and risks an honest wait; a larger one approaches today's "all of it".
const Share = 4

// shareOf is the span a wait beginning with left of the budget may take: left/Share.
func shareOf(left time.Duration) time.Duration {
	return left / Share
}

// shared is the moment a deadline-bound wait in t gives up: now plus a share of what is left of
// the deadline after the margin, and false when the binary has no deadline (-timeout 0). It is
// never later than giveUpAt's moment, so a wait still leaves its test the margin to fail in and
// run its cleanups.
func shared(t testing.TB) (time.Time, bool) {
	end, bounded := giveUpAt(t)
	if !bounded {
		return time.Time{}, false
	}
	now := waitClock.Now()
	left := end.Sub(now)
	if left <= 0 {
		return end, true
	}
	return now.Add(shareOf(left)), true
}
