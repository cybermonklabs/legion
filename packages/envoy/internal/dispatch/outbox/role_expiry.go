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
	"github.com/sjawhar/envoy/internal/roles"
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
// holder is the event's own actor), passed over because the stream no longer holds its copy while
// another copy of the event still carries its delivery to the role (recordCopyGone), or recorded
// undelivered (role.undelivered).
const (
	expiryKept        = "kept"
	expiryForwarded   = "forwarded"
	expirySkipped     = "skipped"
	expiryGone        = "gone"
	expiryUndelivered = "undelivered"
)

// keptExpiryNotices is the filter every expiry notice matches (bus.KeptExpiredSubject).
const keptExpiryNotices = bus.KeptExpiredSubjectPrefix + ">"

// passKeptExpiries is the outbox's work on the copies it kept for roles whose hour ended with no
// session taking them: it reads the listeners' expiry notices into outbox_role_copy_expiries
// (readKeptExpiries), keeps each event again whose expiry owes it a keep, or forwards it to the
// session that holds the role by now (keepExpiredCopiesAgain), and posts the message about each
// window of events recorded undelivered that has ended (postUndeliveredNotes). It runs on the
// publisher hold's holder, after the fallback pass of each scan and every keptExpiryReadEvery
// besides (Outbox.serve), and reports what the holder does next as that pass does: full, there is
// more to read, keep or post now; failed, a read or a claim failed and the holder backs off.
//
// A Deps with no kept-lane reader reads no notice and keeps nothing again: the outbox has no
// stream to read the notices or the copies from, as it makes no keep's handover then
// (Deps.KeptLanes).
func (o *Outbox) passKeptExpiries(ctx context.Context, hold fence) (full, failed bool) {
	readFull, readFailed := readKeptExpiries(ctx, o.deps, hold, o.instance)
	keepFull, keepFailed := keepExpiredCopiesAgain(ctx, o.deps, hold, o.instance)
	postFull, postFailed := postUndeliveredNotes(ctx, o.deps, hold, o.instance)
	return readFull || keepFull || postFull, readFailed || keepFailed || postFailed
}

// readKeptExpiries reads the expiry notices the stream holds past the outbox's cursor
// (outbox_kept_expiry_cursor), batchSize of them at most, in stream order, and records each with
// the cursor's move past it in one transaction (recordKeptExpiry), so a notice is recorded once
// however often the read is made and whichever process makes it. It reports full when it read a
// whole batch, so the holder reads again at once, and failed when a read or a record failed: the
// cursor stands before that notice, and the next read takes it up again.
//
// Only the publisher hold's holder records a notice: each record's transaction reads the hold's
// row and writes nothing - no expiry, no record, no cursor move - while instance does not hold it
// (errNotTheHolder), and a pass past the hold's deadline reads nothing. So a process the hold has
// left cannot record an expiry the holder's own pass is about to keep again.
func readKeptExpiries(ctx context.Context, deps Deps, hold fence, instance uuid.UUID) (full, failed bool) {
	if deps.KeptLanes == nil || ctx.Err() != nil || !time.Now().Before(hold.Deadline()) {
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
		if err := recordKeptExpiry(ctx, deps, instance, stored); err != nil {
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

// errNotTheHolder is recordKeptExpiry's answer when the publisher hold's row does not name the
// process that would record the notice.
var errNotTheHolder = errors.New("the publisher hold is not this process's")

// holdsPublisherHold reports, in tx, whether the publisher hold's row names instance live on the
// database's clock, reading it with no lock. It is an expiry record's first statement, so a
// process the hold has already left does no work, and lockPublisherHold decides at commit.
func holdsPublisherHold(ctx context.Context, tx pgx.Tx, instance uuid.UUID) (bool, error) {
	var mine bool
	err := tx.QueryRow(ctx, `
		select exists (select 1 from duty_holds where name = $1 and holder = $2 and expires_at > now())
	`, publisherHold, instance).Scan(&mine)
	return mine, err
}

// lockPublisherHold reports, in tx, whether the publisher hold's row names instance live on the
// database's clock, and locks that row for share until tx ends. It is an expiry record's last
// statement before its commit. A takeover of the hold writes that row (duty's takeHold), so it
// waits for the commit: a record that found the hold its own at its end has written everything it
// writes before another process can hold the hold, and so before that process reads, posts or
// claims anything those writes touch. A record that finds the hold gone there rolls back. The
// lock is held only across the commit, never while the record waits on anything else - an event
// owner's lock that appendUndelivered takes, say - so the hold's renewal, which writes the same
// row, never waits behind such a wait: the hold is lost after its TTL less its margin (4 s) with
// no renewal, which is shorter than recordTimeout. Deps.holdLocked runs once the lock is held, for
// a test.
func lockPublisherHold(ctx context.Context, deps Deps, tx pgx.Tx, instance uuid.UUID) (bool, error) {
	var mine bool
	err := tx.QueryRow(ctx, `
		select holder = $2 and expires_at > now() from duty_holds where name = $1 for share
	`, publisherHold, instance).Scan(&mine)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if deps.holdLocked != nil {
		deps.holdLocked()
	}
	return mine, nil
}

// recordKeptExpiry records the expiry notice stored, and moves the cursor past it, in one
// transaction (recordExpiry) under recordTimeout, and publishes the role.undelivered event that
// record appended, if any, once it has committed. The transaction reads the publisher hold's row
// first (holdsPublisherHold) and locks it last, just before the commit (lockPublisherHold), and
// writes nothing and answers errNotTheHolder while that row does not name instance at either.
//
// A notice is passed over with one Error line, and only the cursor moves, when it names no copy
// this outbox kept for a role: its body does not decode; it names no source event or no attempt;
// its role is no role name (roles.Valid); it was stored on another role's expiry subject than the
// one its role names; or the event it names was never routed to that role - the event's own
// destinations, and its comment's fallback rows, do not name the role's topic - which an event
// the log does not hold is not either. No read of such a notice will ever name one. Only a NATS
// user on the listener's grant can store a notice at all, so this guards the role that becomes a
// subject (bus.KeptRoleSubject) and is written into a posted message, not a door.
func recordKeptExpiry(ctx context.Context, deps Deps, instance uuid.UUID, stored bus.StoredMessage) error {
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	var notice bus.KeptCopyExpiry
	decodeErr := json.Unmarshal(stored.Data, &notice)
	eventID, parseErr := strconv.ParseInt(notice.SourceEventID, 10, 64)
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if mine, err := holdsPublisherHold(ctx, tx, instance); err != nil {
		return err
	} else if !mine {
		return errNotTheHolder
	}
	var appended *model.Event
	refusal := errors.Join(decodeErr, parseErr)
	switch {
	case refusal != nil || notice.Attempt < 1:
	case !roles.Valid(notice.Role):
		refusal = fmt.Errorf("its role %q is no role name", notice.Role)
	case stored.Subject != bus.KeptExpiredSubject(notice.Role):
		refusal = fmt.Errorf("it was stored on %s, not on role %s's expiry subject", stored.Subject, notice.Role)
	default:
		routed, err := routedToRole(ctx, tx, eventID, notice.Role)
		if err != nil {
			return err
		}
		if !routed {
			refusal = fmt.Errorf("event %d was never routed to role %s", eventID, notice.Role)
		}
	}
	if refusal != nil || notice.Attempt < 1 {
		slog.Error("dispatch outbox: an expiry notice names no copy this outbox kept for a role; it is passed over",
			"sequence", stored.Sequence, "subject", stored.Subject, "event_id", notice.EventID, "role", notice.Role, "attempt", notice.Attempt, "error", refusal)
	} else if appended, err = recordExpiry(ctx, deps, tx, eventID, notice); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into outbox_kept_expiry_cursor (id, sequence) values (true, $1)
		on conflict (id) do update set sequence = greatest(outbox_kept_expiry_cursor.sequence, excluded.sequence)
	`, int64(stored.Sequence)); err != nil {
		return err
	}
	if mine, err := lockPublisherHold(ctx, deps, tx, instance); err != nil {
		return err
	} else if !mine {
		return errNotTheHolder
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if appended != nil {
		deps.Broker.Publish(*appended)
	}
	return nil
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
	return endRoleDelivery(ctx, deps, tx, eventID, notice.Role, attempt, keptAt, droppedAt, model.RoleUndeliveredExpired)
}

// endRoleDelivery is the one way an event's delivery to a role ends, in tx, for both reasons it
// ends - its last keep expired (recordExpiry) or no copy of it is left to keep (recordCopyGone):
// every row of the pair still owed a keep is settled undelivered, and the event's role.undelivered
// record is appended (appendUndelivered) unless the pair had ended already, so the pair has one
// record however many of its rows end, and no row of it is left owed. The caller has written its
// own row, the attempt'th, settled undelivered; ended reads whether any other row of the pair was
// undelivered before it.
func endRoleDelivery(ctx context.Context, deps Deps, tx pgx.Tx, eventID int64, role string, attempt int, keptAt, droppedAt time.Time, reason string) (*model.Event, error) {
	var ended bool
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from outbox_role_copy_expiries where event_id = $1 and role = $2 and outcome = $3 and keep_attempt <> $4)
	`, eventID, role, expiryUndelivered, attempt).Scan(&ended); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		update outbox_role_copy_expiries set settled_at = now(), outcome = $3
		where event_id = $1 and role = $2 and settled_at is null
	`, eventID, role, expiryUndelivered); err != nil {
		return nil, err
	}
	if ended {
		slog.Info("dispatch outbox: an event already recorded undelivered to a role ended again; it keeps its one record",
			"event_id", eventID, "role", role, "keeps", attempt, "reason", reason)
		return nil, nil
	}
	return appendUndelivered(ctx, deps, tx, eventID, role, attempt, keptAt, droppedAt, reason)
}

// appendUndelivered appends, in tx, the role.undelivered record of eventID's copy for role on that
// event's own owner - its issue, its document or its project - written by the outbox's own system
// actor (graphWriter), which notifies nobody (events.Broker.Notify), so the record wakes no route
// and reaches no role: it is published to its owner's topic alone, where a person reads it in the
// owner's history and a session that follows that topic is sent it. It also notes the event for
// the message a person is sent about it (noteUndelivered). It logs the record at Warn and returns
// it for the caller to publish once tx has committed.
//
// An event the log does not hold records nothing but an Error line, and so does one that names no
// owner. Every event the outbox keeps a copy of for a role is of an issue or of an issue's
// document: a role destination is the route (addRoute), which is read from the event's issue or
// from the issue of its document (readBatchSQL, coalesce(i.route, ai.route)), and a role topic's
// fallback row holds that route's copy (fallbackCopy.ofASessionsOwnTopic). So no write makes an
// expiry of a project's own event or of a project document's, and none of those owners has a
// place to post a message on.
func appendUndelivered(ctx context.Context, deps Deps, tx pgx.Tx, eventID int64, role string, keeps int, keptAt, droppedAt time.Time, reason string) (*model.Event, error) {
	record := model.Event{Type: model.RoleUndeliveredEventType, Actor: graphWriter}
	var about undeliveredEvent
	err := tx.QueryRow(ctx, `
		select e.issue_key, e.artifact_id::text, e.project_key, e.type, coalesce(e.payload ->> 'id', ''),
		       ar.id::text, ar.issue_key, coalesce(ar.slug, '')
		from events e
		left join artifacts ar on ar.id::text = coalesce(e.artifact_id::text, e.payload ->> 'artifact_id')
		where e.id = $1
	`, eventID).Scan(&record.IssueKey, &record.ArtifactID, &record.ProjectKey, &about.eventType, &about.postID,
		&about.documentID, &about.documentIssue, &about.documentSlug)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && record.IssueKey == nil && record.ArtifactID == nil && record.ProjectKey == nil {
		slog.Error("dispatch outbox: an event's copy for a role reached no session, and the event names no owner to record it on",
			"event_id", eventID, "role", role, "keeps", keeps, "reason", reason)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record.Payload = model.RoleUndeliveredEventPayload{
		EventID: eventID, EventType: about.eventType, Role: role, Keeps: keeps, KeptAt: keptAt, DroppedAt: droppedAt, Reason: reason,
	}
	appended, err := deps.Broker.Append(ctx, tx, record)
	if err != nil {
		return nil, fmt.Errorf("append the role.undelivered record of event %d: %w", eventID, err)
	}
	slog.Warn("dispatch outbox: an event's copy for a role reached no session; it is recorded undelivered",
		"event_id", eventID, "event_type", about.eventType, "role", role, "keeps", keeps, "reason", reason, "record_id", appended.ID)
	about.issueKey = record.IssueKey
	if err := noteUndelivered(ctx, tx, eventID, role, keeps, reason, about); err != nil {
		return nil, err
	}
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
// A send's answer is read by the rule the fallback pass reads its own by (awaitAnswer, judgeSend):
// a keep NATS refuses for good cannot be kept again, so its row goes the way of a gone copy
// (recordCopyGone, reason expired); a send the hold's deadline refused is released due at once; a
// send that found NATS unavailable is counted and released after retry.BaseDelay; and every other
// failure is counted as a failed attempt, logged at the dead-letter threshold (failedAttempt).
//
// It sends exactly the copy that expired - its envelope, dedupe key and mentions as the stream
// holds them - and writes nothing of the event's own row: its published_at and
// published_destinations stay as its batch left them, and no destination but the role is reached
// again, whoever the event's route, followers or authors name by now. A copy the stream no longer
// holds, or that no read can decode, cannot be kept again (recordCopyGone): its row settles gone
// while another copy of the event still carries its delivery to the role, and otherwise
// undelivered, with the event's role.undelivered record appended.
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
	claimed, err := claimExpiredCopies(ctx, deps, instance)
	if err != nil {
		slog.Error("dispatch outbox: claim the expired copies owed a keep", "error", err)
		return false, true
	}
	if len(claimed) == 0 {
		return false, false
	}
	full = len(claimed) == batchSize
	var records []expiryRecord
	// gone is every row whose copy cannot be kept again, with the reason its pair would end for:
	// a copy the stream no longer holds (copy_gone), and a keep NATS refused for good (expired:
	// each of its keeps ended its hour with no session taking it, and no further keep can be
	// stored). recordCopyGone ends the pair for that reason only when no other copy carries it.
	var gone []cannotKeep
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
		for _, row := range gone {
			if err := recordCopyGone(record, deps, instance, row.held, row.reason); err != nil {
				slog.Error("dispatch outbox: record an expired copy that cannot be kept again", "event_id", row.held.eventID, "role", row.held.role, "reason", row.reason, "error", err)
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
		failedAttempt(held.failures+1, "expired copy", "event_id", held.eventID, "role", held.role, "keep_attempt", held.attempt)
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
			gone = append(gone, cannotKeep{held: held, reason: model.RoleUndeliveredCopyGone})
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
		err := awaitAnswer(ctx, item.answer)
		switch judgeSend(ctx, deps, err, item.forward.keptFor == "", false) {
		case sendReached:
			if item.forward.keptFor != "" {
				records = append(records, expiryRecord{held: item.held, outcome: expiryKept})
			} else {
				records = append(records, expiryRecord{held: item.held, outcome: expiryForwarded})
			}
		case sendRefused:
			// No later send of this copy would be taken, so it cannot be kept again: its pair ends
			// as one whose copy is gone does, unless another copy carries it (recordCopyGone).
			slog.Error("dispatch outbox: an expired copy's keep was refused for good", "event_id", item.held.eventID, "role", item.held.role,
				"next_attempt", item.held.attempt+1, "error", err)
			gone = append(gone, cannotKeep{held: item.held, reason: model.RoleUndeliveredExpired})
		case sendNotSent:
			release(item.held, 0)
		case sendUnavailable:
			slog.Error("dispatch outbox: keep an expired copy again", "event_id", item.held.eventID, "role", item.held.role,
				"next_attempt", item.held.attempt+1, "error", err)
			release(item.held, retry.BaseDelay)
		default:
			slog.Error("dispatch outbox: keep an expired copy again", "event_id", item.held.eventID, "role", item.held.role,
				"next_attempt", item.held.attempt+1, "error", err)
			fail(item.held)
		}
	}
	return full, false
}

// cannotKeep is a claimed expiry whose copy cannot be kept again, and the reason its pair ends for
// if no other copy carries it (recordCopyGone).
type cannotKeep struct {
	held   expiredCopy
	reason string
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
// (fallbackClaimQuery). It claims nothing while the publisher hold's row does not name the
// claiming process, on the database's clock, and no row of an event already recorded undelivered
// to its role (recordExpiry settles those, and this keeps any it missed from being kept again).
const expiryClaimQuery = `
		with claimed as (
			update outbox_role_copy_expiries set next_attempt_at = now() + $2
			where (event_id, role, keep_attempt) in (
				select pending.event_id, pending.role, pending.keep_attempt from outbox_role_copy_expiries pending
				where pending.settled_at is null and (pending.next_attempt_at is null or pending.next_attempt_at <= now())
				  and exists (select 1 from duty_holds where name = $3 and holder = $4 and expires_at > now())
				  and not exists (
					select 1 from outbox_role_copy_expiries ended
					where ended.event_id = pending.event_id and ended.role = pending.role and ended.outcome = $5
				  )
				order by pending.received_at
				limit $1
				for update of pending skip locked
			)
			returning event_id, role, keep_attempt, copy_sequence, kept_at, dropped_at, attempt_count, next_attempt_at
		)
		select event_id, role, keep_attempt, copy_sequence, kept_at, dropped_at, attempt_count, next_attempt_at
		from claimed
	`

// claimExpiredCopies claims at most batchSize unsettled expiry rows that are due, for instance, in
// one statement run outside the pass's context under claimTimeout, as the fallback pass's claim is
// (claimFallbackCopies): a context that ended once the claim committed would lose the rows it
// claimed.
func claimExpiredCopies(ctx context.Context, deps Deps, instance uuid.UUID) ([]expiredCopy, error) {
	claim, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()
	rows, err := deps.Store.Pool.Query(claim, expiryClaimQuery, batchSize, fallbackClaim, publisherHold, instance, expiryUndelivered)
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

// recordCopyGone settles held, whose copy the stream no longer holds to keep again, in one
// transaction, and ends its event's delivery to its role only when no other copy of the event
// carries that delivery on. Another copy carries it when another row of the pair is still owed a
// keep - claimed in this pass or not, since its copy may still be read - or was settled kept,
// forwarded or skipped after held's expiry was recorded, which made a new copy or reached a session
// after held's copy had ended. Then held is settled gone and nothing else is written: the delivery
// goes on through that copy, which ends in its own expiry or in a session. Otherwise held is settled
// undelivered and the pair ends as the bound ends it (endRoleDelivery, model.RoleUndeliveredCopyGone):
// its other owed rows are settled and its role.undelivered record is appended once. The pass
// records its kept and forwarded rows before it records any gone one (keepExpiredCopiesAgain), so a
// sibling this pass delivered is read here.
//
// The transaction reads the publisher hold's row first (holdsPublisherHold), locks it last, just
// before the commit (lockPublisherHold), and is fenced on the
// row's claim as the pass's record is: a process the hold has left, or a row a later pass claimed,
// writes nothing. It publishes the record once that has committed. The caller runs it under
// recordTimeout.
func recordCopyGone(ctx context.Context, deps Deps, instance uuid.UUID, held expiredCopy, reason string) error {
	tx, err := deps.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if mine, err := holdsPublisherHold(ctx, tx, instance); err != nil || !mine {
		return err
	}
	var carried bool
	if err := tx.QueryRow(ctx, `
		select exists (
			select 1 from outbox_role_copy_expiries other, outbox_role_copy_expiries gone
			where gone.event_id = $1 and gone.role = $2 and gone.keep_attempt = $3
			  and other.event_id = $1 and other.role = $2 and other.keep_attempt <> $3
			  and (other.settled_at is null
			       or (other.outcome = any($4) and other.settled_at >= gone.received_at))
		)
	`, held.eventID, held.role, held.attempt, []string{expiryKept, expiryForwarded, expirySkipped}).Scan(&carried); err != nil {
		return err
	}
	outcome := expiryUndelivered
	if carried {
		outcome = expiryGone
	}
	tag, err := tx.Exec(ctx, `
		update outbox_role_copy_expiries set settled_at = now(), outcome = $5
		where event_id = $1 and role = $2 and keep_attempt = $3 and settled_at is null and next_attempt_at = $4
	`, held.eventID, held.role, held.attempt, held.claim, outcome)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	var appended *model.Event
	if carried {
		slog.Info("dispatch outbox: an expired copy that cannot be kept again is passed over; another copy of the event carries its delivery to the role",
			"event_id", held.eventID, "role", held.role, "attempt", held.attempt, "sequence", held.sequence, "reason", reason)
	} else if appended, err = endRoleDelivery(ctx, deps, tx, held.eventID, held.role, held.attempt, held.keptAt, held.droppedAt, reason); err != nil {
		return err
	}
	if mine, err := lockPublisherHold(ctx, deps, tx, instance); err != nil || !mine {
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
