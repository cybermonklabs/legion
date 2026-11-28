package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// KeptExpiredSubjectPrefix begins the expiry notice of every role (KeptExpiredSubject): what
// Dispatch's outbox reads all of them by, from the stream sequence it last read
// (internal/dispatch/outbox, readKeptExpiries). It lies under KeptTopicPrefix, so the stream
// carries it as one of the listener's own lanes: no consumer filter takes it, no session may
// register an interest reaching it, the publish route refuses a topic under it, and only a NATS
// user whose grant publishes every notification topic - a listener's - can write it
// (natsgrant.listenerGrant).
const KeptExpiredSubjectPrefix = KeptTopicPrefix + "expired."

// KeptExpiredSubject is the subject of role's expiry notices: one for each copy on role's kept
// lane (KeptRoleSubject) that Dispatch answers for (KeptCopyOutbox, KeptCopyDispatchDelivery),
// whose hour ended with no session taking it, which the listener's walk stores as it drops that
// copy (cmd/listener/role_keep.go), and whose notice also ends a copy of Dispatch's delivery of a
// post when the role's live holder does not take its delivery mode. The role name is written into
// the subject as KeptRoleSubject writes it.
func KeptExpiredSubject(role string) string {
	return KeptExpiredSubjectPrefix + role
}

// PublishDedupePrefix opens every dedupe key the listener's publish route makes for a request that
// chose none: publish.<a new id>, or publish.<the request's idempotency_key> (cmd/listener,
// publishHandler). The route refuses a dedupe key chosen for an envelope with source dispatch, so
// every copy it keeps of a dispatch envelope carries the prefix, and a takeover's copy of such a
// message keeps it in its own key (cmd/listener, takenFrameDedupeKey). Dispatch's outbox keeps
// its copies under keys of its own, dispatch-<id> or a mention's key, which never begin with it,
// so the prefix tells a copy the route kept from one the outbox kept (ClassifyKeptCopy).
const PublishDedupePrefix = "publish."

// KeptCopyKind is who answers for a copy on a role's kept lane that ends with no session taking
// it, and so what the listener's walk does at its end (ClassifyKeptCopy).
type KeptCopyKind uint8

const (
	// KeptCopySession is a copy the listener kept whose publish named a source session, which the
	// walk sends the copy's dead letter.
	KeptCopySession KeptCopyKind = iota
	// KeptCopyUnanswered is a copy the listener kept whose publish named no source session: no
	// one hears of its end. The publish route keeps no such send for a role with no live holder
	// (cmd/listener, roleSend.keepTellsNobody); a message published straight onto the role's
	// subject, and a send Dispatch marked with a role that moved, can still leave one.
	KeptCopyUnanswered
	// KeptCopyOutbox is a copy Dispatch's outbox kept for a role-routed event, under a key of its
	// own and the event's decimal id as its source event id. The walk ends it with an expiry
	// notice, and Dispatch's outbox keeps the event again or records it undelivered. It raises no
	// wake, as the outbox's own keep raises none.
	KeptCopyOutbox
	// KeptCopyDispatchDelivery is a copy of Dispatch's delivery of a post that the publish route
	// kept for a role's first holder (DispatchPostEventID). The walk ends it with an expiry notice,
	// and Dispatch settles the delivery's attempts the copy carried. It is the listener's own copy
	// otherwise: the route raised the role's wake as it kept it.
	KeptCopyDispatchDelivery
)

// IsDispatch reports whether k is one of the copies Dispatch answers for (KeptCopyOutbox,
// KeptCopyDispatchDelivery), whose end a walk tells Dispatch with an expiry notice.
func (k KeptCopyKind) IsDispatch() bool {
	return k == KeptCopyOutbox || k == KeptCopyDispatchDelivery
}

// ClassifyKeptCopy is the kind of item, a copy on a role's kept lane, and the one rule every
// reader of a kept copy decides it by: the listener's walk, its keep and its wake scan, and
// Dispatch's outbox, which reads a notice's copy back (KeptCopyExpiry.CopyKind). A copy whose
// source is dispatch and that carries no role mark (contracts.Envelope.Role) is Dispatch's when
// its dedupe key and source event id agree on which of Dispatch's copies it is
// (dispatchCopyKind). Every other copy is the listener's: a send Dispatch's API marked with its
// role (the send route kept it, or a takeover copied it from its holder's subject), and every
// copy from any other source. It is KeptCopySession when it names a source session and
// KeptCopyUnanswered when it names none.
func ClassifyKeptCopy(item contracts.Envelope) KeptCopyKind {
	if item.Source == "dispatch" && item.Role == "" {
		if kind := dispatchCopyKind(item.DedupeKey, item.SourceEventID); kind.IsDispatch() {
			return kind
		}
	}
	if item.SourceSession != "" {
		return KeptCopySession
	}
	return KeptCopyUnanswered
}

// dispatchCopyKind is which of Dispatch's copies a dispatch copy's dedupe key and source event id
// name, when the two agree on one:
//
//   - KeptCopyDispatchDelivery: the key begins with PublishDedupePrefix, and the source event id
//     names a post (ParseDispatchPostEventID), which only the publish route writes, from
//     Dispatch's own principal;
//   - KeptCopyOutbox: the key does not begin with PublishDedupePrefix, and the source event id
//     names no post.
//
// A key and a source event id that disagree - a key of the outbox's beside a post's source event
// id, or a key of the route's beside none - name neither, and answer KeptCopyUnanswered, which is
// none of Dispatch's (IsDispatch), so no copy is told to Dispatch as an outbox copy by one reader
// and as a delivery by another. It is the one rule both a copy (ClassifyKeptCopy) and a notice of
// one (KeptCopyExpiry.CopyKind) are judged by.
func dispatchCopyKind(dedupeKey, sourceEventID string) KeptCopyKind {
	_, delivers := ParseDispatchPostEventID(sourceEventID)
	switch routeKey := strings.HasPrefix(dedupeKey, PublishDedupePrefix); {
	case routeKey && delivers:
		return KeptCopyDispatchDelivery
	case !routeKey && !delivers:
		return KeptCopyOutbox
	}
	return KeptCopyUnanswered
}

// dispatchPostEventIDPrefix begins the source_event_id of the envelope of a role publish that
// delivers a Dispatch post (DispatchPostEventID).
const dispatchPostEventIDPrefix = "dispatch-post:"

// DispatchPostEventID is the source_event_id the listener's publish route writes into the envelope
// it builds for a role publish that delivers post (listenerwire.PublishRequest.DispatchPost, which
// the route takes from Dispatch's own verified principal alone): dispatch-post:<kind>:<id>. No
// other writer of that route produces it - every other envelope the route builds carries a new id
// of 32 hex digits (contracts.MessageEnvelope, internal/id) - and Dispatch's outbox writes its
// event's decimal id there. So a kept copy that carries it under the route's key is a delivery of
// Dispatch's the route kept (KeptCopyDispatchDelivery), and the expiry notice of that copy
// (KeptCopyExpiry) names the post by it, which Dispatch's outbox reads back
// (ParseDispatchPostEventID) to settle the attempts of that post the copy carried. The envelope
// gains no field for it: the envelope's schema takes none
// (packages/contracts/schemas/envelope.schema.json), and the plugins that read envelopes only
// carry this one. post is one ValidDispatchPost takes, which the route checks first.
func DispatchPostEventID(post listenerwire.DispatchPost) string {
	return dispatchPostEventIDPrefix + post.Kind + ":" + post.ID
}

// ParseDispatchPostEventID is the post a source_event_id in DispatchPostEventID's form names, and
// whether it is in that form: the prefix, then a kind and an id ValidDispatchPost takes, so every
// id it takes is the one spelling DispatchPostEventID writes for its post. An outbox event's
// decimal id, and every id the publish route mints for anything else, is in no such form. A
// Dispatch of an earlier release reads a notice carrying it as one whose source event id is no
// event id of its own (it is not a number), and passes it over with one Error line and its cursor
// moved past it (internal/dispatch/outbox, recordKeptExpiry).
func ParseDispatchPostEventID(sourceEventID string) (listenerwire.DispatchPost, bool) {
	rest, ok := strings.CutPrefix(sourceEventID, dispatchPostEventIDPrefix)
	if !ok {
		return listenerwire.DispatchPost{}, false
	}
	kind, id, ok := strings.Cut(rest, ":")
	post := listenerwire.DispatchPost{Kind: kind, ID: id}
	if !ok || !ValidDispatchPost(post) {
		return listenerwire.DispatchPost{}, false
	}
	return post, true
}

// ValidDispatchPost reports whether post names a Dispatch post a role publish may deliver: its kind
// is listenerwire.DispatchPostComment or listenerwire.DispatchPostMessage, and its id a UUID in its
// canonical lower-case form, the one Dispatch's store writes.
func ValidDispatchPost(post listenerwire.DispatchPost) bool {
	if post.Kind != listenerwire.DispatchPostComment && post.Kind != listenerwire.DispatchPostMessage {
		return false
	}
	parsed, err := uuid.Parse(post.ID)
	return err == nil && parsed.String() == post.ID
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

// KeptCopyExpiry is the body of an expiry notice (KeptExpiredSubject): a copy on Role's kept lane
// that Dispatch answers for ended with no session taking it, and a listener's walk dropped it -
// a copy Dispatch's outbox kept, at the end of its hour, or a copy of Dispatch's delivery of a
// post the publish route kept, at the end of its hour or when the role's live holder does not
// take its delivery mode. It names the copy's envelope by its EventID and SourceEventID - for an
// outbox copy the outbox's own event id, which Dispatch reopens the event's delivery to the role
// by, and for a delivery's copy the post it delivers (DispatchPostEventID), whose attempts that
// copy carried Dispatch settles undelivered - the Attempt it was kept for (KeptCopyAttempt), its
// DedupeKey, the stream Sequence it was stored at on the kept lane, which is where Dispatch reads
// the copy back, the Unix-millisecond times the stream stored it (KeptAt) and the walk dropped it
// (DroppedAt), and ForwardUnconfirmed, whether a walk claimed the copy to forward it and no walk
// confirmed that forward, so the copy may have reached the session that held the role then.
//
// It lives here, as KeptRoleCursor does, because the listener writes it and Dispatch's outbox
// reads it: a body each spelled out for itself would decode a drifted tag as zero.
type KeptCopyExpiry struct {
	EventID            string `json:"event_id"`
	SourceEventID      string `json:"source_event_id"`
	Role               string `json:"role"`
	Attempt            int    `json:"attempt"`
	DedupeKey          string `json:"dedupe_key"`
	Sequence           uint64 `json:"sequence"`
	KeptAt             int64  `json:"kept_at"`
	DroppedAt          int64  `json:"dropped_at"`
	ForwardUnconfirmed bool   `json:"forward_unconfirmed,omitempty"`
}

// CopyKind is the kind of copy n names, by the one rule ClassifyKeptCopy applies to a dispatch
// copy (dispatchCopyKind): KeptCopyOutbox or KeptCopyDispatchDelivery when n's dedupe key and
// source event id agree on which of Dispatch's copies it names, and KeptCopyUnanswered when they
// do not. Every reader of a notice judges it here, so the listener and Dispatch's outbox can never
// read one copy as two kinds.
func (n KeptCopyExpiry) CopyKind() KeptCopyKind {
	return dispatchCopyKind(n.DedupeKey, n.SourceEventID)
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
