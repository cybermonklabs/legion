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
	"unicode/utf8"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// relayTooLargeLine is the line the relay writes each time it sends a session a notice in place of
// a frame too large to publish (sessionRelays.publishTooLargeNotice).
const relayTooLargeLine = "session relay sent a notice in place of a frame too large to publish"

// relayTooLargeReason is the reason a too-large notice's payload names (relayTooLargePayload).
const relayTooLargeReason = "too_large"

// relayNoticeFieldRunes is the most runes a notice copies of any one text of the message it stands
// for (noticeField): the 160 runes of a summary line (contracts.OneLineSummary).
const relayNoticeFieldRunes = 160

// githubOwnerRunes and githubNameRunes are the longest owner and repository name GitHub allows, the
// longest repo a notice copies (noticeRepo).
const (
	githubOwnerRunes = 39
	githubNameRunes  = 100
)

// errRelayNoticeTooLarge is the failure to build a notice that fits the room its frame leaves under
// the server's max payload, even with every text of the message left out (relayTooLargeNotice):
// only a server whose max payload is smaller than the notice's fixed fields, its subject and the
// relay's headers meets it.
var errRelayNoticeTooLarge = errors.New("the notice of a frame too large to publish does not fit its own frame")

// relayTooLargePayload is the payload of the notice a session is sent in place of a frame too large
// to publish (relayTooLargeNotice): which message it stands for - its event id, the topic it was
// sent on and its stream sequence - how large it is - its stored bytes, the frame they make with
// the relay's headers, and the server's max payload they are past - and who sent it, as far as
// the message says. repo is the repository a GitHub event names (eventRepository), for an edge
// listener's relay socket, which passes a frame on a GitHub subject only for a repository of the
// session's projects and reads it from the payload. cut names each field the notice cut or left
// out, by its name in this payload, so a reader can tell a value the message held whole from one
// the notice shortened; it is absent when the notice cut nothing.
type relayTooLargePayload struct {
	Reason         string   `json:"reason"`
	EventID        string   `json:"event_id,omitempty"`
	OriginalTopic  string   `json:"original_topic"`
	StreamSeq      uint64   `json:"stream_seq"`
	Size           int      `json:"size"`
	FrameSize      int64    `json:"frame_size"`
	MaxPayload     int64    `json:"max_payload"`
	Source         string   `json:"source,omitempty"`
	SourceSession  string   `json:"source_session,omitempty"`
	PayloadSummary string   `json:"payload_summary,omitempty"`
	Repo           string   `json:"repo,omitempty"`
	Cut            []string `json:"cut,omitempty"`
}

// publishTooLargeNotice publishes the notice of msg (relayTooLargeNotice) on frame, the frame the
// bus refused for its size with refusal, in place of its data, so it goes under the frame's own
// headers and reply subject (publishFrame), and writes one line naming the message. The frame's
// size and the max payload it was held to are the refusal's own (bus.MessageTooLargeError), so the
// notice and the line name the numbers the refusal was made on. The notice is built to fit the
// room those headers leave under that max payload. A refusal that names no such numbers, a notice
// that cannot fit (errRelayNoticeTooLarge) and a notice that cannot be published are the frame's
// failure, which the relay answers as every failed publish: a restart at the ack floor.
func (m *sessionRelays) publishTooLargeNotice(ctx context.Context, sessionID string, frame *nats.Msg, msg bus.SessionMessage, streamID string, refusal error) error {
	var tooLarge *bus.MessageTooLargeError
	if !errors.As(refusal, &tooLarge) {
		return refusal
	}
	// What the server's max payload holds a message to is its data and its headers, never its
	// subject or reply; the notice goes under the frame's own headers.
	room := tooLarge.MaxPayload - int64((&nats.Msg{Header: frame.Header}).Size())
	notice, payload, err := relayTooLargeNotice(msg, streamID, tooLarge.Size, tooLarge.MaxPayload, room)
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
		slog.Int64("frame_size", payload.FrameSize),
		slog.Int64("max_payload", payload.MaxPayload),
		slog.Any("cut", payload.Cut),
		slog.String("error", refusal.Error()))
	return nil
}

// relayTooLargeNotice is the envelope, encoded, that the relay sends a session in place of msg, a
// message stored at stream sequence msg.StreamSeq of stream streamID whose frame of frameSize bytes
// is past maxPayload, with the payload it carries (relayTooLargePayload). room is the most bytes
// the notice may take: the max payload less the frame's own headers, which it goes under.
//
// It is the listener's own (source envoy), and it stands for the message where the session's
// plugin and an edge listener's relay socket read a frame: the frame's headers are the message's
// own (publishFrame), its topic is the message's, and so is its role mark, so the plugin shows it
// under the topic the message was sent on and an edge socket judges it as it would the message.
// Its event id and dedupe key are drawn from the stream and the sequence, so every notice of one
// message is one event to the plugin's ledger, and a notice sent again after a restart at the ack
// floor is answered done there without being shown twice.
//
// The notice fits by construction. The stored message is read only here and can hold any bytes a
// publisher gave it, so each text the notice copies from it is cut to one line of
// relayNoticeFieldRunes (noticeField), a repo past GitHub's own limits is left out (noticeRepo), and
// a topic past what NATS takes as a subject (bus.MaxSubjectBytes), which no publish of the bus made,
// gives way to the subject the stream stored the message on; each field cut or left out is named
// in the payload's cut. The rest is fixed in size, so the notice is a few kilobytes at most. It is
// then measured against room, and a notice past it is built again with every text of the message
// left out, each named in cut; a bare notice still past room, which only a server with a max
// payload of a few kilobytes makes, is errRelayNoticeTooLarge, and nothing is published.
func relayTooLargeNotice(msg bus.SessionMessage, streamID string, frameSize, maxPayload, room int64) ([]byte, relayTooLargePayload, error) {
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
	sum := sha256.Sum256([]byte(relayTooLargeReason + "\x00" + streamID + "\x00" + seq))
	noticeID := hex.EncodeToString(sum[:16])
	var cut []string
	field := func(name, text string) string {
		value, shortened := noticeField(text)
		if shortened {
			cut = append(cut, name)
		}
		return value
	}
	payload := relayTooLargePayload{
		Reason:         relayTooLargeReason,
		EventID:        field("event_id", original.EventID),
		OriginalTopic:  topic,
		StreamSeq:      msg.StreamSeq,
		Size:           len(msg.Data),
		FrameSize:      frameSize,
		MaxPayload:     maxPayload,
		Source:         field("source", original.Source),
		SourceSession:  field("source_session", original.SourceSession),
		PayloadSummary: field("payload_summary", original.PayloadSummary),
	}
	repo, repoLeftOut := noticeRepo(msg.Data)
	payload.Repo = repo
	if repoLeftOut {
		cut = append(cut, "repo")
	}
	// A role mark cut short would name another role, which an edge socket judges the notice by, so
	// one past the bound is left out whole, as a repo is.
	role, roleCut := noticeField(original.Role)
	if roleCut {
		role = ""
		cut = append(cut, "role")
	}
	trace := field("trace_id", original.TraceID)
	payload.Cut = cut
	build := func(payload relayTooLargePayload, role string) ([]byte, error) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode the too-large notice: %w", err)
		}
		named := payload.EventID
		if named == "" {
			named = "the message at stream sequence " + seq
		}
		shownTopic, _ := noticeField(payload.OriginalTopic)
		notice := contracts.Envelope{
			EventID:        noticeID,
			Source:         "envoy",
			SourceEventID:  cmp.Or(payload.EventID, noticeID),
			Topic:          payload.OriginalTopic,
			DedupeKey:      "envoy." + relayTooLargeReason + "." + noticeID,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: fmt.Sprintf("a message too large to deliver: %s on %s, %d bytes", named, shownTopic, payload.Size),
			Payload:        string(encoded),
			TraceID:        cmp.Or(trace, noticeID),
			Role:           role,
		}
		if err := notice.Validate(); err != nil {
			return nil, fmt.Errorf("validate the too-large notice: %w", err)
		}
		data, err := json.Marshal(notice)
		if err != nil {
			return nil, fmt.Errorf("encode the too-large notice: %w", err)
		}
		return data, nil
	}
	data, err := build(payload, role)
	if err != nil {
		return nil, payload, err
	}
	if int64(len(data)) <= room {
		return data, payload, nil
	}
	// The bare notice: every text of the message left out, the topic the stored subject, which
	// the frame's own Envoy-Subject header already carries, and the numbers kept.
	bare := relayTooLargePayload{
		Reason: relayTooLargeReason, OriginalTopic: msg.Subject, StreamSeq: msg.StreamSeq,
		Size: len(msg.Data), FrameSize: frameSize, MaxPayload: maxPayload,
		Cut: []string{"event_id", "original_topic", "source", "source_session", "payload_summary", "repo", "role"},
	}
	data, err = build(bare, "")
	if err != nil {
		return nil, bare, err
	}
	if int64(len(data)) > room {
		return nil, bare, fmt.Errorf("%w: the bare notice takes %d bytes and its frame leaves %d", errRelayNoticeTooLarge, len(data), room)
	}
	return data, bare, nil
}

// noticeField is text as a notice carries it, and whether the notice shortened it: its first
// non-empty line cut to relayNoticeFieldRunes with an ellipsis (contracts.OneLineSummary), and ""
// for one that holds no such line. The message the notice stands for is read nowhere else, so
// nothing else bounds what its fields hold.
func noticeField(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	value := contracts.OneLineSummary(trimmed)
	return value, value != trimmed
}

// noticeRepo is the repository a GitHub event stored as data names (eventRepository), as the
// notice carries it, and whether the notice left one out: a repo whose owner or name is past
// GitHub's own limits names no GitHub repository, and cutting it would name another one, so it is
// left out whole.
func noticeRepo(data []byte) (string, bool) {
	owner, name, named := eventRepository(data)
	if !named {
		return "", false
	}
	if utf8.RuneCountInString(owner) > githubOwnerRunes || utf8.RuneCountInString(name) > githubNameRunes {
		return "", true
	}
	return owner + "/" + name, false
}
