package outbox

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
)

// A role forward the stream never acknowledges holds back only its own chain: another issue's
// event, routed to a holder whose forward the stream does store, reaches that holder while the
// stalled forward is still outstanding, rather than when its wait runs out.
//
// The verdict is the order of those two facts rather than a clock, because a clock measures the
// machine as much as the outbox: this row bounded the live holder's forward at 1 s and waited
// 10 s for the stalled one, and under 48 busy loops on this 2-CPU box those two bounds failed 14
// of 20 runs with nothing wrong in any of them - 11 at the 1 s bound, the slowest at 34.566 s,
// and 3 at the 10 s wait.
//
// A forward counts as reached on the stream's acknowledgement of the copy it stored, and each
// retry of an event whose copy the stream does not store waits the whole roleForwardAckTimeout
// for one. If no other send can start while that wait runs, every event Dispatch commits during
// it - on every issue, to every session - reaches its reader up to 5 s late, and again at each
// retry of the stuck event. The tester's live run at fd93819c measured that against the receipt
// wait this bound replaces: with two issues routed to a holder that never answered, 24 of 30
// comments on a third issue reached their holder more than 1 s late (median 2.8 s, max 5.0 s),
// against 3 of 30 on main's build.
func TestAForwardTheStreamDoesNotAcknowledgeDoesNotDelayAnotherIssuesEvents(t *testing.T) {
	const (
		stalledRole, stalledHolder = "stalled-reviewer", "ses_never_acknowledged"
		liveRole, liveHolder       = "live-reviewer", "ses_acknowledged"
		// What the outbox waits for the stalled forward's acknowledgement (Deps.forwardAckWait).
		// It outlasts the whole test, so the wait the live holder's send must not queue behind is
		// still running when the verdict below is read, however slow the machine is.
		stalledForwardWait = 10 * time.Minute
		// A hang guard on each wait for a forward, not a bound this row measures anything
		// against: a healthy run reaches both of them in milliseconds, and a send queued behind
		// the stalled forward's wait arrives only after stalledForwardWait, ten times this.
		hangGuard = 60 * time.Second
	)
	rig := newRoleRig(t)
	rig.claim(t, stalledRole, stalledHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	// The stream stores and acknowledges the live holder's forwards alone, so the stalled
	// holder's wait out their whole acknowledgement wait.
	rig.narrowAgentCapture(t, liveHolder)
	stalled := rig.receiver(t, stalledHolder)
	live := rig.receiver(t, liveHolder)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	deps.forwardAckWait = stalledForwardWait
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	held := rig.routedEvent(t, "T-1", stalledRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-stalled.arrived:
	case <-time.After(hangGuard):
		t.Fatalf("the stalled holder never received its forward within %s", hangGuard)
	}

	// That forward's wait for an acknowledgement is now running. Another issue's event is
	// committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.routedEvent(t, "T-2", liveRole, model.Actor{Kind: "user", ID: "alice"})
	select {
	case <-live.arrived:
		// T-2's forward counts only while the stalled forward is still outstanding: one that
		// arrived after that send had settled would prove nothing about what its wait holds back.
		if settled, state := settledState(t, rig.database, held.ID); settled {
			t.Fatalf("the stalled holder's event %d had been settled when T-2's forward arrived (%s), so "+
				"T-2 followed that send rather than going out beside it: the %s acknowledgement wait "+
				"should still have been running", held.ID, state, stalledForwardWait)
		}
		// Logged, not asserted: how long T-2's forward took is as much a measure of the machine
		// as of the outbox.
		t.Logf("the live holder received T-2's forward %s after its commit",
			time.Since(committed).Round(time.Millisecond))
	case <-time.After(hangGuard):
		t.Fatalf("the live holder never received T-2's forward within %s, while the stalled "+
			"forward's %s acknowledgement wait was still running: that wait held back another "+
			"issue's send", hangGuard, stalledForwardWait)
	}
}
