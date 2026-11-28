package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/session"
)

// End to end on one NATS server with the relay running as main runs it: a session that registers
// with its consumer and its relay subscription reads connected; the same session, after a clean
// stop, reads stopped with its record's last_seen rather than the 404 that would tell its sender
// to route around it; and the message stored while it was stopped reaches it exactly once when it
// registers again under the same id. The clean stop deletes the session's consumer after it
// answers, recording where the consumer stopped, and the registration that follows makes the
// consumer again from that resume point (sessionDelivery.deleteAfterAnswer).
//
// The "exactly once" is read from the server rather than waited out: a relay that handed the frame
// over without acknowledging it would leave the message pending on the consumer, and the server
// would deliver it again once the consumer's ack wait (bus.SessionConsumerAckWait, a minute) ran
// out. So the test waits for the consumer to hold nothing pending with the resumed message acked,
// and for its count of messages delivered again to read zero; the server delivers again only a
// message it holds pending, so an acknowledged one never comes back. A copy sent twice at once,
// which needs no ack wait to arrive, is watched for over a short window first.
func TestASessionThatStopsAndResumesTakesItsMessageExactlyOnce(t *testing.T) {
	rig := newRelayRig(t)
	clock := time.Now()
	known := openKnownSessionsOn(t, rig.harness.client.Conn, func() time.Time { return clock })
	rig.deps.known = known
	const sessionID = "ses_stop_and_resume"
	started := time.Now()

	first := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.register(t, sessionID, map[string]any{"delivery": map[string]any{"consumer": true, "epoch": "epoch-1"}})

	connected := sendThroughRoutes(t, rig.mux, sessionID, map[string]any{"message": "while you are here"})
	if connected.status != http.StatusOK || connected.State != sessionStateConnected || connected.RecipientState != string(session.Live) {
		t.Fatalf("a send to the registered session: status %d, body %s; want 200 connected and live",
			connected.status, connected.raw)
	}
	if probed := first.probed.Load(); probed != 1 {
		t.Fatalf("the plugin was probed %d times, want once", probed)
	}
	// The message sent while it was there is relayed and answered now, so the one collected
	// after the resume below can only be the message stored while it was stopped.
	frames := first.collect(t, 1, 10*time.Second)
	if frames[0].item.EventID != connected.EventID {
		t.Fatalf("the connected session took %s, want the message just sent, %s",
			frames[0].item.EventID, connected.EventID)
	}
	// The stop below deletes the consumer, which the registration after it creates again, so the
	// ack the session's done makes is in the consumer's state file first.
	awaitAcked(t, rig.consumers, sessionID, frames[0].seq)
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)

	// The session shuts down cleanly: its plugin deregisters and its process, and with it the
	// connection holding its relay subject, goes away.
	clock = clock.Add(time.Minute)
	stopSession(t, rig.mux, sessionID, "")
	first.close()
	if _, err := rig.harness.sessions.Get(sessionID); err == nil {
		t.Fatalf("%s still holds a session entry, so it did not stop", sessionID)
	}
	rig.awaitNoConsumer(t, sessionID)
	record, recorded := known.Get(sessionID)
	if !recorded {
		t.Fatalf("the clean stop left no known-session record of %s", sessionID)
	}

	stopped := sendThroughRoutes(t, rig.mux, sessionID, map[string]any{"message": "while you were away"})
	if stopped.status != http.StatusOK || stopped.State != sessionStateStopped || stopped.RecipientState != string(session.Gone) {
		t.Fatalf("a send to the stopped session: status %d, body %s; want 200 stopped and gone, never 404",
			stopped.status, stopped.raw)
	}
	if stopped.LastSeen != record.LastSeen {
		t.Fatalf("the stopped answer's last_seen = %d, want the record's %d: %s",
			stopped.LastSeen, record.LastSeen, stopped.raw)
	}

	// It runs again under the same id, on a new NATS connection, which is a new epoch: the
	// registration makes its consumer again from the point the stop recorded, and the relay hands
	// it what it never acknowledged.
	second := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.register(t, sessionID, map[string]any{"delivery": map[string]any{"consumer": true, "epoch": "epoch-2"}})
	resumed := second.collect(t, 1, 30*time.Second)
	if resumed[0].item.EventID != stopped.EventID {
		t.Fatalf("the resumed session took %s, want the message stored while it was stopped, %s",
			resumed[0].item.EventID, stopped.EventID)
	}

	// A second copy sent at once arrives inside this window.
	second.none(t, 2*time.Second)
	// A copy the server would send again after its ack wait is one it still holds pending; this one
	// is acknowledged, and nothing was delivered twice.
	awaitAcked(t, rig.consumers, sessionID, resumed[0].seq)
	info, err := rig.consumers.Info(t.Context(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer of %s after the resume: %v", sessionID, err)
	}
	if info.NumRedelivered != 0 {
		t.Fatalf("the consumer of %s delivered %d messages again after the resume, want none: the resumed message reached the session more than once", sessionID, info.NumRedelivered)
	}
	t.Logf("the stop-and-resume test took %s", time.Since(started).Round(time.Second))
}
