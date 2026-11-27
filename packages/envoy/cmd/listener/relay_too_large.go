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
// session's projects and reads it from the payload.
//
// cut names each text of the message the notice does not carry as the message holds it - cut
// short, left out, or a topic replaced by the subject the stream stored the message on - so a
// reader can tell a value the message held whole from one the notice changed; it is absent when
// the notice carries every text the message holds whole. A text is named where the notice carries
// it: by its name in this payload for the six this payload carries (event_id, original_topic,
// source, source_session, payload_summary, repo), and by its name in the notice's own envelope
// (contracts.Envelope) for the role mark and the trace id, which the envelope carries (role,
// trace_id). noticeTexts.cut is the one rule that names them.
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
// size and the max payload it was held to are the refusal's own, so the notice and the line name
// the numbers the refusal was made on, and so is the room the notice may take: the refusal counted
// the frame's data and headers (bus.MessageBytes), so its size less the data is the headers the
// notice goes under. A notice that cannot fit (errRelayNoticeTooLarge) and a notice that cannot be
// published are the frame's failure, which the relay answers as every failed publish: a restart at
// the ack floor.
func (m *sessionRelays) publishTooLargeNotice(ctx context.Context, sessionID string, frame *nats.Msg, msg bus.SessionMessage, streamID string, refusal *bus.MessageTooLargeError) error {
	room := refusal.MaxPayload - (refusal.Size - int64(len(frame.Data)))
	notice, payload, err := relayTooLargeNotice(msg, streamID, refusal.Size, refusal.MaxPayload, room)
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
// publisher gave it, so each text the notice copies is bounded (noticeTexts.carried), and the rest
// is fixed in size, so the notice is a few kilobytes at most. It is then measured against room,
// and a notice past it is built again bare: every text of the message left out and the topic the
// stored subject. A bare notice still past room, which only a server with a max payload of a few
// kilobytes makes, is errRelayNoticeTooLarge, and nothing is published. Both notices are built
// from the same parts, and each names in its cut every text it does not carry as the message holds
// it (noticeTexts.cut).
func relayTooLargeNotice(msg bus.SessionMessage, streamID string, frameSize, maxPayload, room int64) ([]byte, relayTooLargePayload, error) {
	// A message that does not decode, or decodes in part, still has its notice: the fields it
	// does not name are left out, and the notice names it by the subject and sequence it was
	// stored at.
	var original contracts.Envelope
	_ = json.Unmarshal(msg.Data, &original)
	held := heldNoticeTexts(original, msg.Data)
	seq := strconv.FormatUint(msg.StreamSeq, 10)
	sum := sha256.Sum256([]byte(relayTooLargeReason + "\x00" + streamID + "\x00" + seq))
	noticeID := hex.EncodeToString(sum[:16])
	build := func(carried noticeTexts) ([]byte, relayTooLargePayload, error) {
		payload := relayTooLargePayload{
			Reason:         relayTooLargeReason,
			EventID:        carried.eventID,
			OriginalTopic:  carried.originalTopic,
			StreamSeq:      msg.StreamSeq,
			Size:           len(msg.Data),
			FrameSize:      frameSize,
			MaxPayload:     maxPayload,
			Source:         carried.source,
			SourceSession:  carried.sourceSession,
			PayloadSummary: carried.payloadSummary,
			Repo:           carried.repo,
			Cut:            held.cut(carried),
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, payload, fmt.Errorf("encode the too-large notice: %w", err)
		}
		named := payload.EventID
		if named == "" {
			named = "the message at stream sequence " + seq
		}
		notice := contracts.Envelope{
			EventID:        noticeID,
			Source:         "envoy",
			SourceEventID:  cmp.Or(payload.EventID, noticeID),
			Topic:          payload.OriginalTopic,
			DedupeKey:      "envoy." + relayTooLargeReason + "." + noticeID,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: fmt.Sprintf("a message too large to deliver: %s on %s, %d bytes", named, noticeField(payload.OriginalTopic), payload.Size),
			Payload:        string(encoded),
			TraceID:        cmp.Or(carried.traceID, noticeID),
			Role:           carried.role,
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
	data, payload, err := build(held.carried(msg.Subject))
	if err != nil || int64(len(data)) <= room {
		return data, payload, err
	}
	// The bare notice: every text of the message left out, the topic the stored subject, which
	// the frame's own Envoy-Subject header already carries, and the numbers kept.
	data, payload, err = build(noticeTexts{originalTopic: msg.Subject})
	if err != nil {
		return nil, payload, err
	}
	if int64(len(data)) > room {
		return nil, payload, fmt.Errorf("%w: the bare notice takes %d bytes and its frame leaves %d", errRelayNoticeTooLarge, len(data), room)
	}
	return data, payload, nil
}

// noticeTexts is each text of a message a notice copies, under the name the notice's cut gives it:
// the payload's own (relayTooLargePayload) for the six the payload carries, and the notice
// envelope's own (contracts.Envelope) for the role mark and the trace id, which the envelope
// carries. It is both what a message holds (heldNoticeTexts) and what a notice carries of it
// (carried, and the bare notice's), so a notice's cut is the difference of the two (cut), one rule
// for every text and the topic.
type noticeTexts struct {
	eventID, originalTopic, source, sourceSession, payloadSummary, repo, role, traceID string
	// repoUnread, on what a message holds, is a repository the event names in a way readers
	// disagree on (eventRepository's errAmbiguousFrameKey and errPrototypeFrameKey): it holds a
	// repo that no notice can carry, so cut names it. What a notice carries never sets it.
	repoUnread bool
}

// heldNoticeTexts is each text of original, the message the stream stored as data, as the message
// holds it (noticeTexts). Its repo is the repository the GitHub event names (eventRepository),
// owner/name, and none for an event that names none; an event that names one in a way readers
// disagree on holds one no notice carries (repoUnread).
func heldNoticeTexts(original contracts.Envelope, data []byte) noticeTexts {
	held := noticeTexts{
		eventID:        original.EventID,
		originalTopic:  original.Topic,
		source:         original.Source,
		sourceSession:  original.SourceSession,
		payloadSummary: original.PayloadSummary,
		role:           original.Role,
		traceID:        original.TraceID,
	}
	owner, name, named, err := eventRepository(data)
	switch {
	case err != nil:
		held.repoUnread = true
	case named:
		held.repo = owner + "/" + name
	}
	return held
}

// carried is what a notice carries of a message that holds held, each text bounded: cut to one
// line of relayNoticeFieldRunes (noticeField); a repo past GitHub's own limits left out whole
// (noticeRepo), and so is one readers disagree on; a role mark the notice would change left out
// whole, since a role mark cut short would name another role, which an edge socket judges the
// notice by; and a topic that is empty or past what NATS takes as a subject
// (contracts.SubjectMaxBytes), which no publish of the bus made, replaced by subject, the one the
// stream stored the message on.
func (held noticeTexts) carried(subject string) noticeTexts {
	topic := held.originalTopic
	if strings.TrimSpace(topic) == "" || len(topic) > contracts.SubjectMaxBytes {
		topic = subject
	}
	role := noticeField(held.role)
	if role != held.role {
		role = ""
	}
	return noticeTexts{
		eventID:        noticeField(held.eventID),
		originalTopic:  topic,
		source:         noticeField(held.source),
		sourceSession:  noticeField(held.sourceSession),
		payloadSummary: noticeField(held.payloadSummary),
		repo:           noticeRepo(held.repo),
		role:           role,
		traceID:        noticeField(held.traceID),
	}
}

// cut is relayTooLargePayload's cut of a notice that carries carried of a message that holds held:
// the name of each text the message holds that the notice does not carry as the message holds it
// - cut short, left out, or a topic replaced by the stored subject - in the order the payload and
// then the envelope carry them, and nil when the notice carries every text the message holds
// whole. A text the message does not hold is no cut, whatever the notice carries in its place.
func (held noticeTexts) cut(carried noticeTexts) []string {
	var cut []string
	for _, text := range []struct {
		name, held, carried string
		unread              bool
	}{
		{"event_id", held.eventID, carried.eventID, false},
		{"original_topic", held.originalTopic, carried.originalTopic, false},
		{"source", held.source, carried.source, false},
		{"source_session", held.sourceSession, carried.sourceSession, false},
		{"payload_summary", held.payloadSummary, carried.payloadSummary, false},
		{"repo", held.repo, carried.repo, held.repoUnread},
		{"role", held.role, carried.role, false},
		{"trace_id", held.traceID, carried.traceID, false},
	} {
		if text.unread || (text.held != "" && text.carried != text.held) {
			cut = append(cut, text.name)
		}
	}
	return cut
}

// noticeField is text as a notice carries it: its first non-empty line, cut to
// relayNoticeFieldRunes with an ellipsis (contracts.OneLineSummary), and "" for one that holds no
// such line. A text it does not return unchanged is one the notice does not carry whole, which
// noticeTexts.cut names. The message the notice stands for is read nowhere else, so nothing else
// bounds what its fields hold.
func noticeField(text string) string {
	return contracts.OneLineSummary(strings.TrimSpace(text))
}

// noticeRepo is repo, the owner/name a GitHub event names (heldNoticeTexts), as a notice carries
// it: whole, or left out whole when its owner or name is past GitHub's own limits, since such a
// repo names no GitHub repository and cutting it would name another one.
func noticeRepo(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	if utf8.RuneCountInString(owner) > githubOwnerRunes || utf8.RuneCountInString(name) > githubNameRunes {
		return ""
	}
	return repo
}
