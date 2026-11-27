package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
)

// KeptExpiredSubjectPrefix begins the expiry notice of every role (KeptExpiredSubject): what
// Dispatch's outbox reads all of them by, from the stream sequence it last read
// (internal/dispatch/outbox, readKeptExpiries). It lies under KeptTopicPrefix, so the stream
// carries it as one of the listener's own lanes: no consumer filter takes it, no session may
// register an interest reaching it, the publish route refuses a topic under it, and only a NATS
// user whose grant publishes every notification topic - a listener's - can write it
// (natsgrant.listenerGrant).
const KeptExpiredSubjectPrefix = KeptTopicPrefix + "expired."

// KeptExpiredSubject is the subject of role's expiry notices: one for each copy Dispatch's outbox
// kept on role's kept lane (KeptRoleSubject) whose hour ended with no session taking it, which
// the listener's walk stores as it drops that copy (cmd/listener/role_keep.go). The role name is
// written into the subject as KeptRoleSubject writes it.
func KeptExpiredSubject(role string) string {
	return KeptExpiredSubjectPrefix + role
}

// KeptExpiryMsgID is the Nats-Msg-Id of the expiry notice of the copy stored at seq on role's
// kept lane: envoy.kept-expired.<role>.<seq>, which no other copy has. Every listener whose walk
// drops that copy, and every walk that takes over the claim of one that did, stores the notice
// under it, so the stream holds one notice for each copy, as a dead letter is held once.
func KeptExpiryMsgID(role string, seq uint64) string {
	return "envoy.kept-expired." + role + "." + strconv.FormatUint(seq, 10)
}

// keptAttemptSeparator joins a kept copy's message id to its attempt (KeptAttemptMsgID).
const keptAttemptSeparator = ":attempt:"

// KeptAttemptMsgID is the Nats-Msg-Id Dispatch's outbox keeps the attempt'th copy of a role-routed
// event under, whose dedupe key is dedupeKey, on role's kept lane. The first keep is KeptMsgID,
// the id the listener keeps the same message under, so the two writers of the lane still store one
// copy of it. A later keep - the outbox keeping the event again after its copy's hour ended with
// no session taking it (internal/dispatch/outbox, keepExpiredCopiesAgain) - adds the attempt,
// since the stream's duplicate window spans the whole retained lifetime of a message
// (streamDuplicateWindow) and would otherwise take the new copy for the one it already holds and
// store nothing. The copy's envelope keeps its own dedupe key, so a holder that already took an
// earlier copy drops the later one. The listener reads the attempt back from the stored copy's id
// (KeptCopyAttempt).
func KeptAttemptMsgID(dedupeKey, role string, attempt int) string {
	id := KeptMsgID(dedupeKey, role)
	if attempt <= 1 {
		return id
	}
	return id + keptAttemptSeparator + strconv.Itoa(attempt)
}

// KeptCopyAttempt is the attempt a copy stored on role's kept lane under msgID was kept for, whose
// envelope's dedupe key is dedupeKey: the one KeptAttemptMsgID wrote into the id, and 1 for every
// other id - the first keep, the listener's own keep, a takeover's copy under a key of its own, and
// a copy an earlier outbox kept before it named attempts.
func KeptCopyAttempt(msgID, dedupeKey, role string) int {
	rest, ok := strings.CutPrefix(msgID, KeptMsgID(dedupeKey, role))
	if !ok {
		return 1
	}
	digits, ok := strings.CutPrefix(rest, keptAttemptSeparator)
	if !ok {
		return 1
	}
	attempt, err := strconv.Atoi(digits)
	if err != nil || attempt < 2 {
		return 1
	}
	return attempt
}

// KeptCopyExpiry is the body of an expiry notice (KeptExpiredSubject): a copy Dispatch's outbox
// kept on Role's kept lane ended its hour with no session taking it, and a listener's walk dropped
// it. It names the copy's envelope by its EventID and SourceEventID - the outbox's own event id,
// which Dispatch reopens the event's delivery to the role by - the Attempt it was kept for
// (KeptCopyAttempt), its DedupeKey, the stream Sequence it was stored at on the kept lane, which is
// where Dispatch reads the copy back to keep it again, and the Unix-millisecond times the stream
// stored it (KeptAt) and the walk dropped it (DroppedAt).
//
// It lives here, as KeptRoleCursor does, because the listener writes it and Dispatch's outbox
// reads it: a body each spelled out for itself would decode a drifted tag as zero.
type KeptCopyExpiry struct {
	EventID       string `json:"event_id"`
	SourceEventID string `json:"source_event_id"`
	Role          string `json:"role"`
	Attempt       int    `json:"attempt"`
	DedupeKey     string `json:"dedupe_key"`
	Sequence      uint64 `json:"sequence"`
	KeptAt        int64  `json:"kept_at"`
	DroppedAt     int64  `json:"dropped_at"`
}

// PublishKeptCopyExpiry stores notice on its role's expiry subject (KeptExpiredSubject) under the
// copy's own message id (KeptExpiryMsgID), within ctx's bound as every stored publish is. A
// notice the stream already holds under that id is no error. Its refusals are those of every raw
// stored write (publishStoredRaw, sendStored): ErrPublishDenied, ErrTooLarge, ErrStreamFull, and a
// failure that wraps ErrUnacknowledged when the stream may hold the notice.
func (c *Client) PublishKeptCopyExpiry(ctx context.Context, notice KeptCopyExpiry) error {
	data, err := json.Marshal(notice)
	if err != nil {
		return fmt.Errorf("encode the expiry notice of role %s's copy %d: %w", notice.Role, notice.Sequence, err)
	}
	_, err = c.publishStoredRaw(ctx, KeptExpiredSubject(notice.Role), data, nats.MsgId(KeptExpiryMsgID(notice.Role, notice.Sequence)))
	return err
}
