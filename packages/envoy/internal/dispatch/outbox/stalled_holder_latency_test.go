package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// A role holder that takes its forward and never sends the receipt holds back only its own
// chain: another issue's event, routed to a holder that answers, reaches that holder while the
// silent holder's forward is still outstanding, rather than when its receipt wait runs out.
//
// The verdict is the order of those two facts rather than a clock, because a clock measures the
// machine as much as the outbox: this row bounded the live holder's forward at 1 s and waited
// 10 s for the silent holder's, and under 48 busy loops on this 2-CPU box those two bounds
// failed 14 of 20 runs with nothing wrong in any of them - 11 at the 1 s bound, the slowest at
// 34.566 s, and 3 at the 10 s wait.
//
// A session that died holding a role stays registered and reads live for up to
// session.LiveWindow, and each retry of its issue's event waits the whole roleReceiptTimeout
// for a receipt that never comes. If no other send can start while that wait runs, every event
// Dispatch commits during it - on every issue, to every session - reaches its reader up to 5 s
// late, and again at each retry of the stuck event, until the holder stops reading live. The tester's
// live run at fd93819c measured that: with two issues routed to a holder
// that never answers, 24 of 30 comments on a third issue reached their answering holder more
// than 1 s late (median 2.8 s, max 5.0 s), against 3 of 30 on main's build.
func TestAHolderThatNeverAnswersDoesNotDelayAnotherIssuesEvents(t *testing.T) {
	const (
		silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
		liveRole, liveHolder     = "live-reviewer", "ses_answers"
		// What the outbox waits for the silent holder's receipt (Deps.RoleReceiptTimeout). It
		// outlasts the whole test, so the wait the live holder's send must not queue behind is
		// still running when the verdict below is read, however slow the machine is.
		silentHolderWait = 10 * time.Minute
		// A hang guard on each wait for a forward, not a bound this row measures anything
		// against: a healthy run reaches both of them in milliseconds, and a send queued behind
		// the silent holder's receipt wait arrives only after silentHolderWait, ten times this.
		hangGuard = 60 * time.Second
	)
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)
	live := rig.receiver(t, liveHolder, true)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	deps.RoleReceiptTimeout = silentHolderWait
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	held := rig.routedEvent(t, "T-1", silentRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-silent.arrived:
	case <-time.After(hangGuard):
		t.Fatalf("the silent holder never received its forward within %s", hangGuard)
	}

	// The silent holder's receipt wait is now running. Another issue's event is committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.routedEvent(t, "T-2", liveRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-live.arrived:
		// T-2's forward counts only while the silent holder's forward is still outstanding: one
		// that arrived after the silent holder's send had settled would prove nothing about what
		// that wait holds back.
		if settled, state := settledState(t, rig.database, held.ID); settled {
			t.Fatalf("the silent holder's event %d had been settled when T-2's forward arrived (%s), so "+
				"T-2 followed that send rather than going out beside it: the %s receipt wait should "+
				"still have been running", held.ID, state, silentHolderWait)
		}
		// Logged, not asserted: how long T-2's forward took is as much a measure of the machine
		// as of the outbox.
		t.Logf("the answering holder received T-2's forward %s after its commit",
			time.Since(committed).Round(time.Millisecond))
	case <-time.After(hangGuard):
		t.Fatalf("the answering holder never received T-2's forward within %s, while the silent "+
			"holder's %s receipt wait was still running: that wait held back another issue's send",
			hangGuard, silentHolderWait)
	}
}
