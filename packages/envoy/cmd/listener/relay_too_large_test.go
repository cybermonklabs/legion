package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
// exactly size bytes (bandText).
func sendTextOfSize(t *testing.T, sessionID string, size int) string {
	t.Helper()
	return bandText(t, func(text string) int {
		item := contracts.AgentSendEnvelope(sessionID, "", listenerMessage(listenerwire.MessageBody{Source: "agent", Message: text}, contracts.ExpectsReplyUnset))
		return len(marshalListenerEnvelope(t, item))
	}, size)
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
// at all (INFRA-5995).
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
// restarts at the ack floor in 60 s and not one later frame (INFRA-5995).
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
// again and the count names it. The wait for the notice's answer to settle the message is bounded
// by the test binary's deadline (listenerWaitBound) rather than a fixed span, which a loaded
// machine outruns without anything being wrong (awaitAckState); it fails nothing itself, so that
// control still reaches the count.
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
	settleBound := listenerWaitBound(t)
	settled := waitForAckFloor(t, rig.consumers, sessionID, bandSeq, settleBound)
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
		t.Errorf("the consumer's ack floor did not pass sequence %d within %s of the notice: the session's done settled nothing", bandSeq, settleBound)
	}
}

// The notice of a frame too large to publish fits its own frame whatever the message it stands for
// holds. The row's message is the worst case: it takes the server's max payload to the byte, its
// topic is as long as NATS takes a subject (contracts.SubjectMaxBytes), its payload names a repo of
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
	item := listenerTestEnvelope("notifications.team."+strings.Repeat("t", contracts.SubjectMaxBytes-len("notifications.team.")), "relay-too-large-worst")
	item.EventID, item.SourceSession, item.PayloadSummary = long, long, long
	fixed := len(marshalListenerEnvelope(t, item))
	item.Payload = `{"repo":"owner/` + strings.Repeat("r", maxPayload-fixed-len(`,"payload":"{\"repo\":\"owner/\"}"`)) + `"}`
	data := marshalListenerEnvelope(t, item)
	if len(data) != maxPayload || len(item.Topic) != contracts.SubjectMaxBytes {
		t.Fatalf("UNKNOWN: the message takes %d bytes on a topic of %d, want the max payload of %d on a topic of %d", len(data), len(item.Topic), maxPayload, contracts.SubjectMaxBytes)
	}
	ack, err := rig.harness.client.JS().Publish(agent, data)
	if err != nil {
		t.Fatalf("store the worst-case message: %v", err)
	}
	after, _ := rig.storeEvent(t, agent)

	notice := requireEventuallyDelivered(t, standIn, noticeEventOf(t, rig, ack.Sequence), listenerWaitBound(t))
	if size := bus.MessageBytes(notice.msg); size > maxPayload {
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

// A notice names in its cut each text of the message it does not carry as the message holds it,
// and no other (noticeTexts.cut). Each row builds the notice of a message straight
// (relayTooLargeNotice) and judges its cut against the notice it returned, read back from the
// notice's own encoding (wantNoticeCut): each text the message holds against the notice's value of
// it, the payload's for the six the payload carries and the envelope's for the role mark and the
// trace id. The cut must name exactly the texts that differ, in that order, each by the JSON name
// of a field the payload or the envelope carries, so a cut that misses a change, names a text the
// notice carries whole or one the message never held, or names a field neither carries, fails it.
//
// The messages reach every way a notice changes a text: a message held whole, which cuts nothing;
// one whose summary spans two lines, whose event id and role mark are past the field bound and
// whose repo names an owner past GitHub's own limit; one whose topic is past what NATS takes as a
// subject, which gives way to the stored subject; one whose texts carry spaces around them, which
// the notice trims; one whose trace id is past the field bound, a text the notice's envelope
// carries; one whose payload names its repo under two keys, which no notice carries; and, under a
// room one byte smaller than the first build, the bare notices of two messages, one stored on its
// own topic and one stored away from it, which must name exactly the texts each message held and
// the bare notice left out - the topic only where the stored subject is not the message's own. Each
// row also names the texts it stands on, the ones its message makes a bounded notice carry
// otherwise, so a notice that stops bounding a text, or a row whose message no longer reaches its
// case, fails rather than passing on an empty difference.
func TestATooLargeNoticeNamesEachFieldItCutAndNoOther(t *testing.T) {
	t.Parallel()
	const github = "notifications.github.acme.widgets.push.branch.main"
	agent := contracts.LegacyAgentSubject("ses_notice_cut")
	message := func(topic string, change func(*contracts.Envelope)) contracts.Envelope {
		item := listenerTestEnvelope(topic, "notice-cut")
		change(&item)
		return item
	}
	longOwner := strings.Repeat("o", githubOwnerRunes+1)
	payloadNames, envelopeNames := jsonFieldNames(relayTooLargePayload{}), jsonFieldNames(contracts.Envelope{})
	for _, row := range []struct {
		name string
		// stored is the subject the stream stored the message on.
		stored string
		item   contracts.Envelope
		// repo is the repository the message names, owner/name, and unread a message that names
		// one in a way readers disagree on.
		repo   string
		unread bool
		// bare builds the notice under a room one byte smaller than its first build.
		bare bool
		// stands is the cut the row's message makes the notice carry differently.
		stands []string
	}{
		{"a message held whole", github, message(github, func(item *contracts.Envelope) {
			item.Payload = `{"repo":"acme/widgets"}`
		}), "acme/widgets", false, false, nil},
		{"a message with four texts past the bounds", github, message(github, func(item *contracts.Envelope) {
			item.EventID = strings.Repeat("e", relayNoticeFieldRunes+1)
			item.PayloadSummary = "the first line\nthe second line"
			item.Payload = `{"repo":"` + longOwner + `/widgets"}`
			item.Role = strings.Repeat("r", relayNoticeFieldRunes+1)
		}), longOwner + "/widgets", false, false, []string{"event_id", "payload_summary", "repo", "role"}},
		{"a message whose topic is past what NATS takes as a subject", agent, message("notifications.team."+strings.Repeat("t", contracts.SubjectMaxBytes), func(*contracts.Envelope) {}),
			"", false, false, []string{"original_topic"}},
		{"a message whose texts carry spaces around them", github, message(github, func(item *contracts.Envelope) {
			item.Source, item.SourceSession = " agent", "ses_spaced\n"
		}), "", false, false, []string{"source", "source_session"}},
		{"a message whose trace id is past the field bound", github, message(github, func(item *contracts.Envelope) {
			item.TraceID = strings.Repeat("x", relayNoticeFieldRunes+1)
		}), "", false, false, []string{"trace_id"}},
		{"a message that names its repo under two keys", github, message(github, func(item *contracts.Envelope) {
			item.Payload = `{"repo":"acme/widgets","repo":"acme/other"}`
		}), "", true, false, []string{"repo"}},
		{"the bare notice of a message stored on its own topic", agent, message(agent, func(item *contracts.Envelope) {
			item.PayloadSummary = strings.Repeat("s", 150)
		}), "", false, true, []string{"event_id", "source", "payload_summary", "trace_id"}},
		{"the bare notice of a message stored away from its topic", agent, message(github, func(item *contracts.Envelope) {
			item.Payload = `{"repo":"acme/widgets"}`
		}), "acme/widgets", false, true, []string{"event_id", "original_topic", "source", "payload_summary", "repo", "trace_id"}},
	} {
		msg := bus.SessionMessage{StreamSeq: 7, Subject: row.stored, Data: marshalListenerEnvelope(t, row.item)}
		room := int64(1 << 20)
		if row.bare {
			first, _, err := relayTooLargeNotice(msg, "stream", 1<<20+1, 1<<20, room)
			if err != nil {
				t.Fatalf("%s: the first build: %v", row.name, err)
			}
			room = int64(len(first)) - 1
		}
		data, returned, err := relayTooLargeNotice(msg, "stream", 1<<20+1, 1<<20, room)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		notice, payload := decodeNotice(t, data)
		if row.bare && (payload.EventID != "" || payload.OriginalTopic != row.stored) {
			t.Errorf("%s: the notice built under a room of %d bytes, one byte smaller than its first build, carries event id %q on %q: it is not the bare notice", row.name, room, payload.EventID, payload.OriginalTopic)
		}
		want := wantNoticeCut(row.item, row.repo, row.unread, notice, payload)
		if !slices.Equal(payload.Cut, want) || !slices.Equal(returned.Cut, payload.Cut) {
			t.Errorf("%s: the notice says it cut %v (returned %v), want %v: the texts the notice's own fields carry otherwise than the message", row.name, payload.Cut, returned.Cut, want)
		}
		for _, name := range payload.Cut {
			if !payloadNames[name] && !envelopeNames[name] {
				t.Errorf("%s: the notice's cut names %q, a field neither its payload nor its envelope carries", row.name, name)
			}
		}
		if !slices.Equal(want, row.stands) {
			t.Errorf("%s: the notice carries %v otherwise than the message, want %v: it no longer bounds those texts, or the row's message no longer reaches its case", row.name, want, row.stands)
		}
	}
}

// A notice past the room its frame leaves is built again bare, and a bare notice still past that
// room is errRelayNoticeTooLarge. The rows build the notice of one message straight
// (relayTooLargeNotice). Under a room one byte smaller than its first build the notice is the bare
// one, within that room: the reason, the stream sequence, the size, the frame's size and the max
// payload kept, the topic the stored subject, the notice's own event id the full notice's, and no
// text of the message, its trace id and its role mark among them. Under a room one byte smaller
// than the bare notice nothing is built, and the error is errRelayNoticeTooLarge, which the relay
// answers as a failed publish. The control is the same message under the server's max payload,
// whose notice carries its texts, so a message with none could not pass the bare row.
func TestATooLargeNoticePastItsRoomIsBuiltBareAndABareOnePastItIsRefused(t *testing.T) {
	t.Parallel()
	agent := contracts.LegacyAgentSubject("ses_notice_bare")
	item := listenerTestEnvelope("notifications.team.bare", "notice-bare")
	item.SourceSession, item.Role, item.Payload = "ses_bare_sender", "bare-role", `{"repo":"acme/widgets"}`
	msg := bus.SessionMessage{StreamSeq: 9, Subject: agent, Data: marshalListenerEnvelope(t, item)}
	build := func(room int64) ([]byte, error) {
		data, _, err := relayTooLargeNotice(msg, "stream", 1<<20+1, 1<<20, room)
		return data, err
	}

	whole, err := build(1 << 20)
	if err != nil {
		t.Fatalf("the notice under the max payload: %v", err)
	}
	fullNotice, full := decodeNotice(t, whole)
	if full.EventID != item.EventID || full.SourceSession != item.SourceSession || full.Repo != "acme/widgets" || fullNotice.Role != item.Role || fullNotice.TraceID != item.TraceID {
		t.Fatalf("control: the notice under the max payload carries event id %q, source session %q, repo %q, role %q and trace id %q, want the message's own",
			full.EventID, full.SourceSession, full.Repo, fullNotice.Role, fullNotice.TraceID)
	}

	room := int64(len(whole)) - 1
	bare, err := build(room)
	if err != nil {
		t.Fatalf("the notice under a room one byte smaller than its first build (%d bytes): %v", room, err)
	}
	notice, payload := decodeNotice(t, bare)
	if int64(len(bare)) > room {
		t.Errorf("the bare notice takes %d bytes against a room of %d", len(bare), room)
	}
	if payload.Reason != relayTooLargeReason || payload.StreamSeq != 9 || payload.Size != len(msg.Data) || payload.FrameSize != 1<<20+1 || payload.MaxPayload != 1<<20 ||
		payload.OriginalTopic != agent || notice.Topic != agent || notice.Source != "envoy" || notice.EventID != fullNotice.EventID {
		t.Errorf("the bare notice is %s's %s on %s saying %+v, want envoy's %s on the stored subject %s: reason %s, sequence 9, %d bytes, a frame of 1048577 and a max payload of 1048576",
			notice.Source, notice.EventID, notice.Topic, payload, fullNotice.EventID, agent, relayTooLargeReason, len(msg.Data))
	}
	if payload.EventID != "" || payload.Source != "" || payload.SourceSession != "" || payload.PayloadSummary != "" || payload.Repo != "" || notice.Role != "" || notice.TraceID != notice.EventID {
		t.Errorf("the bare notice carries event id %q, source %q, source session %q, summary %q, repo %q, role %q and trace id %q, want no text of the message, its trace id the notice's own %s",
			payload.EventID, payload.Source, payload.SourceSession, payload.PayloadSummary, payload.Repo, notice.Role, notice.TraceID, notice.EventID)
	}

	refused, err := build(int64(len(bare)) - 1)
	if !errors.Is(err, errRelayNoticeTooLarge) || refused != nil {
		t.Errorf("the notice under a room one byte smaller than the bare notice: %d bytes and %v, want nothing and errRelayNoticeTooLarge", len(refused), err)
	}
}

// decodeNotice is the notice data encodes (relayTooLargeNotice): its envelope and the payload it
// carries.
func decodeNotice(t *testing.T, data []byte) (contracts.Envelope, relayTooLargePayload) {
	t.Helper()
	var notice contracts.Envelope
	if err := json.Unmarshal(data, &notice); err != nil {
		t.Fatalf("decode the notice %.300s: %v", data, err)
	}
	var payload relayTooLargePayload
	if err := json.Unmarshal([]byte(notice.Payload), &payload); err != nil {
		t.Fatalf("decode the notice's payload %.300s: %v", notice.Payload, err)
	}
	return notice, payload
}

// wantNoticeCut is the cut the notice of message must carry, judged from the notice itself: the
// name of each text message holds whose value in the notice differs, the payload's value for the
// six the payload carries and the envelope's for the role mark and the trace id, in that order.
// repo is the repository message names, owner/name, and repoUnread a message that names one in a
// way readers disagree on, which no notice carries.
func wantNoticeCut(message contracts.Envelope, repo string, repoUnread bool, notice contracts.Envelope, payload relayTooLargePayload) []string {
	var want []string
	for _, text := range []struct {
		name, held, carried string
		unread              bool
	}{
		{"event_id", message.EventID, payload.EventID, false},
		{"original_topic", message.Topic, payload.OriginalTopic, false},
		{"source", message.Source, payload.Source, false},
		{"source_session", message.SourceSession, payload.SourceSession, false},
		{"payload_summary", message.PayloadSummary, payload.PayloadSummary, false},
		{"repo", repo, payload.Repo, repoUnread},
		{"role", message.Role, notice.Role, false},
		{"trace_id", message.TraceID, notice.TraceID, false},
	} {
		if text.unread || (text.held != "" && text.carried != text.held) {
			want = append(want, text.name)
		}
	}
	return want
}

// jsonFieldNames is the JSON name of each field of v's struct type, as its json tag gives it.
func jsonFieldNames(v any) map[string]bool {
	names := map[string]bool{}
	typ := reflect.TypeOf(v)
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		names[name] = true
	}
	return names
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

// requireRelayedWhole fails the test unless frame carries, byte for byte, the message the stream
// stored and the request sent: its data must be the stored copy at the subject and stream sequence
// the frame names (relayHeaderSubject, relayHeaderStreamSeq), read back from the stream with
// client, and that copy's payload must be text whole, the message the request carried. A frame of
// the right length whose bytes differ fails it, as one cut short does.
func requireRelayedWhole(t *testing.T, client *bus.Client, frame relayedFrame, text string) {
	t.Helper()
	seq, err := strconv.ParseUint(frame.msg.Header.Get(relayHeaderStreamSeq), 10, 64)
	if err != nil {
		t.Fatalf("the frame names stream sequence %q: %v", frame.msg.Header.Get(relayHeaderStreamSeq), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), listenerWaitBound(t))
	defer cancel()
	stored, err := client.NextStoredMessage(ctx, frame.msg.Header.Get(relayHeaderSubject), seq)
	if err != nil || stored.Sequence != seq {
		t.Fatalf("read the stored copy at stream sequence %d: sequence %d, %v", seq, stored.Sequence, err)
	}
	if !bytes.Equal(frame.msg.Data, stored.Data) {
		at := 0
		for at < len(frame.msg.Data) && at < len(stored.Data) && frame.msg.Data[at] == stored.Data[at] {
			at++
		}
		t.Errorf("the session was relayed %d bytes that differ from the %d the stream stored at sequence %d, first at byte %d: the message did not reach it whole",
			len(frame.msg.Data), len(stored.Data), seq, at)
	}
	var kept contracts.Envelope
	if err := json.Unmarshal(stored.Data, &kept); err != nil || kept.Payload != text {
		t.Errorf("the stored copy at sequence %d carries a payload of %d bytes (%v), want the %d bytes of the message the request sent, whole", seq, len(kept.Payload), err, len(text))
	}
}

// A message the send route accepts for a session reaches that session whole: the route's size
// check reserves the bytes the relay's headers add to the frame (bus.RelayFrameHeaderBytes), so
// the largest message it accepts is relayed, byte for byte the message the stream stored and the
// request sent (requireRelayedWhole), and one byte more is answered 413 naming the max payload.
// Without the reserve the route accepted the larger one, the stream stored it, and no frame of it
// could ever be published.
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
	if size := bus.MessageBytes(frame.msg); size > maxPayload {
		t.Errorf("its frame takes %d bytes against the server's max payload of %d", size, maxPayload)
	}
	requireRelayedWhole(t, rig.harness.client, frame, text)

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
// and reaches the holder whole, byte for byte the forward the stream stored (requireRelayedWhole);
// one byte more is the 413 naming the frame's size and the max payload, with nothing forwarded. The
// route's own size check of the role message passes both, since a role lane is no stored topic, so
// only the forward's check can tell them apart. The forwards no sender waits on are held to the
// stream's bound alone - the kept lane's walk, which hands on a copy the stream already holds, and
// the role lane's delivery of a role message it took from its core-NATS subscription - and their
// holder is handed them whole or as the relay's notice.
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
	text := bandText(t, func(text string) int {
		item := contracts.MessageEnvelope(listenerMessage(listenerwire.MessageBody{Source: "agent", Message: text}, contracts.ExpectsReplyUnset), topic, publishDedupePrefix+id.New())
		item.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
		return len(marshalListenerEnvelope(t, item))
	}, largest)
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
	if size := bus.MessageBytes(frame.msg); size > maxPayload {
		t.Errorf("its frame takes %d bytes against the server's max payload of %d", size, maxPayload)
	}
	requireRelayedWhole(t, rig.harness.client, frame, text)

	refused := publish(text + "m")
	if refused.Code != http.StatusRequestEntityTooLarge || !strings.Contains(refused.Body.String(), "which reaches a session in a relay frame of") || !strings.Contains(refused.Body.String(), "max payload of 1048576 bytes") {
		t.Fatalf("a role send one byte past that: status %d, want 413 naming the relay frame and the server's max payload; body %.300s", refused.Code, refused.Body.String())
	}
	standIn.none(t, time.Second)
}

// The room the listener's checks reserve for a frame's headers (bus.RelayFrameHeaderBytes) is the
// relay's largest header block exactly: the gap with the longest reason the relay can write, over
// every relayGapReason below relayGapReasonCount, which are all relayFrameHeader takes; the highest
// stream sequence and delivery count a uint64 holds; a stream identity of the width the bus writes
// (bus.StreamIDBytes); and a stored time in RFC 3339 with nanoseconds, whose longest is 30 bytes.
// A header the relay adds, stops writing, or writes longer or shorter moves the block off the
// reserve and fails this row; so does a reserve set larger than the relay needs, which would refuse
// messages every session could be handed whole, and so does a gap reason with no name.
//
// The control is each value the row passes at its largest given at its smallest instead: the block
// must shrink by exactly the bytes that value loses in the header it is written to, so the row's
// largest values are what the relay writes into the block. A header the relay writes from anything
// but the value it is handed, or stops writing, fails it.
func TestTheRelayFrameHeaderBoundIsTheRelaysLargestHeader(t *testing.T) {
	t.Parallel()
	longest := relayGapRetention
	for reason := range relayGapReasonCount {
		if reason != relayGapRetention && relayGapReasonNames[reason] == "" {
			t.Errorf("the gap reason %d has no name, so a frame marking its gap says it is the stream's retention gap", reason)
		}
		if len(relayGapReasonNames[reason]) > len(relayGapReasonNames[longest]) {
			longest = reason
		}
	}
	storedAt := time.Date(2026, 11, 26, 20, 1, 14, 748147695, time.UTC)
	if width := len(storedAt.Format(time.RFC3339Nano)); width != 30 {
		t.Fatalf("UNKNOWN: the stored time takes %d bytes, want the 30 of nanoseconds with no zero trimmed", width)
	}
	const most = 1<<64 - 1
	streamID := strings.Repeat("f", bus.StreamIDBytes)
	reasonLine := len(relayHeaderGapReason+": \r\n") + len(relayGapReasonNames[longest])
	for _, subject := range []string{
		contracts.LegacyAgentSubject("ses_header_bound"),
		"notifications.dispatch.issue.INFRA-5995.comment",
		"notifications.github.cybermonklabs.legion.pull_request.opened",
	} {
		header := bus.MessageBytes(&natsgo.Msg{Header: relayFrameHeader(subject, most, streamID, most, storedAt, true, longest)})
		if reserved := bus.RelayFrameHeaderBytes(subject); header != reserved {
			t.Errorf("the relay's largest header for %s takes %d bytes and the listener reserves %d: want them equal", subject, header, reserved)
		}
		for _, smaller := range []struct {
			name  string
			block natsgo.Header
			loses int
		}{
			{"a stream sequence of 1", relayFrameHeader(subject, 1, streamID, most, storedAt, true, longest), 19},
			{"a delivery count of 1", relayFrameHeader(subject, most, streamID, 1, storedAt, true, longest), 19},
			{"a stream identity of one byte", relayFrameHeader(subject, most, "f", most, storedAt, true, longest), bus.StreamIDBytes - 1},
			{"a stored time on the second", relayFrameHeader(subject, most, streamID, most, storedAt.Truncate(time.Second), true, longest), len(".748147695")},
			{"the stream's retention gap", relayFrameHeader(subject, most, streamID, most, storedAt, true, relayGapRetention), reasonLine},
			{"no gap", relayFrameHeader(subject, most, streamID, most, storedAt, false, longest), len(relayHeaderGap+": true\r\n") + reasonLine},
		} {
			if lost := header - bus.MessageBytes(&natsgo.Msg{Header: smaller.block}); lost != smaller.loses {
				t.Errorf("control: %s for %s takes %d bytes off the block, want %d: the row's largest value there is not what the relay writes", smaller.name, subject, lost, smaller.loses)
			}
		}
	}
}
