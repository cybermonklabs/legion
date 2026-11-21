package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// relayTooLargeLine is the WARN a relay writes when it acknowledges a stored message too large to
// relay. It is a string here rather than the relay's own, so the row compiles against a relay
// that has no such line.
const relayTooLargeLine = "session relay acknowledged a message too large to relay; the session does not receive it"

// relayTooLargeRecord is what a row reads of a log record.
type relayTooLargeRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	SessionID string `json:"session_id"`
	EventID   string `json:"event_id"`
	StreamSeq uint64 `json:"stream_seq"`
}

// relayTooLargeWarnings is every WARN record of relayTooLargeLine the rig's listener wrote for
// sessionID.
func relayTooLargeWarnings(rig *relayRig, sessionID string) []relayTooLargeRecord {
	var found []relayTooLargeRecord
	for line := range strings.SplitSeq(rig.harness.logs.String(), "\n") {
		var record relayTooLargeRecord
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record.Level == "WARN" && record.Msg == relayTooLargeLine && record.SessionID == sessionID {
			found = append(found, record)
		}
	}
	return found
}

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

// A stored message can fit the server's max payload while its relay frame, the same bytes with
// the relay's headers, does not, and NATS refuses that frame however often it is sent. The relay
// acknowledges such a message, writes one WARN naming the session, the event and the stream
// sequence, and goes on, so the session's next message arrives. Before, the relay restarted at
// the ack floor at each refusal, and the session received nothing after that message until the
// stream's retention took it (dispatch://INFRA-3918). The refused frame never went out, so the
// relay waits for no receipt of it: under a receipt wait shorter than the row's own wait, the
// relay does not pause the session. The control: a message half the limit is relayed as any
// other, with no WARN.
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
		warnings := relayTooLargeWarnings(rig, sessionID)
		if len(warnings) != 1 || warnings[0].EventID != large.EventID || warnings[0].StreamSeq != largeSeq {
			t.Fatalf("the relay wrote %+v, want one WARN %q naming event %s at stream sequence %d", warnings, relayTooLargeLine, large.EventID, largeSeq)
		}
		if line := "session relay could not publish a frame"; strings.Contains(rig.harness.logs.String(), line) {
			t.Fatalf("the relay logged %q, want the message acknowledged rather than sent again", line)
		}
		for line := range strings.SplitSeq(rig.harness.logs.String(), "\n") {
			var record relayTooLargeRecord
			if json.Unmarshal([]byte(line), &record) == nil && record.SessionID == sessionID &&
				strings.HasPrefix(record.Msg, "session relay paused: the session answered no frame") {
				t.Fatalf("the relay paused the session: %s; want no receipt wait for a frame that never went out", line)
			}
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
		if warnings := relayTooLargeWarnings(rig, sessionID); len(warnings) != 0 {
			t.Fatalf("the relay wrote %+v for a message half the limit, want no WARN", warnings)
		}
	})
}
