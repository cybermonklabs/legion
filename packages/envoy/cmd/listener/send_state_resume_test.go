package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// End to end on one NATS server with the relay running as main runs it: a session that registers
// with its consumer and its relay subscription reads connected; the same session, after a clean
// stop, reads stopped with its record's last_seen rather than the 404 that would tell its sender
// to route around it; and the message stored while it was stopped reaches it exactly once when it
// registers again under the same id. The clean stop deletes the session's consumer after it
// answers, recording where the consumer stopped, and the registration that follows makes the
// consumer again from that resume point (sessionDelivery.deleteAfterAnswer).
//
// The "exactly once" is read from the session consumer's own acknowledgement state rather than
// watched for: the server delivers a pulled message again only once the consumer's ack wait
// (bus.SessionConsumerAckWait) passes without that message's acknowledgement, so a consumer whose
// ack floor has reached the resumed frame, with nothing pending and the one delivery made, has
// nothing to deliver again. A relay that handed the frame over without acknowledging it leaves
// the frame pending, which that read catches where a watch would have to outlast the ack wait,
// and an immediate duplicate is the stand-in's own quiet check (sessionStandIn.none).
func TestASessionThatStopsAndResumesTakesItsMessageExactlyOnce(t *testing.T) {
	rig := newRelayRig(t)
	clock := time.Now()
	known := openKnownSessionsOn(t, rig.harness.client.Conn, func() time.Time { return clock })
	rig.deps.known = known
	const sessionID = "ses_stop_and_resume"

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
	frames := first.collect(t, 1)
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
	first.conn.Close()
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
	resumed := second.collect(t, 1)
	if resumed[0].item.EventID != stopped.EventID {
		t.Fatalf("the resumed session took %s, want the message stored while it was stopped, %s",
			resumed[0].item.EventID, stopped.EventID)
	}

	consumer, ok := bus.SessionConsumerName(sessionID)
	if !ok {
		t.Fatalf("no session consumer name for %s", sessionID)
	}
	testwait.Until(t, "the resumed session's consumer to hold the resumed frame acknowledged", func() (string, bool) {
		info, err := rig.harness.client.JS().ConsumerInfo(bus.Stream, consumer)
		if err != nil {
			t.Fatalf("read %s's session consumer: %v", sessionID, err)
		}
		acknowledged := info.AckFloor.Stream >= resumed[0].seq && info.NumAckPending == 0
		if acknowledged && info.Delivered.Consumer != 1 {
			t.Fatalf("the remade consumer delivered %d times, want the resumed frame once: %+v", info.Delivered.Consumer, info)
		}
		return fmt.Sprintf("%+v", info), acknowledged
	})
	second.none(t)
}
