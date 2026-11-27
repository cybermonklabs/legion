package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// relayTooLargeLine is the WARN a relay writes each time it sends a session a notice in place of a
// stored message too large to relay (sessionRelays.publishTooLargeNotice). The relay writes that
// message as a literal in its one log call, so the rows hold their own copy.
const relayTooLargeLine = "session relay sent a notice in place of a message too large to relay; the session's answer to the notice settles the message"

// relayPausedPrefix begins the INFO line a relay writes when it pauses a session that answered no
// frame within the receipt wait.
const relayPausedPrefix = "session relay paused: the session answered no frame"

// storeEnvelopeOfSize stores on subject, raw and with no headers, an envelope whose serialized size
// is size bytes, its payload padded to that, and returns it with its stream sequence.
func storeEnvelopeOfSize(t *testing.T, rig *relayRig, subject string, size int) (contracts.Envelope, uint64) {
	t.Helper()
	item := listenerTestEnvelope(subject, "relay-size-"+id.New())
	item.Payload = `{"pad":""}`
	pad := size - len(marshalListenerEnvelope(t, item))
	if pad < 0 {
		t.Fatalf("an envelope with an empty pad is %d bytes, past the %d asked for", size-pad, size)
	}
	item.Payload = `{"pad":"` + strings.Repeat("p", pad) + `"}`
	data := marshalListenerEnvelope(t, item)
	if len(data) != size {
		t.Fatalf("the envelope serializes to %d bytes, want %d", len(data), size)
	}
	ack, err := rig.harness.client.JS().Publish(subject, data)
	if err != nil {
		t.Fatalf("store the %d-byte envelope on %s: %v", size, subject, err)
	}
	return item, ack.Sequence
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: a relay's receipt wait of 500 ms, which the silent session's pause waits out.
//
// A stored message can fit the server's max payload while its relay frame, the same bytes with
// the relay's headers, does not, and NATS refuses that frame however often it is sent. The relay
// acknowledges such a message, writes one WARN naming the session, the event and the stream
// sequence, and goes on, so the session's next message arrives. A relay that restarted at the ack
// floor at each refusal would deliver the session nothing after that message until the stream's
// retention took it (dispatch://INFRA-3918). The refused frame never went out, so the relay waits
// for no receipt of it: under a receipt wait shorter than the row's own wait, the relay does not
// pause the session. The controls: a message half the limit is relayed as any other, with no
// WARN; and a session that answers no frame is paused once its receipt wait has passed, with one
// line.
func TestRelayStoredNearLimitEnvelopeDoesNotBlockFollowingFrame(t *testing.T) {
	rig := newRelayRigReceipt(t, relayPauseFirst, relayPauseMax, 500*time.Millisecond)
	limit := int(rig.harness.client.Conn.MaxPayload())

	t.Run("a message 64 bytes under the limit is acknowledged with one WARN", func(t *testing.T) {
		const sessionID = "ses_near_limit"
		agent := contracts.LegacyAgentSubject(sessionID)
		standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		large, largeSeq := storeEnvelopeOfSize(t, rig, agent, limit-64)
		marker, markerSeq := rig.storeEvent(t, agent)

		requireFrames(t, standIn.collect(t, 1, 15*time.Second), marker)
		standIn.none(t, time.Second)
		awaitAcked(t, rig.consumers, sessionID, markerSeq)
		warnings := edgeSessionLogLines(rig.harness.logs.String(), "WARN", relayTooLargeLine, sessionID)
		if len(warnings) != 1 || warnings[0]["event_id"] != large.EventID || warnings[0]["stream_seq"] != float64(largeSeq) {
			t.Fatalf("the relay wrote %+v, want one WARN %q naming event %s at stream sequence %d", warnings, relayTooLargeLine, large.EventID, largeSeq)
		}
		if n := len(edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayPausedPrefix, sessionID)); n != 0 {
			t.Fatalf("the relay paused the session %d times; want no receipt wait for a frame that never went out:\n%s", n, rig.harness.logs.String())
		}
	})

	t.Run("a message half the limit is relayed with no WARN", func(t *testing.T) {
		const sessionID = "ses_half_limit"
		agent := contracts.LegacyAgentSubject(sessionID)
		standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		half, _ := storeEnvelopeOfSize(t, rig, agent, limit/2)
		marker, markerSeq := rig.storeEvent(t, agent)

		requireFrames(t, standIn.collect(t, 2, 15*time.Second), half, marker)
		awaitAcked(t, rig.consumers, sessionID, markerSeq)
		if warnings := edgeSessionLogLines(rig.harness.logs.String(), "WARN", relayTooLargeLine, sessionID); len(warnings) != 0 {
			t.Fatalf("the relay wrote %+v for a message half the limit, want no WARN", warnings)
		}
	})

	t.Run("control: a session that answers no frame is paused, with one line", func(t *testing.T) {
		const sessionID = "ses_near_limit_silent"
		agent := contracts.LegacyAgentSubject(sessionID)
		// A nil answer leaves the stand-in silent, so the frame it is sent stays unanswered.
		standIn := startSessionStandIn(t, rig.url, sessionID, nil)
		rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
		marker, _ := rig.storeEvent(t, agent)

		requireFrames(t, standIn.collect(t, 1, 15*time.Second), marker)
		waitFor(t, "the relay to pause the silent session", func() bool {
			return len(edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayPausedPrefix, sessionID)) > 0
		})
		if n := len(edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayPausedPrefix, sessionID)); n != 1 {
			t.Fatalf("the relay paused the silent session %d times, want once:\n%s", n, rig.harness.logs.String())
		}
	})
}
