package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// tooLargeRig is a relay rig with one session in consumer delivery whose stand-in answers each
// frame done, and the message of the server's max payload to the byte stored on the session's own
// subject once a first message has reached the session whole (the row's control: a session that
// received nothing cannot pass it).
//
// That message is the largest the stream takes under no message id. The listener's own checks took
// it before they counted the relay's headers (bus.RelayFrameHeaderBytes), and no route takes it
// now, so it is stored by a publish of the stream's own. It stands for a message this build's
// writes held to the stream's bound alone still store - a publish window's send of Dispatch's
// outbox, a delivery exception, a role's kept copy - for one an older build stored, and for one
// stored under a max payload the server has since lowered.
type tooLargeRig struct {
	*relayRig
	sessionID, agent string
	standIn          *sessionStandIn
	maxPayload       int
	banded           contracts.Envelope
	bandSeq          uint64
}

func newTooLargeRig(t *testing.T, sessionID string) tooLargeRig {
	t.Helper()
	rig := newRelayRig(t)
	agent := contracts.LegacyAgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, beforeSeq := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, listenerWaitBound(t)), before)
	awaitAcked(t, rig.consumers, sessionID, beforeSeq)
	maxPayload := int(rig.harness.client.Conn.MaxPayload())
	banded, bandSeq := storeSized(t, rig, agent, maxPayload)
	return tooLargeRig{relayRig: rig, sessionID: sessionID, agent: agent, standIn: standIn, maxPayload: maxPayload, banded: banded, bandSeq: bandSeq}
}

// A stored message whose relay frame is past the server's max payload - the message within the
// bound, its frame past it by the relay's own headers - reaches its session as one notice naming
// it (R2). Before this, the relay's publish of such a frame failed, the relay restarted at the
// consumer's ack floor, where the same message came first again, and the session was told
// nothing: measured at 68d0eb8f8985eead00d893262545d540f132a943, 62 restarts in 60 s and no frame
// at all (dispatch://INFRA-5995).
//
// What the row asks: the notice reaches the session in the frame's place, under the message's own
// stream sequence, naming its event id, its topic, its size and the frame's size past the max
// payload, and cutting nothing of a message whose texts are short; nothing else reaches the
// session for four times the relay's first retry wait, the span in which a relay that restarted
// at the ack floor sends the notice again; and the listener writes exactly one line for it. It
// asks nothing of the session's later messages, which are the next row's.
func TestAFrameTooLargeToPublishBecomesOneNoticeNamingTheMessage(t *testing.T) {
	t.Parallel()
	rig := newTooLargeRig(t, "ses_too_large_notice")
	reserved := bus.RelayFrameHeaderBytes(rig.agent)

	notice := requireEventuallyDelivered(t, rig.standIn, noticeEventOf(t, rig.relayRig, rig.bandSeq), listenerWaitBound(t))
	payload := requireTooLargePayload(t, notice, rig.banded, rig.agent, rig.bandSeq, len(marshalListenerEnvelope(t, rig.banded)), int64(rig.maxPayload))
	if payload.FrameSize <= int64(rig.maxPayload) || payload.FrameSize-int64(payload.Size) > int64(reserved) {
		t.Errorf("the notice says the frame takes %d bytes for a message of %d: want it past the max payload of %d and within the message plus the relay's %d header bytes",
			payload.FrameSize, payload.Size, rig.maxPayload, reserved)
	}
	if len(payload.Cut) != 0 {
		t.Errorf("the notice of a message whose texts are short says it cut %v, want nothing cut", payload.Cut)
	}
	if seq := notice.msg.Header.Get(relayHeaderStreamSeq); seq != strconv.FormatUint(rig.bandSeq, 10) {
		t.Errorf("the notice carries stream sequence %q, want the message's own %d: the session answers it as it would the frame", seq, rig.bandSeq)
	}
	rig.standIn.none(t, 4*relayRetryFirst)
	rig.requireOneLogLine(t, "WARN", relayTooLargeLine, rig.sessionID, map[string]any{
		"stream_seq": rig.bandSeq,
		"event_id":   rig.banded.EventID,
		"topic":      rig.agent,
	})
}

// A stored message whose relay frame is past the server's max payload holds none of its session's
// later messages (R1): the later message reaches the session behind its notice, and the
// consumer's ack floor passes the message, so nothing of it is sent again. Before this, no later
// message of that session was ever sent: measured at 68d0eb8f8985eead00d893262545d540f132a943, 62
// restarts at the ack floor in 60 s and not one later frame (dispatch://INFRA-5995).
//
// The notice may come before the later message any number of times; how often it comes is the row
// before's. This one asks only that the later message arrives and that the message is settled.
func TestTheSessionsLaterMessagesArriveBehindAFrameTooLargeToPublish(t *testing.T) {
	t.Parallel()
	rig := newTooLargeRig(t, "ses_too_large_later")
	after, _ := rig.storeEvent(t, rig.agent)
	requireEventuallyDelivered(t, rig.standIn, after, listenerWaitBound(t), noticeEventOf(t, rig.relayRig, rig.bandSeq))
	awaitAcked(t, rig.consumers, rig.sessionID, rig.bandSeq)
}

// A frame too large to publish is noticed once, however often the session's relay restarts after
// it: the session's done to the notice acknowledges the message (publishTooLargeNotice sends the
// notice under the frame's own reply subject), so the consumer's ack floor passes it and a restart
// at that floor has nothing of it to deliver again. A notice whose answer settled nothing would be
// sent again by every restart - a new epoch, a pause after a missed receipt - and logged again
// each time.
//
// The row restarts the relay with a registration of a new epoch once the notice is in, then
// stores a later message, and counts every notice and every notice line until that message
// arrives, so a second notice is reported as the count it is rather than stopping the row at the
// first frame it did not expect. The control is a done that settles nothing: the notice comes
// again and the count names it.
func TestAFrameTooLargeToPublishIsNoticedOnceAcrossARelayRestart(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	const sessionID = "ses_too_large_restart"
	agent := contracts.LegacyAgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	maxPayload := int(rig.harness.client.Conn.MaxPayload())

	_, bandSeq := storeSized(t, rig, agent, maxPayload)
	notice := noticeEventOf(t, rig, bandSeq)
	requireEventuallyDelivered(t, standIn, notice, listenerWaitBound(t))
	// The restart comes once the notice's answer has had its chance to settle the message; the
	// wait does not fail the row, so a done that settled nothing reaches the count below.
	settled := waitForAckFloor(t, rig.consumers, sessionID, bandSeq, 5*time.Second)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	after, _ := rig.storeEvent(t, agent)

	notices, laterArrived := 1, false
	deadline := time.After(listenerWaitBound(t))
collect:
	for !laterArrived {
		select {
		case frame := <-standIn.frames:
			switch frame.item.EventID {
			case notice.EventID:
				notices++
			case after.EventID:
				laterArrived = true
			default:
				t.Fatalf("the session received %s on %s, which is neither the notice of sequence %d nor the later message", frame.item.EventID, frame.item.Topic, bandSeq)
			}
		case <-deadline:
			break collect
		}
	}
	lines := len(rig.logLines(t, relayTooLargeLine, sessionID))
	if notices != 1 || lines != 1 {
		t.Errorf("across the restart the session received %d notices of sequence %d and the listener wrote %d lines %q, want one of each (the message was settled before the restart: %v)",
			notices, bandSeq, lines, relayTooLargeLine, settled)
	}
	if !laterArrived {
		t.Errorf("the later message %s did not reach the session within %s of the restart", after.EventID, listenerWaitBound(t))
	}
	if !settled {
		t.Errorf("the consumer's ack floor did not pass sequence %d within 5 s of the notice: the session's done settled nothing", bandSeq)
	}
}

// The notice of a frame too large to publish fits its own frame whatever the message it stands for
// holds. The row's message is the worst case: it takes the server's max payload to the byte, its
// topic is as long as NATS takes a subject (bus.MaxSubjectBytes), its payload names a repo of
// nearly a megabyte, and its event id, its sender's session id and its summary are each
// kilobytes long. Before the notice was built to fit, that repo went into the notice whole, the
// notice was itself too large to publish, and the relay restarted at the ack floor on the same
// message. The row asks that the notice still reaches the session within the max payload, still
// names the message by its stream sequence, its size and its topic whole, says which fields it
// cut, and that the later message arrives behind it. A role mark is no part of it: a frame with
// one is read against the role's claim before the relay publishes it (takeOverFrame), and the next
// row holds the notice's cut of one.
func TestTheNoticeOfAFrameTooLargeToPublishFitsWhateverTheMessageHolds(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	const sessionID = "ses_too_large_worst"
	agent := contracts.LegacyAgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	maxPayload := int(rig.harness.client.Conn.MaxPayload())

	long := strings.Repeat("x", 4000)
	item := listenerTestEnvelope("notifications.team."+strings.Repeat("t", bus.MaxSubjectBytes-len("notifications.team.")), "relay-too-large-worst")
	item.EventID, item.SourceSession, item.PayloadSummary = long, long, long
	fixed := len(marshalListenerEnvelope(t, item))
	item.Payload = `{"repo":"owner/` + strings.Repeat("r", maxPayload-fixed-len(`,"payload":"{\"repo\":\"owner/\"}"`)) + `"}`
	data := marshalListenerEnvelope(t, item)
	if len(data) != maxPayload || len(item.Topic) != bus.MaxSubjectBytes {
		t.Fatalf("UNKNOWN: the message takes %d bytes on a topic of %d, want the max payload of %d on a topic of %d", len(data), len(item.Topic), maxPayload, bus.MaxSubjectBytes)
	}
	ack, err := rig.harness.client.JS().Publish(agent, data)
	if err != nil {
		t.Fatalf("store the worst-case message: %v", err)
	}
	after, _ := rig.storeEvent(t, agent)

	notice := requireEventuallyDelivered(t, standIn, noticeEventOf(t, rig, ack.Sequence), listenerWaitBound(t))
	if size := relayFrameBytes(notice.msg); size > maxPayload {
		t.Fatalf("the notice's frame takes %d bytes against the max payload of %d", size, maxPayload)
	}
	var payload relayTooLargePayload
	if err := json.Unmarshal([]byte(notice.item.Payload), &payload); err != nil {
		t.Fatalf("decode the notice's payload: %v", err)
	}
	if payload.StreamSeq != ack.Sequence || payload.Size != maxPayload || payload.OriginalTopic != item.Topic || notice.item.Topic != item.Topic {
		t.Errorf("the notice names sequence %d, %d bytes, topic of %d bytes (envelope topic of %d); want sequence %d, %d bytes and the message's own topic of %d bytes",
			payload.StreamSeq, payload.Size, len(payload.OriginalTopic), len(notice.item.Topic), ack.Sequence, maxPayload, len(item.Topic))
	}
	if want := []string{"event_id", "source_session", "payload_summary", "repo"}; !slices.Equal(payload.Cut, want) {
		t.Errorf("the notice says it cut %v, want %v", payload.Cut, want)
	}
	if payload.Repo != "" || utf8.RuneCountInString(payload.EventID) != relayNoticeFieldRunes || !strings.HasSuffix(payload.EventID, "…") {
		t.Errorf("the notice carries repo %d bytes and an event id of %d runes, want no repo and the event id cut to %d runes with an ellipsis",
			len(payload.Repo), utf8.RuneCountInString(payload.EventID), relayNoticeFieldRunes)
	}
	requireEventuallyDelivered(t, standIn, after, listenerWaitBound(t))
	awaitAcked(t, rig.consumers, sessionID, ack.Sequence)
}

// A notice names each field it cut or left out, and none it did not. The rows build the notice of a
// message straight (relayTooLargeNotice): one whose texts are short and whose repo is a GitHub
// repository, which keeps every field whole and says it cut nothing; and one whose summary spans
// two lines, whose event id is past the field bound, whose repo names an owner past GitHub's own
// limit and whose role mark is past the field bound, which cuts the first two to one bounded line,
// leaves the repo and the role mark out and names exactly those four.
func TestATooLargeNoticeNamesEachFieldItCutAndNoOther(t *testing.T) {
	t.Parallel()
	const subject = "notifications.github.acme.widgets.push.branch.main"
	whole := listenerTestEnvelope(subject, "notice-cut-none")
	whole.Payload = `{"repo":"acme/widgets"}`
	shortened := listenerTestEnvelope(subject, "notice-cut-some")
	shortened.EventID = strings.Repeat("e", relayNoticeFieldRunes+1)
	shortened.PayloadSummary = "the first line\nthe second line"
	shortened.Payload = `{"repo":"` + strings.Repeat("o", githubOwnerRunes+1) + `/widgets"}`
	shortened.Role = strings.Repeat("r", relayNoticeFieldRunes+1)
	for _, row := range []struct {
		name string
		item contracts.Envelope
		cut  []string
		repo string
	}{
		{"a message held whole", whole, nil, "acme/widgets"},
		{"a message with four fields past the bounds", shortened, []string{"event_id", "payload_summary", "repo", "role"}, ""},
	} {
		data, payload, err := relayTooLargeNotice(bus.SessionMessage{StreamSeq: 7, Subject: subject, Data: marshalListenerEnvelope(t, row.item)}, "stream", 1<<20+1, 1<<20, 1<<20)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		if len(data) == 0 || !slices.Equal(payload.Cut, row.cut) || payload.Repo != row.repo {
			t.Errorf("%s: the notice says it cut %v and carries repo %q, want cut %v and repo %q", row.name, payload.Cut, payload.Repo, row.cut, row.repo)
		}
		if row.cut == nil && (payload.EventID != row.item.EventID || payload.PayloadSummary != row.item.PayloadSummary) {
			t.Errorf("%s: the notice carries event id %q and summary %q, want the message's own", row.name, payload.EventID, payload.PayloadSummary)
		}
		if row.cut != nil && (utf8.RuneCountInString(payload.EventID) != relayNoticeFieldRunes || payload.PayloadSummary != "the first line") {
			t.Errorf("%s: the notice carries an event id of %d runes and summary %q, want %d runes and the first line alone", row.name, utf8.RuneCountInString(payload.EventID), payload.PayloadSummary, relayNoticeFieldRunes)
		}
	}
}

// waitForAckFloor reports whether sessionID's consumer acknowledged everything through seq within
// bound, reading it every ackStatePoll, and fails nothing when it did not.
func waitForAckFloor(t *testing.T, consumers *bus.SessionConsumers, sessionID string, seq uint64, bound time.Duration) bool {
	t.Helper()
	for end := time.Now().Add(bound); time.Now().Before(end); time.Sleep(ackStatePoll) {
		if info, err := consumers.Info(context.Background(), sessionID); err == nil && info.AckFloor.Stream >= seq {
			return true
		}
	}
	return false
}

// noticeEventOf is the envelope a test waits for in place of the message stored at seq: only its
// event id is read, which the notice derives from the stream and the sequence
// (relayTooLargeNotice).
func noticeEventOf(t *testing.T, rig *relayRig, seq uint64) contracts.Envelope {
	t.Helper()
	data, _, err := relayTooLargeNotice(bus.SessionMessage{StreamSeq: seq, Subject: "x", Data: []byte("{}")}, rig.streamID(t), 0, 0, 1<<20)
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

// A role send to a live holder is held to the room its forward's relay frame leaves
// (bus.Client.RelayFrameTooLarge): the forward is the role message under the role forward's
// dedupe key, stored on the holder's agent subject, and its sender is still waiting for the answer.
// The largest role send whose forward leaves the relay's headers exactly their room is answered 200
// and reaches the holder whole; one byte more is the 413 naming the frame's size and the max
// payload, with nothing forwarded. The route's own size check of the role message passes both,
// since a role lane is no stored topic, so only the forward's check can tell them apart. A forward
// of a copy the stream already holds is held to the stream's bound alone: the kept lane's walk and
// the role lane's delivery hand such a copy on, whole or as the relay's notice.
func TestARoleSendWhoseForwardCouldNotBeRelayedWholeIs413(t *testing.T) {
	t.Parallel()
	rig := newRelayRig(t)
	// The publish route's role sends and POST /v1/roles/set need the delivery config main wires
	// for them; the relay keeps the rig's own.
	withPublishRoute(t, rig.deps)
	const holder, role = "ses_role_forward_bound", "forward-bound-holder"
	topic := contracts.RoleTopicPrefix + role
	standIn := startSessionStandIn(t, rig.url, holder, answerDone)
	rig.subscribe(t, consumerRegistration(holder, "epoch-1"))
	rig.post(t, "/v1/roles/set", `{"session_id":"`+holder+`","role":"`+role+`"}`)
	maxPayload := int(rig.harness.client.Conn.MaxPayload())
	largest := maxPayload - bus.RelayFrameHeaderBytes(contracts.LegacyAgentSubject(holder))

	// A publish under no key gets a minted one of a fixed width (publishHandler), so the forward's
	// size is the same for every text of one length.
	forwardSize := func(text string) int {
		item := contracts.MessageEnvelope(listenerMessage(listenerwire.MessageBody{Source: "agent", Message: text}, contracts.ExpectsReplyUnset), topic, publishDedupePrefix+id.New())
		item.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
		return len(marshalListenerEnvelope(t, item))
	}
	const base = 1000
	text := strings.Repeat("m", base+largest-forwardSize(strings.Repeat("m", base)))
	if got := forwardSize(text); got != largest {
		t.Fatalf("UNKNOWN: the forward of that role send takes %d bytes, want %d", got, largest)
	}
	publish := func(message string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"topic": topic, "message": message, "source": "agent"})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		rig.mux.ServeHTTP(recorder, postJSON("/v1/messages/publish", string(body)))
		return recorder
	}

	accepted := publish(text)
	if accepted.Code != http.StatusOK {
		t.Fatalf("a role send whose forward takes exactly the %d bytes its frame leaves: status %d, body %.300s", largest, accepted.Code, accepted.Body.String())
	}
	frame := standIn.collect(t, 1, listenerWaitBound(t))[0]
	if len(frame.msg.Data) != largest || frame.item.Topic != topic {
		t.Errorf("the holder was relayed %d bytes on %s, want the forward whole, %d bytes, on %s", len(frame.msg.Data), frame.item.Topic, largest, topic)
	}
	if size := relayFrameBytes(frame.msg); size > maxPayload {
		t.Errorf("its frame takes %d bytes against the server's max payload of %d", size, maxPayload)
	}

	refused := publish(text + "m")
	if refused.Code != http.StatusRequestEntityTooLarge || !strings.Contains(refused.Body.String(), "which reaches a session in a relay frame of") || !strings.Contains(refused.Body.String(), "max payload of 1048576 bytes") {
		t.Fatalf("a role send one byte past that: status %d, want 413 naming the relay frame and the server's max payload; body %.300s", refused.Code, refused.Body.String())
	}
	standIn.none(t, time.Second)
}

// The room the listener's checks reserve for a frame's headers (bus.RelayFrameHeaderBytes) is the
// relay's largest header block exactly: the gap with the longest reason the relay writes
// (relayGapReasons), the highest stream sequence and delivery count a uint64 holds, a stream
// identity of the width the bus writes (bus.StreamIDBytes) and a stored time in RFC 3339 with
// nanoseconds, whose longest is 30 bytes. A header the relay adds, or a value it writes longer,
// passes the reserve and fails this row; so does a reserve set larger than the relay needs, which
// would refuse messages every session could be handed whole. The control is each header the row
// writes, taken off: every one of them changes the block's size, so none of the rows is blind to
// a header the relay stops writing either.
func TestTheRelayFrameHeaderBoundIsTheRelaysLargestHeader(t *testing.T) {
	t.Parallel()
	longest := ""
	for _, reason := range relayGapReasons {
		if len(reason) > len(longest) {
			longest = reason
		}
	}
	storedAt := time.Date(2026, 11, 26, 20, 1, 14, 748147695, time.UTC)
	if width := len(storedAt.Format(time.RFC3339Nano)); width != 30 {
		t.Fatalf("UNKNOWN: the stored time takes %d bytes, want the 30 of nanoseconds with no zero trimmed", width)
	}
	for _, subject := range []string{
		contracts.LegacyAgentSubject("ses_header_bound"),
		"notifications.dispatch.issue.INFRA-5995.comment",
		"notifications.github.cybermonklabs.legion.pull_request.opened",
	} {
		largest := relayFrameHeader(subject, 1<<64-1, strings.Repeat("f", bus.StreamIDBytes), 1<<64-1, storedAt, true, longest)
		header := relayFrameBytes(&natsgo.Msg{Header: largest})
		if reserved := bus.RelayFrameHeaderBytes(subject); header != reserved {
			t.Errorf("the relay's largest header for %s takes %d bytes and the listener reserves %d: want them equal", subject, header, reserved)
		}
		for name := range largest {
			fewer := maps.Clone(largest)
			delete(fewer, name)
			if relayFrameBytes(&natsgo.Msg{Header: fewer}) >= header {
				t.Errorf("control: the header %s for %s adds nothing to the block, so the row cannot see it", name, subject)
			}
		}
	}
}
