package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/listenerwire"
)

// sendTextOfSize is the message text whose send to sessionID, under no idempotency key and no
// source session, makes the envelope the route builds (contracts.AgentSendEnvelope) encode to
// exactly size bytes. A text past the summary's 160 runes is the envelope's payload whole, so
// each further 'm' adds one byte to it, and its ids and issue time have a fixed width.
func sendTextOfSize(t *testing.T, sessionID string, size int) string {
	t.Helper()
	envelopeSize := func(text string) int {
		item := contracts.AgentSendEnvelope(sessionID, "", listenerMessage(listenerwire.MessageBody{Source: "agent", Message: text}, contracts.ExpectsReplyUnset))
		return len(marshalListenerEnvelope(t, item))
	}
	const base = 1000
	text := strings.Repeat("m", base+size-envelopeSize(strings.Repeat("m", base)))
	if got := envelopeSize(text); got != size {
		t.Fatalf("UNKNOWN: the envelope of that send takes %d bytes, want %d", got, size)
	}
	return text
}

// relayFrameBytes is what the server's max payload holds a relay frame to: its data and its
// headers, never its subject or its reply.
func relayFrameBytes(msg *natsgo.Msg) int {
	return msg.Size() - len(msg.Subject) - len(msg.Reply)
}

// storeSized stores an envelope of exactly size bytes on subject, which must be the session's own
// agent subject or a topic its consumer filters, and returns it with its stream sequence. It is a
// publish of the stream's own, under no message id, so it stores what no route would accept: a
// message in the band whose frame the relay cannot publish.
func storeSized(t *testing.T, rig *relayRig, subject string, size int) (contracts.Envelope, uint64) {
	t.Helper()
	item := listenerTestEnvelope(subject, "relay-too-large-"+id.New())
	fixed := len(marshalListenerEnvelope(t, item))
	if size <= fixed {
		t.Fatalf("UNKNOWN: an envelope of %d bytes is smaller than its fixed fields, %d bytes", size, fixed)
	}
	item.Payload = strings.Repeat("p", size-fixed-len(`"payload":"",`))
	data := marshalListenerEnvelope(t, item)
	if len(data) != size {
		t.Fatalf("UNKNOWN: the envelope takes %d bytes, want %d", len(data), size)
	}
	ack, err := rig.harness.client.JS().Publish(subject, data)
	if err != nil {
		t.Fatalf("store a message of %d bytes on %s: %v", size, subject, err)
	}
	return item, ack.Sequence
}

// A stored message whose relay frame is past the server's max payload - the message within the
// bound, its frame past it by the relay's own headers - reaches its session as a notice naming
// it, and the session's later messages arrive behind it. Before this, the relay's publish of such
// a frame failed, the relay restarted at the consumer's ack floor, where the same message came
// first again, and no later message of that session was ever sent: measured at
// 68d0eb8f8985eead00d893262545d540f132a943, 62 restarts in 60 s and not one later frame
// (dispatch://INFRA-5995).
//
// The message is the largest the stream takes under no message id, which the listener's own check
// took before it counted the relay's headers (bus.RelayFrameHeaderBytes) and no route takes now,
// and it is stored by a publish of the stream's own: it stands for a message an older build
// stored, or one stored under a max payload the server has since lowered.
//
// What the row asks: the notice reaches the session in the frame's place, naming the message's
// event id, its topic and its size; the message is acknowledged once, so the consumer's ack floor
// passes it and nothing of it comes again; the later message reaches the session after it; and
// the listener writes one line for the notice. The control is the first message, relayed whole
// before the band message is stored, so a session that received nothing cannot pass the row.
func TestAFrameTooLargeToPublishBecomesANoticeAndTheSessionsLaterMessagesArrive(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	const sessionID = "ses_too_large_frame"
	agent := contracts.LegacyAgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	maxPayload := int(rig.harness.client.Conn.MaxPayload())
	reserved := bus.RelayFrameHeaderBytes(agent)

	before, beforeSeq := rig.storeEvent(t, agent)
	control := standIn.collect(t, 1, listenerWaitBound(t))
	requireFrames(t, control, before)
	awaitAcked(t, rig.consumers, sessionID, beforeSeq)

	// The max payload to the byte: the stream takes it, and its frame is past the bound by every
	// header the relay writes.
	banded, bandSeq := storeSized(t, rig, agent, maxPayload)
	after, _ := rig.storeEvent(t, agent)

	notice := requireEventuallyDelivered(t, standIn, noticeEventOf(t, rig, bandSeq), listenerWaitBound(t))
	payload := requireTooLargePayload(t, notice, banded, agent, bandSeq, len(marshalListenerEnvelope(t, banded)), int64(maxPayload))
	if payload.FrameSize <= maxPayload || payload.FrameSize-payload.Size > reserved {
		t.Errorf("the notice says the frame takes %d bytes for a message of %d: want it past the max payload of %d and within the message plus the relay's %d header bytes",
			payload.FrameSize, payload.Size, maxPayload, reserved)
	}
	if seq := notice.msg.Header.Get(relayHeaderStreamSeq); seq != strconv.FormatUint(bandSeq, 10) {
		t.Errorf("the notice carries stream sequence %q, want the message's own %d: the session answers it as it would the frame", seq, bandSeq)
	}
	requireEventuallyDelivered(t, standIn, after, listenerWaitBound(t))
	awaitAcked(t, rig.consumers, sessionID, bandSeq)
	standIn.none(t, time.Second)
	rig.requireOneLogLine(t, "WARN", relayTooLargeLine, sessionID, map[string]any{
		"stream_seq": bandSeq,
		"event_id":   banded.EventID,
		"topic":      agent,
	})
}

// noticeEventOf is the envelope a test waits for in place of the message stored at seq: only its
// event id is read, which the notice derives from the stream and the sequence
// (relayTooLargeNotice).
func noticeEventOf(t *testing.T, rig *relayRig, seq uint64) contracts.Envelope {
	t.Helper()
	data, _, err := relayTooLargeNotice(bus.SessionMessage{StreamSeq: seq, Subject: "x", Data: []byte("{}")}, rig.streamID(t), 0, 0)
	if err != nil {
		t.Fatalf("build the notice the relay would send for stream sequence %d: %v", seq, err)
	}
	var notice contracts.Envelope
	if err := json.Unmarshal(data, &notice); err != nil {
		t.Fatalf("decode that notice: %v", err)
	}
	return notice
}

// requireTooLargePayload fails the test unless frame carries the notice of the message banded,
// stored on subject at seq and taking size bytes against maxPayload, and returns its payload.
func requireTooLargePayload(t *testing.T, frame relayedFrame, banded contracts.Envelope, subject string, seq uint64, size int, maxPayload int64) relayTooLargePayload {
	t.Helper()
	var payload relayTooLargePayload
	if err := json.Unmarshal([]byte(frame.item.Payload), &payload); err != nil {
		t.Fatalf("decode the notice's payload %.300s: %v", frame.item.Payload, err)
	}
	if payload.Reason != relayTooLargeReason || payload.EventID != banded.EventID || payload.OriginalTopic != banded.Topic ||
		payload.StreamSeq != seq || payload.Size != size || payload.MaxPayload != maxPayload {
		t.Fatalf("the notice says %+v, want reason %s for event %s on %s at sequence %d, %d bytes against a max payload of %d",
			payload, relayTooLargeReason, banded.EventID, banded.Topic, seq, size, maxPayload)
	}
	if frame.item.Topic != banded.Topic || frame.item.Source != "envoy" {
		t.Fatalf("the notice is %s's own message on %s, want envoy's on the message's own topic %s", frame.item.Source, frame.item.Topic, banded.Topic)
	}
	if !strings.Contains(frame.item.PayloadSummary, banded.EventID) || !strings.Contains(frame.item.PayloadSummary, subject) {
		t.Fatalf("the notice's summary %q names neither the message %s nor its topic %s", frame.item.PayloadSummary, banded.EventID, subject)
	}
	return payload
}

// A message the send route accepts for a session reaches that session whole: the route's size
// check reserves the bytes the relay's headers add to the frame (bus.RelayFrameHeaderBytes), so
// the largest message it accepts is relayed, and one byte more is answered 413 naming the max
// payload. Without the reserve the route accepted the larger one, the stream stored it, and no
// frame of it could ever be published.
func TestTheLargestMessageTheRouteAcceptsIsRelayedWholeAndOneBytePastIs413(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	const sessionID = "ses_accepted_relays"
	agent := contracts.LegacyAgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	maxPayload := int(rig.harness.client.Conn.MaxPayload())
	largest := maxPayload - bus.RelayFrameHeaderBytes(agent)

	// A send carrying no source_session takes no sender stamp, so the envelope the route checks
	// is the one it stores: its text is grown until that envelope takes exactly the bytes the
	// frame leaves it.
	text := sendTextOfSize(t, sessionID, largest)
	accepted := sendThroughRoutes(t, rig.mux, sessionID, map[string]any{"source_session": "", "message": text})
	if accepted.status != http.StatusOK {
		t.Fatalf("a send whose envelope takes exactly the %d bytes the frame leaves it: status %d, body %.300s", largest, accepted.status, accepted.raw)
	}
	frame := requireEventuallyDelivered(t, standIn, contracts.Envelope{EventID: accepted.EventID}, listenerWaitBound(t))
	if len(frame.msg.Data) != largest {
		t.Errorf("the session was relayed %d bytes of a stored message of %d: the message must reach it whole", len(frame.msg.Data), largest)
	}
	if size := relayFrameBytes(frame.msg); size > maxPayload {
		t.Errorf("its frame takes %d bytes against the server's max payload of %d", size, maxPayload)
	}

	refused := sendThroughRoutes(t, rig.mux, sessionID, map[string]any{"source_session": "", "message": sendTextOfSize(t, sessionID, largest+1)})
	if refused.status != http.StatusRequestEntityTooLarge || !strings.Contains(refused.raw, "max payload of 1048576 bytes") {
		t.Fatalf("a send one byte past that: status %d, want 413 naming the server's max payload; body %.300s", refused.status, refused.raw)
	}
	standIn.none(t, time.Second)
}

// The relay's header is never larger than the room the listener's size check reserves for it
// (bus.RelayFrameHeaderBytes): the largest frame header the relay writes carries the gap and its
// longest reason, the highest stream sequence and delivery count a uint64 holds, a stream identity
// of 32 characters and a stored time in RFC 3339 with nanoseconds. A header the relay adds, or a
// value it writes longer, fails this row until the bound counts it.
func TestTheRelayFrameHeaderBoundIsTheRelaysLargestHeader(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{
		contracts.LegacyAgentSubject("ses_header_bound"),
		"notifications.dispatch.issue.INFRA-5995.comment",
		"notifications.github.cybermonklabs.legion.pull_request.opened",
	} {
		largest := relayFrameHeader(subject, 1<<64-1, strings.Repeat("f", 32), 1<<64-1,
			time.Date(2026, 11, 26, 20, 1, 14, 748147695, time.UTC), true, relayGapRepositoryWithheld)
		header := relayFrameBytes(&natsgo.Msg{Header: largest})
		if reserved := bus.RelayFrameHeaderBytes(subject); header > reserved {
			t.Errorf("the relay's largest header for %s takes %d bytes and the listener reserves %d", subject, header, reserved)
		}
	}
}
