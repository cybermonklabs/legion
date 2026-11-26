package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// relayTooLargeLine is the line the relay writes each time it sends a session a notice in place of
// a frame too large to publish (sessionRelays.publishTooLargeNotice).
const relayTooLargeLine = "session relay sent a notice in place of a frame too large to publish"

// relayTooLargeReason is the reason a too-large notice's payload names (relayTooLargePayload).
const relayTooLargeReason = "too_large"

// relayTooLargePayload is the payload of the notice a session is sent in place of a frame too large
// to publish (relayTooLargeNotice): which message it stands for - its event id, the topic it was
// sent on and its stream sequence - how large it is - its stored bytes, the frame they make with
// the relay's headers, and the server's max payload they are past - and who sent it, as far as
// the message says. repo is the repository a GitHub event names (eventRepository), for an edge
// listener's relay socket, which passes a frame on a GitHub subject only for a repository of the
// session's projects and reads it from the payload.
type relayTooLargePayload struct {
	Reason         string `json:"reason"`
	EventID        string `json:"event_id,omitempty"`
	OriginalTopic  string `json:"original_topic"`
	StreamSeq      uint64 `json:"stream_seq"`
	Size           int    `json:"size"`
	FrameSize      int    `json:"frame_size"`
	MaxPayload     int64  `json:"max_payload"`
	Source         string `json:"source,omitempty"`
	SourceSession  string `json:"source_session,omitempty"`
	PayloadSummary string `json:"payload_summary,omitempty"`
	Repo           string `json:"repo,omitempty"`
}

// publishTooLargeNotice publishes the notice of msg (relayTooLargeNotice) on frame, the frame the
// bus refused for its size with refusal, in place of its data, so it goes under the frame's own
// headers and reply subject (publishFrame), and writes one line naming the message. A notice that
// cannot be built or published is the frame's failure, which the relay answers as every failed
// publish: a restart at the ack floor.
func (m *sessionRelays) publishTooLargeNotice(ctx context.Context, sessionID string, frame *nats.Msg, msg bus.SessionMessage, streamID string, refusal error) error {
	// What the server's max payload holds a message to: its data and its headers, never its
	// subject or reply (bus.messageBytes).
	frameSize := frame.Size() - len(frame.Subject) - len(frame.Reply)
	maxPayload := m.delivery.consumers.MaxPayload()
	notice, payload, err := relayTooLargeNotice(msg, streamID, frameSize, maxPayload)
	if err != nil {
		return errors.Join(refusal, err)
	}
	frame.Data = notice
	if err := m.delivery.consumers.Publish(ctx, frame); err != nil {
		return errors.Join(refusal, fmt.Errorf("publish its notice: %w", err))
	}
	m.logger.Warn(relayTooLargeLine,
		slog.String("session_id", sessionID),
		slog.Uint64("stream_seq", msg.StreamSeq),
		slog.String("event_id", payload.EventID),
		slog.String("topic", payload.OriginalTopic),
		slog.Int("size", payload.Size),
		slog.Int("frame_size", payload.FrameSize),
		slog.Int64("max_payload", payload.MaxPayload),
		slog.String("error", refusal.Error()))
	return nil
}

// relayTooLargeNotice is the envelope, encoded, that the relay sends a session in place of msg, a
// message stored at stream sequence msg.StreamSeq of stream streamID whose frame of frameSize bytes
// is past maxPayload, with the payload it carries (relayTooLargePayload).
//
// It is the listener's own (source envoy), and it stands for the message where the session's
// plugin and an edge listener's relay socket read a frame: the frame's headers are the message's
// own (publishFrame), its topic is the message's, and so is its role mark, so the plugin shows it
// under the topic the message was sent on and an edge socket judges it as it would the message.
// Its event id and dedupe key are drawn from the stream and the sequence, so every notice of one
// message is one event to the plugin's ledger, and a notice sent again after a restart at the ack
// floor is answered done there without being shown twice.
//
// Every text it copies from the message is bounded, so the notice fits in a frame whatever the
// message holds: each is cut to one line of at most 160 runes (noticeField), since the stored
// message is only read here and can carry any bytes a publisher gave it, and a topic past what
// NATS takes as a subject (bus.MaxSubjectBytes), which no publish of the bus made, is left for
// the subject the stream stored it on.
func relayTooLargeNotice(msg bus.SessionMessage, streamID string, frameSize int, maxPayload int64) ([]byte, relayTooLargePayload, error) {
	// A message that does not decode, or decodes in part, still has its notice: the fields it
	// does not name are left out, and the notice names it by the subject and sequence it was
	// stored at.
	var original contracts.Envelope
	_ = json.Unmarshal(msg.Data, &original)
	topic := original.Topic
	if strings.TrimSpace(topic) == "" || len(topic) > bus.MaxSubjectBytes {
		topic = msg.Subject
	}
	seq := strconv.FormatUint(msg.StreamSeq, 10)
	payload := relayTooLargePayload{
		Reason:         relayTooLargeReason,
		EventID:        noticeField(original.EventID),
		OriginalTopic:  topic,
		StreamSeq:      msg.StreamSeq,
		Size:           len(msg.Data),
		FrameSize:      frameSize,
		MaxPayload:     maxPayload,
		Source:         noticeField(original.Source),
		SourceSession:  noticeField(original.SourceSession),
		PayloadSummary: noticeField(original.PayloadSummary),
	}
	// The repository a GitHub event names is its payload's repo (eventRepository), which an edge
	// listener's relay socket reads to judge such a frame. An edge relay withholds a frame whose
	// repo its own read cannot place before it publishes it (edgeWithholds), so the notice of one
	// it publishes carries the field as that read answered it.
	if owner, name, named := eventRepository(msg.Data); named {
		payload.Repo = owner + "/" + name
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, payload, fmt.Errorf("encode the too-large notice: %w", err)
	}
	named := payload.EventID
	if named == "" {
		named = "the message at stream sequence " + seq
	}
	sum := sha256.Sum256([]byte(relayTooLargeReason + "\x00" + streamID + "\x00" + seq))
	notice := contracts.Envelope{
		EventID:        hex.EncodeToString(sum[:16]),
		Source:         "envoy",
		SourceEventID:  cmp.Or(payload.EventID, streamID+":"+seq),
		Topic:          topic,
		DedupeKey:      "envoy." + relayTooLargeReason + "." + streamID + "." + seq,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: fmt.Sprintf("a message too large to deliver: %s on %s, %d bytes", named, topic, len(msg.Data)),
		Payload:        string(encoded),
		TraceID:        cmp.Or(noticeField(original.TraceID), id.New()),
		Role:           noticeField(original.Role),
	}
	if err := notice.Validate(); err != nil {
		return nil, payload, fmt.Errorf("validate the too-large notice: %w", err)
	}
	data, err := json.Marshal(notice)
	if err != nil {
		return nil, payload, fmt.Errorf("encode the too-large notice: %w", err)
	}
	return data, payload, nil
}

// noticeField is text as a notice carries it: its first non-empty line, at most 160 runes
// (contracts.OneLineSummary), and "" for one that holds no such line. The message the notice
// stands for is read nowhere else, so nothing else bounds what its fields hold.
func noticeField(text string) string {
	return contracts.OneLineSummary(strings.TrimSpace(text))
}
