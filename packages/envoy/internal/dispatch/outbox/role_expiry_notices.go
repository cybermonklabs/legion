// The half of the expiry pass (passKeptExpiries, role_expiry.go) that reads the listeners' expiry
// notices and records each in outbox_role_copy_expiries, or, for a notice of a copy of Dispatch's
// delivery of a post the listener kept, settles the attempts that copy carried; role_expiry.go
// keeps the events again.

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/listenerwire"
)

// keptExpiryNotices is the filter every expiry notice matches (bus.KeptExpiredSubject).
const keptExpiryNotices = bus.KeptExpiredSubjectPrefix + ">"

// readKeptExpiries reads the expiry notices the stream holds past the outbox's cursor
// (outbox_kept_expiry_cursor, keptExpiryStart), batchSize of them at most, in stream order, and
// records each with the cursor's move past it in one transaction (recordKeptExpiry), so a notice
// is recorded once however often the read is made and whichever process makes it. It reports full
// when it read a whole batch, so the holder reads again at once, and failed when a read or a
// record failed: the cursor stands before that notice, and the next read takes it up again.
//
// Only the publisher hold's holder records a notice: each record's transaction reads the hold's
// row and writes nothing - no expiry, no record, no cursor move - while instance does not hold it
// (errNotTheHolder), and a pass past the hold's deadline reads nothing. So a process the hold has
// left cannot record an expiry the holder's own pass is about to keep again.
func readKeptExpiries(ctx context.Context, deps Deps, hold fence, instance uuid.UUID, stream *keptStreamRead) (full, failed bool) {
	if deps.KeptLanes == nil || ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
		return false, false
	}
	after, proceed, failed := keptExpiryStart(ctx, deps, instance, stream)
	if !proceed {
		return false, failed
	}
	for range batchSize {
		stored, err := deps.KeptLanes.NextStoredMessage(ctx, keptExpiryNotices, after+1)
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return false, false
		}
		if err != nil {
			if !lostToContextEnd(ctx, err) {
				slog.Error("dispatch outbox: read the expiry notices", "after", after, "error", err)
			}
			return false, true
		}
		if err := recordKeptExpiry(ctx, deps, instance, stored, stream.id); err != nil {
			if errors.Is(err, errNotTheHolder) {
				slog.Warn("dispatch outbox: the publisher hold is no longer this process's, so the expiry notices are left to the hold's next pass",
					"sequence", stored.Sequence)
				return false, false
			}
			if !lostToContextEnd(ctx, err) {
				slog.Error("dispatch outbox: record an expiry notice", "sequence", stored.Sequence, "error", err)
			}
			return false, true
		}
		after = stored.Sequence
	}
	return true, false
}

// keptExpiryStart is the sequence the notices' read goes on past, and whether the read proceeds
// this pass; failed reports a read or a write that failed, and the holder backs off.
//
// The cursor's sequence counts in one stream, whose identity (bus.StreamIDMetadataKey) the cursor
// names beside it. A stream created again, as a hub moved without its stream's snapshot is,
// numbers its notices from its own first message, below a cursor of the stream before it, where a
// read from the cursor would find none of them and say nothing. So the read compares the cursor's
// identity with the stream's (keptStreamRead), and a cursor that names another stream is set back
// to the start of this one, with one Error line: the notices are read from the first the stream
// holds, and a notice recorded already is a repeat that records nothing (recordExpiry). The
// identity is taken again before the compare when it differs from the cursor's, so a cursor is
// never set back on an identity this process took before the stream was created again. The set
// back is fenced on the publisher hold and on the cursor's own values: a process the hold has
// left, or one that read a cursor another write has moved since, writes nothing and does not
// read, and the next pass reads the cursor as it stands then.
func keptExpiryStart(ctx context.Context, deps Deps, instance uuid.UUID, stream *keptStreamRead) (after uint64, proceed, failed bool) {
	var cursor int64
	var cursorStream string
	err := deps.Store.Pool.QueryRow(ctx, `select sequence, stream_id from outbox_kept_expiry_cursor`).Scan(&cursor, &cursorStream)
	written := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		if !lostToContextEnd(ctx, err) {
			slog.Error("dispatch outbox: read the expiry notices' cursor", "error", err)
		}
		return 0, false, true
	}
	if err := stream.refresh(ctx, deps.KeptLanes, written && cursorStream != stream.id); err != nil {
		if !lostToContextEnd(ctx, err) {
			slog.Error("dispatch outbox: read the stream's identity for the expiry notices' cursor", "error", err)
		}
		return 0, false, true
	}
	if !written || cursorStream == stream.id {
		return uint64(cursor), true, false
	}
	tag, err := deps.Store.Pool.Exec(ctx, `
		update outbox_kept_expiry_cursor set sequence = 0, stream_id = $3
		where sequence = $1 and stream_id = $2
		  and exists (select 1 from duty_holds where name = $4 and holder = $5 and expires_at > now())
	`, cursor, cursorStream, stream.id, publisherHold, instance)
	if err != nil {
		if !lostToContextEnd(ctx, err) {
			slog.Error("dispatch outbox: set the expiry notices' cursor back to the stream's start", "error", err)
		}
		return 0, false, true
	}
	if tag.RowsAffected() == 0 {
		return 0, false, false
	}
	slog.Error("dispatch outbox: the expiry notices' cursor counts in a stream created again since; it is set back to this stream's start, and its notices are read from the first it holds",
		"cursor", cursor, "cursor_stream_id", cursorStream, "stream_id", stream.id)
	return 0, true, false
}

// keptStreamRead is the stream's identity as the expiry notices' read last took it
// (keptExpiryStart), and when. The identity changes only when the stream is created again, so a
// holder takes it once every keptExpiryReadEvery rather than on every pass: the pass after each
// scan would otherwise send a stream info each time, which the stream's leader answers. A pass
// that reads with an identity taken before the stream was created again records the notices it
// finds under that identity, and the next take sets the cursor back, so those notices are read
// again as repeats and the ones below the old cursor are read for the first time.
type keptStreamRead struct {
	id string
	at time.Time
}

// refresh takes the stream's identity from reader when force is set, when none was taken, or when
// the last take is keptExpiryReadEvery old. A stream that carries no identity is an error
// (bus.ErrNoStreamID): no notice is read until the stream has one, which every start of a
// program that keeps the stream gives it.
func (r *keptStreamRead) refresh(ctx context.Context, reader KeptLaneReader, force bool) error {
	if !force && !r.at.IsZero() && time.Since(r.at) < keptExpiryReadEvery {
		return nil
	}
	id, err := reader.StreamID(ctx)
	if err != nil {
		return err
	}
	*r = keptStreamRead{id: id, at: time.Now()}
	return nil
}

// recordKeptExpiry records the expiry notice stored, and moves the cursor past it in the stream
// whose identity is streamID, in one holder transaction (inHolderTransaction) under recordTimeout,
// which publishes the events the record appended, if any, once it has committed, and writes
// nothing and answers errNotTheHolder while the publisher hold is not instance's. A notice names
// one of two copies by its source event id:
//
//   - a copy Dispatch's outbox kept for a role names the outbox's event by its decimal id: the
//     notice is recorded in outbox_role_copy_expiries (recordExpiry), which may append the event's
//     role.undelivered record;
//   - a copy of Dispatch's delivery of a post that the listener's publish route kept for a role's
//     first holder names the post (bus.ParseDispatchPostEventID): the notice settles every attempt
//     of that post the copy carried that nobody answered, undelivered (settleKeptDelivery), which
//     appends a receipt for each. Nothing is kept again for it, and no outbox_role_copy_expiries
//     row is written.
//
// A notice is passed over with one Error line, and only the cursor moves, when it names no copy
// Dispatch answers for: its body does not decode; its source event id names neither an event nor
// a post; its role is no role name (roles.Valid); it was stored on another role's expiry subject
// than the one its role names; the stream holds something else than that copy at the sequence the
// notice names (noticedCopy); the event it names was never routed to that role - the event's own
// destinations, and its comment's fallback rows, do not name the role's topic - which an event the
// log does not hold is not either; or the post it names is one the store does not hold. No read of
// such a notice will ever name one. Only a NATS user on the listener's grant can store a notice at
// all, so this guards the role that becomes a subject (bus.KeptRoleSubject) and is written into a
// posted message, and the event whose delivery to the role a notice reopens or ends, and the post
// whose attempts it settles, not a door. The notice's own attempt decides nothing (recordExpiry).
//
// A Dispatch of an earlier release reads the notice of a delivery's copy as one whose source event
// id is not a number, and passes it over with that one Error line, its cursor moved past it.
func recordKeptExpiry(ctx context.Context, deps Deps, instance uuid.UUID, stored bus.StoredMessage, streamID string) error {
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	var notice bus.KeptCopyExpiry
	decodeErr := json.Unmarshal(stored.Data, &notice)
	post, delivery := bus.ParseDispatchPostEventID(notice.SourceEventID)
	var eventID int64
	var parseErr error
	if !delivery {
		eventID, parseErr = strconv.ParseInt(notice.SourceEventID, 10, 64)
	}
	refusal := errors.Join(decodeErr, parseErr)
	switch {
	case refusal != nil:
	case !roles.Valid(notice.Role):
		refusal = fmt.Errorf("its role %q is no role name", notice.Role)
	case stored.Subject != bus.KeptExpiredSubject(notice.Role):
		refusal = fmt.Errorf("it was stored on %s, not on role %s's expiry subject", stored.Subject, notice.Role)
	default:
		// Read before the transaction, which holds no lock across a read of NATS.
		reason, err := noticedCopy(ctx, deps.KeptLanes, stored, notice)
		if err != nil {
			return err
		}
		if reason != "" {
			refusal = errors.New(reason)
		}
	}
	return inHolderTransaction(ctx, deps, instance, func(tx pgx.Tx) ([]model.Event, error) {
		var appended []model.Event
		switch {
		case refusal != nil:
		case delivery:
			receipts, held, err := settleKeptDelivery(ctx, deps, tx, post, notice)
			if err != nil {
				return nil, err
			}
			if !held {
				refusal = fmt.Errorf("the %s %s it names is not one the store holds", post.Kind, post.ID)
			}
			appended = receipts
		default:
			routed, err := routedToRole(ctx, tx, eventID, notice.Role)
			if err != nil {
				return nil, err
			}
			if !routed {
				refusal = fmt.Errorf("event %d was never routed to role %s", eventID, notice.Role)
			} else if appended, err = oneEvent(recordExpiry(ctx, deps, tx, eventID, notice)); err != nil {
				return nil, err
			}
		}
		if refusal != nil {
			slog.Error("dispatch outbox: an expiry notice names no copy Dispatch answers for on a role; it is passed over",
				"sequence", stored.Sequence, "subject", stored.Subject, "event_id", notice.EventID, "role", notice.Role,
				"copy_sequence", notice.Sequence, "error", refusal)
		}
		// The cursor names the identity keptExpiryStart read it under, which set back a cursor of
		// another stream before this notice was read, so the two differ here only when another
		// holder wrote the row since: the row then takes this notice's sequence and this pass's
		// identity, and a pass that reads another identity sets it back again.
		if _, err := tx.Exec(ctx, `
			insert into outbox_kept_expiry_cursor (id, sequence, stream_id) values (true, $1, $2)
			on conflict (id) do update set
				sequence = case when outbox_kept_expiry_cursor.stream_id = excluded.stream_id
					then greatest(outbox_kept_expiry_cursor.sequence, excluded.sequence) else excluded.sequence end,
				stream_id = excluded.stream_id
		`, int64(stored.Sequence), streamID); err != nil {
			return nil, err
		}
		return appended, nil
	})
}

// keptDeliveryExpiredError is the error an attempt of Dispatch's delivery of a post is settled
// failed with when the copy the listener kept of it for the role ended with no session taking it
// (settleKeptDelivery).
const keptDeliveryExpiredError = "The Envoy listener kept this message for the role, and no session took it before the listener dropped it."

// settleKeptDelivery settles, in tx, the attempts of post that the copy notice names carried and
// that nobody answered, as undelivered: the copy the listener's publish route kept for the role's
// first holder (Dispatch's publishForRole, internal/dispatch/api) ended with no session taking it -
// its hour ended, or the role's live holder does not take its delivery mode. It reports false, and
// writes nothing, for a post the store does not hold. The post's owner is locked first
// (events.Broker.LockOwners), then its attempts are updated, the order every writer of these rows
// that appends a receipt takes (settleAbandoned in mentions.go, settleDeliveryAttempt in
// internal/dispatch/api).
//
// An attempt the copy carried records the copy's envelope, notice.EventID, which publishForRole
// recorded on it - a second attempt that repeated the first's key records the same copy's id - so
// every attempt still queued under that envelope with no session pinned and no reply is settled:
// failed, with keptDeliveryExpiredError, and with its envelope cleared, since a failed attempt
// that carries an envelope reads as one its session answered with an error, which the dashboard
// offers no Retry for (rowAnsweredWithError in packages/dispatch/web/src/features/conversation/
// delivery.ts). An attempt a holder answered (sent, or queued with a session pinned), one already
// failed, and one under another envelope are left as they are, so a repeated notice settles
// nothing twice. Each settled attempt gets the comment.delivery or message.delivery receipt the API
// writes for a failed attempt, appended in tx and published by the caller once it has committed.
// Its actor is the outbox's own system actor (graphWriter), as the role.undelivered record's is
// (appendUndelivered): the settlement is this process's finding at the copy's end, nobody's act,
// and a person's or a session's actor would read as their own activity at that moment to every
// reader that dates activity by an event's actor. A receipt notifies nobody whoever its actor
// (events.Quiet).
func settleKeptDelivery(ctx context.Context, deps Deps, tx pgx.Tx, post listenerwire.DispatchPost, notice bus.KeptCopyExpiry) ([]model.Event, bool, error) {
	var receipts []model.Event
	var settled int
	switch post.Kind {
	case listenerwire.DispatchPostComment:
		var owner model.Event
		var askID *string
		err := tx.QueryRow(ctx, `select issue_key, artifact_id::text, ask_id::text from comments where id = $1`, post.ID).
			Scan(&owner.IssueKey, &owner.ArtifactID, &askID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if err := deps.Broker.LockOwners(ctx, tx, owner); err != nil {
			return nil, false, err
		}
		rows, err := tx.Query(ctx, `
			update comment_deliveries set state = 'failed', error = $3, envelope_id = null
			where comment_id = $1 and envelope_id = $2 and state = 'queued' and session_id is null and reply_id is null
			returning target, attempt, delivery, duplicate
		`, post.ID, notice.EventID, keptDeliveryExpiredError)
		if err != nil {
			return nil, false, err
		}
		payloads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CommentDeliveryEventPayload, error) {
			payload := model.CommentDeliveryEventPayload{
				CommentID: post.ID, AskID: askID, State: model.DeliveryStateFailed, Error: keptDeliveryExpiredError,
			}
			return payload, row.Scan(&payload.Target, &payload.Attempt, &payload.Delivery, &payload.Duplicate)
		})
		if err != nil {
			return nil, false, err
		}
		for _, payload := range payloads {
			receipt := owner
			receipt.Type, receipt.Actor, receipt.Payload = "comment.delivery", graphWriter, payload
			appended, err := deps.Broker.Append(ctx, tx, receipt)
			if err != nil {
				return nil, false, fmt.Errorf("append the comment.delivery receipt of comment %s's attempt %d: %w", post.ID, payload.Attempt, err)
			}
			receipts = append(receipts, appended)
		}
		settled = len(payloads)
	case listenerwire.DispatchPostMessage:
		var issueKey, target *string
		err := tx.QueryRow(ctx, `select issue_key, target from messages where id = $1`, post.ID).Scan(&issueKey, &target)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		// A message on an issue is that issue's, and one on no issue is its target session's, as
		// the API's receipt of it is (messageEvent in internal/dispatch/api); a message to a role
		// is always on an issue, since its own creation appends nothing ownerless for a role.
		owner := model.Event{IssueKey: issueKey}
		messageTarget := ""
		if target != nil {
			messageTarget = *target
		}
		if issueKey == nil {
			owner.Payload = model.MessageDeliveryEventPayload{Target: messageTarget}
		}
		if err := deps.Broker.LockOwners(ctx, tx, owner); err != nil {
			return nil, false, err
		}
		rows, err := tx.Query(ctx, `
			update message_deliveries set state = 'failed', error = $3, envelope_id = null
			where message_id = $1 and envelope_id = $2 and state = 'queued' and session_id = '' and reply_id is null
			returning attempt, delivery, duplicate
		`, post.ID, notice.EventID, keptDeliveryExpiredError)
		if err != nil {
			return nil, false, err
		}
		payloads, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MessageDeliveryEventPayload, error) {
			payload := model.MessageDeliveryEventPayload{
				MessageID: post.ID, Target: messageTarget, State: model.DeliveryStateFailed, Error: keptDeliveryExpiredError,
			}
			return payload, row.Scan(&payload.Attempt, &payload.Delivery, &payload.Duplicate)
		})
		if err != nil {
			return nil, false, err
		}
		for _, payload := range payloads {
			receipt := model.Event{IssueKey: issueKey, Type: "message.delivery", Actor: graphWriter, Payload: payload}
			appended, err := deps.Broker.Append(ctx, tx, receipt)
			if err != nil {
				return nil, false, fmt.Errorf("append the message.delivery receipt of message %s's attempt %d: %w", post.ID, payload.Attempt, err)
			}
			receipts = append(receipts, appended)
		}
		settled = len(payloads)
	default:
		// bus.ParseDispatchPostEventID answers no other kind.
		return nil, false, nil
	}
	slog.Info("dispatch outbox: a copy the listener kept of a delivery for a role ended with no session taking it; its unanswered attempts are settled undelivered",
		"kind", post.Kind, "post", post.ID, "role", notice.Role, "envelope_id", notice.EventID,
		"copy_sequence", notice.Sequence, "settled", settled)
	return receipts, true, nil
}

// noticedCopy reads the copy notice names back from reader, at the sequence it names on its role's
// kept lane (laneCopyAt), and answers why that is not the copy a listener dropped that the notice
// names - the copy of its event, or of its delivery's post, under its dedupe key - or "" when it
// may be. A listener stores a copy's notice after it reads the copy, and it notices only a copy
// Dispatch answers for, which decodes: one Dispatch's outbox kept, or one of Dispatch's delivery
// of a post that the publish route kept (cmd/listener, isOutboxCopy and isDispatchDeliveryCopy),
// so:
//
//   - the copy's sequence is below the notice's own (stored);
//   - a message the lane holds there is the notice's copy only when it decodes and is Dispatch's
//     copy under the notice's source event id and dedupe key (copyOfEvent). One no read can
//     decode, or another copy, is no copy the notice names, and the notice is passed over: no row
//     is written, so nothing is kept again, nothing is recorded undelivered and no attempt is
//     settled;
//   - a lane that holds nothing there lost the copy to the stream's retention only when the
//     sequence is below the first message the stream still holds. The stream removes its messages
//     oldest first by its age alone: it discards new messages at its byte limit
//     (nats.DiscardNew), so that limit removes none. This assumes the stream has no per-subject
//     limit (MaxMsgsPerSubject), which this repository's programs never set and which would remove
//     a subject's older messages mid-stream. That copy is gone, and the expiry pass ends its
//     pair over it (readExpiredCopy, copy_gone). A sequence at or past that first message where
//     the lane holds nothing - a message of another subject stands there, or none does - is one no
//     copy of the lane's expired at, and the notice is passed over.
//
// A failed read is returned, and the notice is read again on the next pass.
func noticedCopy(ctx context.Context, reader KeptLaneReader, stored bus.StoredMessage, notice bus.KeptCopyExpiry) (string, error) {
	if notice.Sequence == 0 || notice.Sequence >= stored.Sequence {
		return fmt.Sprintf("it names the copy at sequence %d, which is not below its own %d", notice.Sequence, stored.Sequence), nil
	}
	held, err := laneCopyAt(ctx, reader, notice.Role, notice.Sequence, notice.SourceEventID, notice.DedupeKey)
	if err != nil {
		return "", err
	}
	switch held.state {
	case laneCopyOfEvent:
		return "", nil
	case laneCopyUndecodable:
		return fmt.Sprintf("role %s's kept lane holds at sequence %d a message no read can decode, which is no copy Dispatch's outbox kept: %v",
			notice.Role, notice.Sequence, held.decodeErr), nil
	case laneCopyOther:
		return fmt.Sprintf("role %s's kept lane holds at sequence %d another copy than event %s's under dedupe key %s (event %q, source %q, dedupe key %q)",
			notice.Role, notice.Sequence, notice.SourceEventID, notice.DedupeKey, held.item.SourceEventID, held.item.Source, held.item.DedupeKey), nil
	}
	first, err := reader.NextStoredMessage(ctx, ">", 1)
	switch {
	case errors.Is(err, natsgo.ErrMsgNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the first message the stream holds: %w", err)
	case notice.Sequence < first.Sequence:
		return "", nil
	}
	return fmt.Sprintf("role %s's kept lane holds nothing at sequence %d, which is at or past the first message the stream holds (%d), so no copy of the lane's was dropped there",
		notice.Role, notice.Sequence, first.Sequence), nil
}

// laneCopyState is what a role's kept lane holds at one stream sequence, read against the copy of
// one event under one dedupe key (laneCopyAt).
type laneCopyState int

const (
	// laneCopyNone: the lane holds no message at that sequence.
	laneCopyNone laneCopyState = iota
	// laneCopyUndecodable: the lane holds a message there that no read can decode.
	laneCopyUndecodable
	// laneCopyOther: the lane holds a decoded message there that is not the event's copy under
	// the dedupe key.
	laneCopyOther
	// laneCopyOfEvent: the lane holds Dispatch's copy of the event under the dedupe key there.
	laneCopyOfEvent
)

// laneCopy is what laneCopyAt read: its state, the message's envelope where it decodes, and why it
// does not decode where it does not.
type laneCopy struct {
	state     laneCopyState
	item      contracts.Envelope
	decodeErr error
}

// laneCopyAt reads what role's kept lane holds at seq, against Dispatch's copy of the event whose
// id is eventID under dedupeKey (copyOfEvent). It is the one read of the copy at a sequence that
// the notice's record (noticedCopy) and the keep it owes (readExpiredCopy) both make, so both judge
// a copy by one rule. A failed read is returned.
func laneCopyAt(ctx context.Context, reader KeptLaneReader, role string, seq uint64, eventID, dedupeKey string) (laneCopy, error) {
	stored, err := reader.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
	if errors.Is(err, natsgo.ErrMsgNotFound) || err == nil && stored.Sequence != seq {
		return laneCopy{state: laneCopyNone}, nil
	}
	if err != nil {
		return laneCopy{}, fmt.Errorf("read role %s's kept lane at %d: %w", role, seq, err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		return laneCopy{state: laneCopyUndecodable, decodeErr: err}, nil
	}
	if !copyOfEvent(item, eventID, dedupeKey) {
		return laneCopy{state: laneCopyOther, item: item}, nil
	}
	return laneCopy{state: laneCopyOfEvent, item: item}, nil
}

// copyOfEvent reports whether item is Dispatch's copy of the event whose id is sourceEventID under
// dedupeKey: what the outbox kept for that event (eventEnvelope), or the copy of Dispatch's
// delivery of the post sourceEventID names (bus.DispatchPostEventID) the publish route kept, and
// what a notice of either names.
func copyOfEvent(item contracts.Envelope, sourceEventID, dedupeKey string) bool {
	return item.Source == "dispatch" && item.SourceEventID == sourceEventID && item.DedupeKey == dedupeKey
}

// routedToRole reports, in tx, whether the outbox ever routed eventID to role: the role's topic is
// one of the event's own destinations (published_destinations, where a batch records the role
// route its keep reached), or a fallback row of the event's comment names it
// (comment_fallback_copies, whose role topic rows hold the route's copy).
func routedToRole(ctx context.Context, tx pgx.Tx, eventID int64, role string) (bool, error) {
	var routed bool
	err := tx.QueryRow(ctx, `
		select exists (select 1 from events where id = $1 and $2 = any(published_destinations))
		    or exists (select 1 from comment_fallback_copies where event_id = $1 and destination = $2)
	`, eventID, contracts.RoleTopicPrefix+role).Scan(&routed)
	return routed, err
}

// recordExpiry records, in tx, that the copy notice names expired: one outbox_role_copy_expiries
// row, keyed by the event, the role and the attempt, so a repeated notice reopens the event's
// delivery to that role once.
//
// The row's attempt is the pair's next one: one more than the pair's rows. A copy of attempt n+1
// is kept only from row n (keepExpiredCopiesAgain), so a pair's rows run 1 to n with no gap, and
// the copy whose expiry this is - the latest keep, or a takeover's copy of a forward, kept under a
// key of its own - is the next to end. The notice carries the copy's own attempt
// (bus.KeptCopyAttempt), which the notice's design names; it is logged here and decides nothing.
//
// A notice of a copy already recorded, by its stream sequence on the role's kept lane, is that
// copy's repeat - another listener's store of the notice, outside the stream's duplicate window -
// and records nothing. A notice of an event already recorded undelivered to the role records
// nothing: the outbox has stopped for that pair.
//
// A row whose attempt is below maxRoleKeeps owes a keep of the next attempt, which
// keepExpiredCopiesAgain makes. A row at the bound is written settled undelivered, every earlier
// row of the pair still owed a keep is settled undelivered with it - two copies of one event, the
// copy kept again and a takeover's copy of it, can reach the bound in one read, and the earlier one
// must keep nothing more - and the event's role.undelivered record is appended on its own owner in
// the same transaction (endRoleDelivery), which the caller publishes once it has committed. The
// pass that keeps owed rows runs after this read in the same process (passKeptExpiries), and only
// in the hold's holder, so no keep of a row this settles is in flight while it does.
func recordExpiry(ctx context.Context, deps Deps, tx pgx.Tx, eventID int64, notice bus.KeptCopyExpiry) (*model.Event, error) {
	keptAt, droppedAt := time.UnixMilli(notice.KeptAt).UTC(), time.UnixMilli(notice.DroppedAt).UTC()
	attempts := slog.Group("expiry", "event_id", eventID, "role", notice.Role, "notice_attempt", notice.Attempt,
		"copy_sequence", notice.Sequence, "dedupe_key", notice.DedupeKey)
	var repeat, ended bool
	var attempt int
	if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(copy_sequence = $3), false), coalesce(bool_or(outcome = $4), false),
		       coalesce(max(keep_attempt), 0) + 1
		from outbox_role_copy_expiries where event_id = $1 and role = $2
	`, eventID, notice.Role, int64(notice.Sequence), expiryUndelivered).Scan(&repeat, &ended, &attempt); err != nil {
		return nil, err
	}
	if repeat {
		slog.Info("dispatch outbox: an expiry notice repeats one already recorded", attempts)
		return nil, nil
	}
	if ended {
		slog.Info("dispatch outbox: an expiry notice names an event already recorded undelivered to the role; nothing is kept again", attempts)
		return nil, nil
	}
	undelivered := attempt >= maxRoleKeeps
	tag, err := tx.Exec(ctx, `
		insert into outbox_role_copy_expiries
			(event_id, role, keep_attempt, copy_sequence, dedupe_key, kept_at, dropped_at, settled_at, outcome)
		values ($1, $2, $3, $4, $5, $6, $7, case when $8 then now() end, case when $8 then $9 end)
		on conflict (event_id, role, keep_attempt) do nothing
	`, eventID, notice.Role, attempt, int64(notice.Sequence), notice.DedupeKey, keptAt, droppedAt, undelivered, expiryUndelivered)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// Another process recorded this attempt first, in a transaction this one's read did not
		// see: that record stands, and this notice is its repeat.
		return nil, nil
	}
	if !undelivered {
		slog.Info("dispatch outbox: a kept copy's hour ended with no session taking it; the event is kept again",
			attempts, "keeps", attempt, "next_attempt", attempt+1)
		return nil, nil
	}
	return endRoleDelivery(ctx, deps, tx, model.RoleUndeliveredEventPayload{
		EventID: eventID, Role: notice.Role, Keeps: attempt, KeptAt: keptAt, DroppedAt: droppedAt, Reason: model.RoleUndeliveredExpired,
	})
}
