package workflow

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// lastWake is the newest check_wake row of issue: when it is due, and the wake it carries.
func lastWake(t *testing.T, rig checkRig, issue string) (time.Time, record.CheckWakeDue) {
	t.Helper()
	rows := checkWakeRows(t, rig.pool, issue)
	if len(rows) == 0 {
		t.Fatalf("%s has no check_wake row", issue)
	}
	row := rows[len(rows)-1]
	return row.nextAt, decodeWake(t, row)
}

// startLookAt applies the check_wake row of wake at its time, as the outbox does when the row comes
// due, and fails the test unless the look after it is then open.
func startLookAt(t *testing.T, rig checkRig, issue string, at time.Time, wake record.CheckWakeDue) {
	t.Helper()
	rig.clock.now = at
	rig.apply(fmt.Sprintf("wake-%d", wake.Look), intake.CheckWakeOf(issue, wake))
	if check := productionCheck(t, rig.pool, issue); check.Looks != wake.Look+1 || !check.LookOpen {
		t.Fatalf("the check at the wake of look %d = %+v, want look %d open", wake.Look, check, wake.Look+1)
	}
}

// failLook fails the checking role's claim at at, inside the look the check holds open, and
// answers when the next look is due and its wake.
func failLook(t *testing.T, rig checkRig, issue string, at time.Time) (time.Time, record.CheckWakeDue) {
	t.Helper()
	rig.clock.now = at
	look := productionCheck(t, rig.pool, issue).Looks
	rig.apply(fmt.Sprintf("tester-failed-%d", look), failedClaim(issue, claim.RoleTester))
	check := productionCheck(t, rig.pool, issue)
	if check.LookOpen || check.Looks != look {
		t.Fatalf("the check after its look %d lost its tester = %+v, want that look ended", look, check)
	}
	due, wake := lastWake(t, rig, issue)
	if wake.Look != look || check.WakeAt == nil || !check.WakeAt.Equal(due) {
		t.Fatalf("the check after its look %d lost its tester = %+v, its newest wake %+v due %s; want the wake of that look, the one the check records", look, check, wake, due)
	}
	return due, wake
}

// The checking role's claim failing in look after look backs the check off: the first failed look
// names the next a look bound (2 h) out, and each failed look in a row after it doubles that, so
// the second and third failures name wakes 4 h and 8 h after their looks, and the fourth 16 h.
// The doubling never names a wake past the check's bound: the fifth, 32 h out from a look 30 h
// into a 48 h check, names none, and the bound's own row starts the last look. Each look after a
// failed one is a start of the checking role, which the outbox runs as the retry of the failed
// claim with one attempt left (TestCheckLookStartRetriesAFailedClaimWithOneAttemptLeft). At main
// every failed look named its next one 2 h out, so a role that could not start was launched a
// whole budget's worth of times every two hours until the check's bound.
func TestACheckRoleFailureDoublesTheNextLookInterval(t *testing.T) {
	rig := childCheckRig(t)
	at := checkOpened
	for i, want := range []time.Duration{2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 16 * time.Hour} {
		due, wake := failLook(t, rig, "LEGION-209", at)
		if got := due.Sub(at); got != want {
			t.Fatalf("failure %d at %s named the next look %s out, want %s", i+1, at, got, want)
		}
		startLookAt(t, rig, "LEGION-209", due, wake)
		starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
		if last := starts[len(starts)-1]; last.CheckLook != wake.Look+1 || last.Role != claim.RoleTester || last.FreshSession {
			t.Fatalf("the newest start after failure %d = %+v, want look %d handed to the tester, asking no new session", i+1, last, wake.Look+1)
		}
		at = due
	}
	wakes := len(checkWakeRows(t, rig.pool, "LEGION-209"))
	rig.clock.now = at
	rig.apply("tester-failed-5", failedClaim("LEGION-209", claim.RoleTester))
	check := productionCheck(t, rig.pool, "LEGION-209")
	if !check.Open() || check.LookOpen || check.WakeAt != nil {
		t.Fatalf("the check after its fifth failed look, %s into a %s check = %+v, want it open, the look ended and no wake: 32 h out is past its bound", at.Sub(checkOpened), checkBound, check)
	}
	if got := len(checkWakeRows(t, rig.pool, "LEGION-209")); got != wakes {
		t.Fatalf("check_wake rows = %d after the fifth failed look, want the %d before it: no wake past the bound", got, wakes)
	}
}

// A look its role reports ends the run of failed looks: after two failed looks (2 h, then 4 h),
// the third look's tester reports pending with no time of its own, which names the next look a
// look bound out as every such report does, and the failure of the look after it names its next
// look 2 h out again, not 8 h.
func TestAReportedLookResetsTheCheckRoleBackoffToTwoHours(t *testing.T) {
	rig := childCheckRig(t)
	due, wake := failLook(t, rig, "LEGION-209", checkOpened)
	startLookAt(t, rig, "LEGION-209", due, wake)
	due, wake = failLook(t, rig, "LEGION-209", due)
	if got := due.Sub(checkOpened.Add(CheckLookBound)); got != 4*time.Hour {
		t.Fatalf("the second failed look named its next %s out, want 4h: the backoff this test resets did not build", got)
	}
	startLookAt(t, rig, "LEGION-209", due, wake)

	reported := due.Add(20 * time.Minute)
	rig.clock.now = reported
	rig.apply("pending-3", intake.CheckReport{Issue: "LEGION-209", Look: 3, Verdict: "pending", Summary: "the deploy has not carried the merge"})
	due, wake = lastWake(t, rig, "LEGION-209")
	if wake.Look != 3 || !due.Equal(reported.Add(CheckLookBound)) {
		t.Fatalf("the newest wake after look 3's pending = %+v due %s, want look 3's a look bound out, at %s", wake, due, reported.Add(CheckLookBound))
	}
	startLookAt(t, rig, "LEGION-209", due, wake)

	failed, _ := failLook(t, rig, "LEGION-209", due)
	if got := failed.Sub(due); got != CheckLookBound {
		t.Fatalf("the failed look after a reported one named its next %s out, want %s: a report ends the run of failed looks", got, CheckLookBound)
	}
}

// A claim of the checking role that fails after its look ended — the look's bound ended it while
// the claim was still failing its way through its budget, so no look is open — counts as a failed
// look too, and names the next look the backoff out from the failure, as a failure inside a look
// does: a look bound for the first failed look in a row, and double that after a failed look
// before it. The look that ended at its bound named its next a look bound out, and that wake no
// longer stands: its row comes due and starts nothing, and the new wake's row starts the next
// look, whose start retries the claim as any look after a failed one does, with one attempt left
// (the daemon's outbox). The architect is told at once in a worker-died that names the look, the
// failure's cause and that retry, and the failure of the look the new wake starts doubles the wait
// again. At main a failure between looks reached nothing at all. A failure that only counts the
// failed look leaves the look bound's wake standing, so the look after a streak of failures comes
// a look bound after that bound rather than the doubled wait after the failure.
func TestACheckRoleFailureBetweenLooksIsRetriedAtTheNextLook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		streak int           // failed looks in a row before the look that ends at its bound
		wait   time.Duration // from the failure between looks to the look it names
	}{
		{name: "the first failed look in a row", streak: 0, wait: CheckLookBound},
		{name: "after a failed look", streak: 1, wait: 2 * CheckLookBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			at := checkOpened
			for range tc.streak {
				due, wake := failLook(t, rig, "LEGION-209", at)
				startLookAt(t, rig, "LEGION-209", due, wake)
				at = due
			}
			look := tc.streak + 1
			bound := at.Add(CheckLookBound)
			rig.clock.now = bound
			rig.apply(fmt.Sprintf("look-bound-%d", look), intake.CheckLookEnded{Issue: "LEGION-209", Look: look, Cause: "look bound", OpenedAt: checkOpened})
			stood, old := lastWake(t, rig, "LEGION-209")
			if old.Look != look || !stood.Equal(bound.Add(CheckLookBound)) {
				t.Fatalf("the wake look %d's bound named = %+v due %s, want a look bound out, at %s", look, old, stood, bound.Add(CheckLookBound))
			}
			before := len(noticesOfKind(t, rig.pool, "LEGION-209", "worker-died"))

			failedAt := bound.Add(10 * time.Minute)
			rig.clock.now = failedAt
			rig.apply("tester-failed-between", failedClaim("LEGION-209", claim.RoleTester))
			due := failedAt.Add(tc.wait)
			check := productionCheck(t, rig.pool, "LEGION-209")
			if !check.Open() || check.LookOpen || check.Looks != look || check.FailedLooks != tc.streak+1 || check.WakeAt == nil || !check.WakeAt.Equal(due) {
				t.Fatalf("the check after its tester failed between looks = %+v, want it open after look %d, %d failed looks in a row and its next look at %s, %s after the failure", check, look, tc.streak+1, due, tc.wait)
			}
			named, wake := lastWake(t, rig, "LEGION-209")
			if wake.Look != look || !named.Equal(due) {
				t.Fatalf("the newest wake after the failure between looks = %+v due %s, want look %d's at %s", wake, named, look, due)
			}
			died := noticesOfKind(t, rig.pool, "LEGION-209", "worker-died")
			if len(died) != before+1 {
				t.Fatalf("worker-died notices = %d after the failure between looks, want %d: the architect is told", len(died), before+1)
			}
			reason := died[len(died)-1].Reason
			for _, want := range []string{
				fmt.Sprintf("production check of LEGION-209 lost its tester after look %d ended: ", look),
				"its cause is never_registered",
				fmt.Sprintf("look %d retries the claim at %s with one attempt left of the budget its failure spent", look+1, due.Format(time.RFC3339)),
			} {
				if !strings.Contains(reason, want) {
					t.Errorf("the worker-died of the failure between looks reads %q, want it to say %q", reason, want)
				}
			}

			rig.clock.now = stood
			rig.apply(fmt.Sprintf("wake-%d-of-the-look-bound", look), intake.CheckWakeOf("LEGION-209", old))
			if check := productionCheck(t, rig.pool, "LEGION-209"); check.LookOpen || check.Looks != look {
				t.Fatalf("the check at the wake look %d's bound named = %+v, want no look started: the failure between looks named the next look later", look, check)
			}
			startLookAt(t, rig, "LEGION-209", due, wake)
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if last := starts[len(starts)-1]; last.CheckLook != look+1 || last.Role != claim.RoleTester || last.FreshSession {
				t.Fatalf("the newest start at the new wake = %+v, want look %d handed to the tester, asking no new session", last, look+1)
			}
			failed, _ := failLook(t, rig, "LEGION-209", due)
			if got := failed.Sub(due); got != 2*tc.wait {
				t.Fatalf("look %d's failure after the failure between looks named its next look %s out, want %s: one more failed look in a row", look+1, got, 2*tc.wait)
			}
		})
	}
}

// A check_wake row an earlier release queued carries no wakeAt, and checkWakeDue cannot fence one
// by the wake it was queued for. A failure of the checking role's claim between looks that moves
// the next look later queues its replacement and retires such a row of the check, so the row the
// earlier release queued for the look bound's retry starts no look early: every queued check_wake
// row is fired at its own time, oldest due first, as the outbox fires them, and the next look
// starts at the moved wake, the time the worker-died notice and the check's record name. Before
// the retirement the earlier release's row started look 2 at the look bound's retry, two hours
// out, and the replacement then found the look advanced and started nothing.
func TestAFailureBetweenLooksRetiresTheWakeAnEarlierReleaseQueued(t *testing.T) {
	rig := childCheckRig(t)
	bound := checkOpened.Add(CheckLookBound)
	rig.clock.now = bound
	rig.apply("look-bound-1", intake.CheckLookEnded{Issue: "LEGION-209", Look: 1, Cause: "look bound", OpenedAt: checkOpened})
	if _, err := rig.pool.Exec(t.Context(), `update outbox set payload = payload - 'wakeAt' where kind = 'check_wake' and issue = 'LEGION-209'`); err != nil {
		t.Fatalf("make the look bound's wake row one an earlier release queued: %v", err)
	}
	stood, legacy := lastWake(t, rig, "LEGION-209")
	if !legacy.WakeAt.IsZero() || !stood.Equal(bound.Add(CheckLookBound)) {
		t.Fatalf("the earlier release's wake row = %+v due %s, want no wakeAt, due a look bound after the bound", legacy, stood)
	}

	failedAt := bound.Add(10 * time.Minute)
	rig.clock.now = failedAt
	rig.apply("tester-failed-between", failedClaim("LEGION-209", claim.RoleTester))
	due := failedAt.Add(CheckLookBound)
	if check := productionCheck(t, rig.pool, "LEGION-209"); check.WakeAt == nil || !check.WakeAt.Equal(due) {
		t.Fatalf("the check after its tester failed between looks = %+v, want its next look at %s", check, due)
	}

	rows := checkWakeRows(t, rig.pool, "LEGION-209")
	slices.SortStableFunc(rows, func(a, b outboxRow) int { return a.nextAt.Compare(b.nextAt) })
	for _, row := range rows {
		wake := decodeWake(t, row)
		rig.clock.now = row.nextAt
		rig.apply(fmt.Sprintf("wake-row-%d", row.id), intake.CheckWakeOf("LEGION-209", wake))
		check := productionCheck(t, rig.pool, "LEGION-209")
		if row.nextAt.Before(due) && (check.LookOpen || check.Looks != 1) {
			t.Fatalf("the check_wake row %d due %s (%+v) started look %d, want no look before the moved wake at %s", row.id, row.nextAt, wake, check.Looks, due)
		}
	}
	if check := productionCheck(t, rig.pool, "LEGION-209"); !check.LookOpen || check.Looks != 2 || check.LookWakeAt == nil || !check.LookWakeAt.Equal(due) {
		t.Fatalf("the check after every queued wake fired = %+v, want look 2 open, woken at the moved wake %s", check, due)
	}
}
