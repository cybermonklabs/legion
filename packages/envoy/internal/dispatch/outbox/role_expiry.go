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
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// maxRoleKeeps is how many times the outbox keeps one event's copy for one role on that role's
// kept lane, each for the copy's hour (cmd/listener/role_keep.go, roleKeepWindow): the keep its
// batch made, and two more after a copy's hour ended with no session taking it. So a role routed
// an event gets three hours for a session to claim it - room for a launch, a project's pause or a
// wake held back for an hour or two, each of which has held a role's next holder off past one hour
// - and a role nobody will hold again, such as one whose work was abandoned, costs three copies of
// each event and no more: the expiry of the last one is recorded undelivered and nothing keeps the
// event again (recordExpiry).
const maxRoleKeeps = 3

// keptExpiryReadEvery is how often the publisher hold's holder reads the expiry notices when
// nothing else wakes it (Outbox.serve): the notices come from the listeners over NATS, which wakes
// no scan, and the outbox reads them from the stream with the stored-message read its grant
// already holds rather than a subscription of its own. The copy a notice names has waited an hour
// already, so half a minute more before it is kept again costs its next holder nothing a person
// could see, and one read of the stream every half minute costs the stream nothing.
const keptExpiryReadEvery = 30 * time.Second

// The outcomes an outbox_role_copy_expiries row is settled with: the copy kept again on the role's
// kept lane, forwarded to the session that held the role by then, reached with nothing sent (the
// holder is the event's own actor), or recorded undelivered (role.undelivered).
const (
	expiryKept        = "kept"
	expiryForwarded   = "forwarded"
	expirySkipped     = "skipped"
	expiryUndelivered = "undelivered"
)

// keptExpiryNotices is the filter every expiry notice matches (bus.KeptExpiredSubject).
const keptExpiryNotices = bus.KeptExpiredSubjectPrefix + ">"

// passKeptExpiries is the outbox's work on the copies it kept for roles whose hour ended with no
// session taking them: it reads the listeners' expiry notices into outbox_role_copy_expiries
// (readKeptExpiries), and keeps each event again whose expiry owes it a keep, or forwards it to the
// session that holds the role by now (keepExpiredCopiesAgain). It runs on the publisher hold's
// holder, after the fallback pass of each scan and every keptExpiryReadEvery besides
// (Outbox.serve), and reports what the holder does next as that pass does: full, there is more to
// read or keep now; failed, a read or a claim failed and the holder backs off.
//
// A Deps with no kept-lane reader reads no notice and keeps nothing again: the outbox has no
// stream to read the notices or the copies from, as it makes no keep's handover then
// (Deps.KeptLanes).
func (o *Outbox) passKeptExpiries(ctx context.Context, hold fence) (full, failed bool) {
	readFull, readFailed := readKeptExpiries(ctx, o.deps)
	keepFull, keepFailed := keepExpiredCopiesAgain(ctx, o.deps, hold, o.instance)
	return readFull || keepFull, readFailed || keepFailed
}

// readKeptExpiries reads the expiry notices the stream holds past the outbox's cursor
// (outbox_kept_expiry_cursor), batchSize of them at most, in stream order, and records each with
// the cursor's move past it in one transaction (recordKeptExpiry), so a notice is recorded once
// however often the read is made and whichever process makes it. It reports full when it read a
// whole batch, so the holder reads again at once, and failed when a read or a record failed: the
// cursor stands before that notice, and the next read takes it up again.
func readKeptExpiries(ctx context.Context, deps Deps) (full, failed bool) {
	if deps.KeptLanes == nil || ctx.Err() != nil {
		return false, false
	}
	var cursor int64
	if err := deps.Store.Pool.QueryRow(ctx,
		`select coalesce((select sequence from outbox_kept_expiry_cursor), 0)`).Scan(&cursor); err != nil {
		if !lostToContextEnd(ctx, err) {
			slog.Error("dispatch outbox: read the expiry notices' cursor", "error", err)
		}
		return false, true
	}
	after := uint64(cursor)
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
		if err := recordKeptExpiry(ctx, deps, stored); err != nil {
			if !lostToContextEnd(ctx, err) {
				slog.Error("dispatch outbox: record an expiry notice", "sequence", stored.Sequence, "error", err)
			}
			return false, true
		}
		after = stored.Sequence
	}
	return true, false
}

// recordKeptExpiry records the expiry notice stored, and moves the cursor past it, in one
// transaction (recordExpiry), and publishes the role.undelivered event that record appended, if
// any, once it has committed. A notice that names no event's copy - its body does not decode, or
// it names no source event, role or attempt - is passed over with one Error line: no read of it
// will ever name one.
func recordKeptExpiry(ctx context.Context, deps Deps, stored bus.StoredMessage) error {
	var notice bus.KeptCopyExpiry
	decodeErr := json.Unmarshal(stored.Data, &notice)
	eventID, parseErr := strconv.ParseInt(notice.SourceEventID, 10, 64)
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var appended *model.Event
	if err := errors.Join(decodeErr, parseErr); err != nil || notice.Role == "" || notice.Attempt < 1 {
		slog.Error("dispatch outbox: an expiry notice names no event's copy; it is passed over",
			"sequence", stored.Sequence, "event_id", notice.EventID, "role", notice.Role, "attempt", notice.Attempt, "error", err)
	} else if appended, err = recordExpiry(ctx, deps, tx, eventID, notice); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into outbox_kept_expiry_cursor (id, sequence) values (true, $1)
		on conflict (id) do update set sequence = greatest(outbox_kept_expiry_cursor.sequence, excluded.sequence)
	`, int64(stored.Sequence)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if appended != nil {
		deps.Broker.Publish(*appended)
	}
	return nil
}

// recordExpiry records, in tx, that the copy notice names expired: one outbox_role_copy_expiries
// row, keyed by the event, the role and the attempt the copy was kept for, so a repeated notice
// reopens the event's delivery to that role once.
//
// A notice of a copy already recorded, by its stream sequence on the role's kept lane, is that
// copy's repeat - another listener's store of the notice, outside the stream's duplicate window -
// and records nothing. A notice whose attempt is already recorded for another copy of the event is
// a later copy that names no attempt of its own - a takeover's copy of a forward, kept under a key
// of its own (bus.KeptCopyAttempt reads 1 for it) - and is recorded as the attempt after the last
// one recorded, so it neither reads as a repeat nor restarts the count.
//
// A row whose attempt is below maxRoleKeeps owes a keep of the next attempt, which
// keepExpiredCopiesAgain makes. A row at the bound is written settled undelivered, and the event's
// role.undelivered record is appended on its own owner in the same transaction
// (appendUndelivered), which the caller publishes once it has committed. A notice that names an
// event the log does not hold is passed over with one Error line.
func recordExpiry(ctx context.Context, deps Deps, tx pgx.Tx, eventID int64, notice bus.KeptCopyExpiry) (*model.Event, error) {
	keptAt, droppedAt := time.UnixMilli(notice.KeptAt).UTC(), time.UnixMilli(notice.DroppedAt).UTC()
	attempts := slog.Group("expiry", "event_id", eventID, "role", notice.Role, "attempt", notice.Attempt,
		"copy_sequence", notice.Sequence, "dedupe_key", notice.DedupeKey)
	var repeat, taken bool
	var next int
	if err := tx.QueryRow(ctx, `
		select coalesce(bool_or(copy_sequence = $3), false), coalesce(bool_or(keep_attempt = $4), false),
		       coalesce(max(keep_attempt), 0) + 1
		from outbox_role_copy_expiries where event_id = $1 and role = $2
	`, eventID, notice.Role, int64(notice.Sequence), notice.Attempt).Scan(&repeat, &taken, &next); err != nil {
		return nil, err
	}
	if repeat {
		slog.Info("dispatch outbox: an expiry notice repeats one already recorded", attempts)
		return nil, nil
	}
	attempt := notice.Attempt
	if taken {
		attempt = next
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
	return appendUndelivered(ctx, deps, tx, eventID, notice.Role, attempt, keptAt, droppedAt, model.RoleUndeliveredExpired)
}

// appendUndelivered appends, in tx, the role.undelivered record of eventID's copy for role on that
// event's own owner - its issue, its document or its project - written by the outbox's own system
// actor (graphWriter), which notifies nobody (events.Broker.Notify), so the record wakes no route
// and reaches no role: it is published to its owner's topic alone, where a person reads it in the
// owner's history and a session that follows that topic is sent it. It logs the record at Warn and
// returns it for the caller to publish once tx has committed. An event the log does not hold, and
// one that names no owner, which the outbox never keeps a copy of, records nothing but that line.
func appendUndelivered(ctx context.Context, deps Deps, tx pgx.Tx, eventID int64, role string, keeps int, keptAt, droppedAt time.Time, reason string) (*model.Event, error) {
	record := model.Event{Type: model.RoleUndeliveredEventType, Actor: graphWriter}
	var eventType string
	err := tx.QueryRow(ctx, `select issue_key, artifact_id::text, project_key, type from events where id = $1`, eventID).
		Scan(&record.IssueKey, &record.ArtifactID, &record.ProjectKey, &eventType)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && record.IssueKey == nil && record.ArtifactID == nil && record.ProjectKey == nil {
		slog.Error("dispatch outbox: an event's copy for a role reached no session, and the event names no owner to record it on",
			"event_id", eventID, "role", role, "keeps", keeps, "reason", reason)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.Payload = model.RoleUndeliveredEventPayload{
		EventID: eventID, EventType: eventType, Role: role, Keeps: keeps, KeptAt: keptAt, DroppedAt: droppedAt, Reason: reason,
	}
	appended, err := deps.Broker.Append(ctx, tx, record)
	if err != nil {
		return nil, fmt.Errorf("append the role.undelivered record of event %d: %w", eventID, err)
	}
	slog.Warn("dispatch outbox: an event's copy for a role reached no session; it is recorded undelivered",
		"event_id", eventID, "event_type", eventType, "role", role, "keeps", keeps, "reason", reason, "record_id", appended.ID)
	return &appended, nil
}

// expiredCopy is one outbox_role_copy_expiries row the expiry pass claimed: the copy of eventID
// kept for role for the attempt'th time, which the stream holds at sequence on role's kept lane,
// the keep and drop times its notice named, how many of the pass's sends of it failed so far
// (attempt_count), and claim, the next_attempt_at the pass's claim set, which every write of the
// pass's record is guarded on.
type expiredCopy struct {
	eventID   int64
	role      string
	attempt   int
	sequence  uint64
	keptAt    time.Time
	droppedAt time.Time
	failures  int
	claim     time.Time
}

// expiryRecord is what the expiry pass came to for one row: outcome when it settled it, failed
// when its send failed, which counts an attempt and backs the row off (retry.Delay), and otherwise
// a release of its claim for delay, counting nothing.
type expiryRecord struct {
	held    expiredCopy
	outcome string
	failed  bool
	delay   time.Duration
}

// keepExpiredCopiesAgain is the expiry pass. It claims, under the publisher hold, the expiries that
// owe a keep (expiryClaimQuery), reads the copy each names back from the role's kept lane, resolves
// the role's holder as a batch's role route does (resolveRole, forwardOf) and sends it:
//
//   - a role no live holder can take keeps the copy again, for the next attempt
//     (roleForward.keepAttempt, bus.KeptAttemptMsgID), with the settled mentions it carried, and
//     the handover behind that keep hands it to a holder that claimed the role meanwhile
//     (roleForward.keep, handOverKept); the row settles kept once the stream acknowledged it;
//   - a live holder is forwarded the copy, keyed by the one rule every forwarder of a role message
//     follows (roleForward.forward), and the row settles forwarded;
//   - a holder that is the event's own actor is reached with nothing sent, and the row settles
//     skipped;
//   - a holder that is not answering, or a resolution that failed, sends nothing: the row counts an
//     attempt and backs off, as a batch's pending forward does, and is decided again when due.
//
// It sends exactly the copy that expired - its envelope, dedupe key and mentions as the stream
// holds them - and writes nothing of the event's own row: its published_at and
// published_destinations stay as its batch left them, and no destination but the role is reached
// again, whoever the event's route, followers or authors name by now. A copy the stream no longer
// holds, or that no read can decode, cannot be kept again: the row settles undelivered and the
// event's role.undelivered record is appended (recordCopyGone).
//
// The pass is fenced by the publisher hold as the fallback pass is (publishFallbackCopies): it
// claims nothing once the hold's deadline has passed, its sends go through the hold's window, and
// its record is written only while the hold is still this process's, on the database's clock
// (recordExpiredCopies). A pass that lost the hold records nothing: each row keeps its claim,
// falls due once that lapses, and the holder's next pass keeps the copy again under the same
// message id, which the stream holds once, or forwards it under the same one, which the stream
// and the holder each take once. It reports what the holder does next, as the fallback pass does.
func keepExpiredCopiesAgain(ctx context.Context, deps Deps, hold fence, instance uuid.UUID) (full, failed bool) {
	if deps.KeptLanes == nil || ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
		return false, false
	}
	claimed, err := claimExpiredCopies(ctx, deps)
	if err != nil {
		slog.Error("dispatch outbox: claim the expired copies owed a keep", "error", err)
		return false, true
	}
	if len(claimed) == 0 {
		return false, false
	}
	full = len(claimed) == batchSize
	var records []expiryRecord
	var gone []expiredCopy
	// Written even when ctx has ended, as the fallback pass's record is: what NATS accepted is
	// recorded and what the pass did not decide is released.
	defer func() {
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
		defer cancel()
		switch recorded, err := recordExpiredCopies(record, deps, instance, records); {
		case err != nil:
			slog.Error("dispatch outbox: record the expired copies' keeps", "error", err)
		case !recorded:
			slog.Warn("dispatch outbox: the publisher hold is no longer this process's, so the expiry pass was not recorded and the rows it claimed are left to the hold's next pass",
				"copies", len(records))
		}
		for _, held := range gone {
			if err := recordCopyGone(record, deps, instance, held); err != nil {
				slog.Error("dispatch outbox: record an expired copy the stream no longer holds", "event_id", held.eventID, "role", held.role, "error", err)
			}
		}
	}()
	release := func(held expiredCopy, delay time.Duration) {
		records = append(records, expiryRecord{held: held, delay: delay})
	}
	fail := func(held expiredCopy) {
		if ctx.Err() != nil {
			release(held, 0)
			return
		}
		records = append(records, expiryRecord{held: held, failed: true})
	}
	type sentKeep struct {
		held    expiredCopy
		forward *roleForward
		answer  <-chan error
	}
	sender := deps.Publisher.WindowUntil(hold.Deadline)
	handed := newKeptHandovers()
	var sent []sentKeep
	for _, held := range claimed {
		if ctx.Err() != nil {
			release(held, 0)
			continue
		}
		item, ok, err := readExpiredCopy(ctx, deps, held)
		switch {
		case err != nil:
			if !lostToContextEnd(ctx, err) {
				slog.Error("dispatch outbox: read an expired copy back", "event_id", held.eventID, "role", held.role, "sequence", held.sequence, "error", err)
			}
			fail(held)
			continue
		case !ok:
			gone = append(gone, held)
			continue
		}
		resolution, err := resolveRole(ctx, deps, held.role)
		forward := forwardOf(held.role, item, resolution, err, time.Now())
		forward.settledKeys, forward.keepAttempt = item.MentionDedupeKeys, held.attempt+1
		switch {
		case forward.skip != "":
			records = append(records, expiryRecord{held: held, outcome: expirySkipped})
		case forward.err != nil:
			deps.countRoleForwardHeld(forward.heldState)
			slog.Info("dispatch outbox: an expired copy waits for its role's holder",
				"event_id", held.eventID, "role", held.role, "holder", forward.holder, "holder_state", forward.heldState, "error", forward.err)
			fail(held)
		default:
			sent = append(sent, sentKeep{held: held, forward: forward, answer: forward.send(ctx, deps, sender, item, handed)})
		}
	}
	for _, item := range sent {
		var err error
		select {
		case err = <-item.answer:
		default:
			select {
			case err = <-item.answer:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		switch {
		case err == nil && item.forward.keptFor != "":
			records = append(records, expiryRecord{held: item.held, outcome: expiryKept})
		case err == nil:
			records = append(records, expiryRecord{held: item.held, outcome: expiryForwarded})
		case errors.Is(err, bus.ErrWindowDeadline):
			release(item.held, 0)
		case errors.Is(err, bus.ErrUnavailable):
			release(item.held, retry.BaseDelay)
		default:
			switch {
			case errors.Is(err, bus.ErrPublishDenied):
				deps.countPublishFailure("denied")
			case errors.Is(err, bus.ErrRefused):
				deps.countPublishFailure("refused")
			case forwardUnanswered(item.forward.keptFor == "", err):
				deps.countPublishFailure("unanswered")
			case !lostToContextEnd(ctx, err):
				deps.countPublishFailure("failed")
			}
			slog.Error("dispatch outbox: keep an expired copy again", "event_id", item.held.eventID, "role", item.held.role,
				"next_attempt", item.held.attempt+1, "error", err)
			fail(item.held)
		}
	}
	return full, false
}

// readExpiredCopy reads held's copy back from its role's kept lane, at the stream sequence its
// notice named, and decodes it. It reports false with no error when the stream no longer holds a
// copy there - the stream's retention passed it - or holds one no read can decode, either of which
// the pass cannot keep again.
func readExpiredCopy(ctx context.Context, deps Deps, held expiredCopy) (contracts.Envelope, bool, error) {
	stored, err := deps.KeptLanes.NextStoredMessage(ctx, bus.KeptRoleSubject(held.role), held.sequence)
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return contracts.Envelope{}, false, nil
	}
	if err != nil {
		return contracts.Envelope{}, false, err
	}
	var item contracts.Envelope
	if stored.Sequence != held.sequence {
		return contracts.Envelope{}, false, nil
	}
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		slog.Error("dispatch outbox: an expired copy cannot be decoded", "event_id", held.eventID, "role", held.role, "sequence", held.sequence, "error", err)
		return contracts.Envelope{}, false, nil
	}
	return item, true, nil
}

// expiryClaimQuery is claimExpiredCopies's one statement: the due unsettled rows, oldest first,
// claimed for fallbackClaim under for update skip locked, as the fallback pass claims its rows
// (fallbackClaimQuery).
const expiryClaimQuery = `
		with claimed as (
			update outbox_role_copy_expiries set next_attempt_at = now() + $2
			where (event_id, role, keep_attempt) in (
				select event_id, role, keep_attempt from outbox_role_copy_expiries
				where settled_at is null and (next_attempt_at is null or next_attempt_at <= now())
				order by received_at
				limit $1
				for update skip locked
			)
			returning event_id, role, keep_attempt, copy_sequence, kept_at, dropped_at, attempt_count, next_attempt_at
		)
		select event_id, role, keep_attempt, copy_sequence, kept_at, dropped_at, attempt_count, next_attempt_at
		from claimed
	`

// claimExpiredCopies claims at most batchSize unsettled expiry rows that are due, in one statement
// run outside the pass's context under claimTimeout, as the fallback pass's claim is
// (claimFallbackCopies): a context that ended once the claim committed would lose the rows it
// claimed.
func claimExpiredCopies(ctx context.Context, deps Deps) ([]expiredCopy, error) {
	claim, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()
	rows, err := deps.Store.Pool.Query(claim, expiryClaimQuery, batchSize, fallbackClaim)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (expiredCopy, error) {
		var held expiredCopy
		var sequence int64
		if err := row.Scan(&held.eventID, &held.role, &held.attempt, &sequence, &held.keptAt, &held.droppedAt,
			&held.failures, &held.claim); err != nil {
			return expiredCopy{}, err
		}
		held.sequence = uint64(sequence)
		return held, nil
	})
}

// expiryRecordQuery is recordExpiredCopies's one statement, fenced on the publisher hold and on
// each row's claim as fallbackRecordQuery is.
const expiryRecordQuery = `
		with hold as (
			select exists (
				select 1 from duty_holds where name = $9 and holder = $8 and expires_at > now()
			) as mine
		), outcome as (
			select * from unnest($1::bigint[], $2::text[], $3::int[], $4::timestamptz[], $5::text[], $6::interval[], $7::bool[])
				as o(event_id, role, keep_attempt, claim, outcome, retry_after, counted)
		), recorded as (
			update outbox_role_copy_expiries x set
				settled_at = case when o.outcome is not null then now() end,
				outcome = o.outcome,
				attempt_count = x.attempt_count + case when o.counted then 1 else 0 end,
				next_attempt_at = case when o.outcome is null then now() + o.retry_after end
			from outcome o
			cross join hold h
			where x.event_id = o.event_id and x.role = o.role and x.keep_attempt = o.keep_attempt
				and x.settled_at is null and x.next_attempt_at = o.claim and h.mine
		)
		select mine from hold
	`

// recordExpiredCopies writes what an expiry pass came to, in one statement: each settled row with
// its outcome, each failed row with one more attempt and a wait of retry.Delay of its new count,
// and each released row with its claim replaced by the wait it was released with, or cleared when
// that wait is none. It reports whether instance holds the publisher hold still, and writes no row
// when it does not, as recordFallbackCopies writes none.
func recordExpiredCopies(ctx context.Context, deps Deps, instance uuid.UUID, records []expiryRecord) (bool, error) {
	if len(records) == 0 {
		return true, nil
	}
	eventIDs := make([]int64, 0, len(records))
	roles := make([]string, 0, len(records))
	attempts := make([]int, 0, len(records))
	claims := make([]time.Time, 0, len(records))
	outcomes := make([]*string, 0, len(records))
	retryAfter := make([]*time.Duration, 0, len(records))
	counted := make([]bool, 0, len(records))
	for _, record := range records {
		eventIDs = append(eventIDs, record.held.eventID)
		roles = append(roles, record.held.role)
		attempts = append(attempts, record.held.attempt)
		claims = append(claims, record.held.claim)
		counted = append(counted, record.failed)
		if record.outcome != "" {
			outcomes = append(outcomes, new(record.outcome))
		} else {
			outcomes = append(outcomes, nil)
		}
		switch {
		case record.failed:
			retryAfter = append(retryAfter, new(retry.Delay(record.held.failures+1)))
		case record.delay > 0:
			retryAfter = append(retryAfter, new(record.delay))
		default:
			retryAfter = append(retryAfter, nil)
		}
	}
	var holds bool
	if err := deps.Store.Pool.QueryRow(ctx, expiryRecordQuery,
		eventIDs, roles, attempts, claims, outcomes, retryAfter, counted, instance, publisherHold).Scan(&holds); err != nil {
		return false, err
	}
	return holds, nil
}

// recordCopyGone settles held undelivered, with the event's role.undelivered record appended
// beside it (appendUndelivered, model.RoleUndeliveredCopyGone), in one transaction fenced on the
// publisher hold and on the row's claim as the pass's record is: a process the hold has left, or a
// row a later pass claimed, writes nothing. It publishes the record once that has committed.
func recordCopyGone(ctx context.Context, deps Deps, instance uuid.UUID, held expiredCopy) error {
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update outbox_role_copy_expiries set settled_at = now(), outcome = $6
		where event_id = $1 and role = $2 and keep_attempt = $3 and settled_at is null and next_attempt_at = $4
		  and exists (select 1 from duty_holds where name = $7 and holder = $5 and expires_at > now())
	`, held.eventID, held.role, held.attempt, held.claim, instance, expiryUndelivered, publisherHold)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	appended, err := appendUndelivered(ctx, deps, tx, held.eventID, held.role, held.attempt, held.keptAt, held.droppedAt, model.RoleUndeliveredCopyGone)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if appended != nil {
		deps.Broker.Publish(*appended)
	}
	return nil
}
