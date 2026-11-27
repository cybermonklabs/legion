package legion

// apply.go holds the intake's apply path of one state message (dispatch://INFRA-3931): the message
// across the attempts its runner makes at it (messageAttempt, deliveryOf); one attempt, its batch
// and the read the batch calls for (applyMessage, applyBatch, answerRead); what the message comes
// to on its project's position (judge, ownRead); the read a message of a store the cursor drops
// calls for (callRead); and the read a message the database refused for good calls for
// (readRefused, refusedForGood). The intake's lifecycle, its wiring and each message's delivery
// are in intake.go, the cursor and the read rule in cursor.go, a message past stateMaxWaiting in
// crowded.go, and the whole read in whole_read.go.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	natsgo "github.com/nats-io/nats.go"
)

// callRead is the read a message that project's cursor finishes unapplied as a message of a store
// it drops (cursor.finishesUnapplied) calls for, on current, the cursor its batch holds
// (applyBatch): the message has waited on the project's runner behind any read in flight, so
// current is the cursor that read left. The message calls for no read of its own, but it may be
// the setting's own daemon's: until a good read of that daemon has given the cursor a store, no
// read has shown whether it serves another store or the same store behind another URL, and a
// daemon the cursor follows in another store can be redeployed into the store a write recorded
// (previousStateID) with no write of the setting, after which each of its messages is of a store
// the cursor drops (read-rule-cases row 4.3). Its id is the old daemon's, so the intake cannot
// tell the two apart. So where a call no message makes would make a read now (cursor.readFor: an
// owed read that is due and that no message holds), whatever store the cursor holds, callRead
// queues the project's starting point on the project's runner behind the message (startingPoint),
// which makes that read as such a call. Such a read is owed already, and the read rule makes it
// at most once a minute, so an old daemon that still publishes adds no read the project does not
// owe. The message does not wait for it, never holds it, and is acknowledged unapplied at once;
// the read rule starts the project's whole reads ReadRetryAfter apart. At most one such call waits
// per project (calling), until it has run, so the messages of such a store that the runner judges
// meanwhile join it and queue none: a read the daemon never answers lasts RequestTimeout, which is
// ReadRetryAfter, so each call queued behind it would find the read due again and make one more,
// back to back on the project's runner.
func (in *Intake) callRead(project string, current cursor) {
	if !current.readFor("", in.now(), 0, false, nil).read {
		return
	}
	in.mu.Lock()
	if in.calling[project] {
		in.mu.Unlock()
		return
	}
	in.calling[project] = true
	in.mu.Unlock()
	called := func() {
		in.mu.Lock()
		delete(in.calling, project)
		in.mu.Unlock()
	}
	if !in.durable.Queue(project, func(ctx context.Context) {
		defer called()
		in.startingPoint(ctx, project, false)
	}) {
		called()
	}
}

// messageAttempt is one message on its runner, across the attempts untilDone makes at it.
type messageAttempt struct {
	message stateMessage
	bytes   int
	msg     *natsgo.Msg
	// seq is the message's sequence in the stream, which names it as the message that holds its
	// project's owed read (cursor.readHolder), and delivered how many times this durable has
	// delivered it, which the log lines name: a take-over creates the durable again and starts it
	// again at 1, so the hold's own count is the cursor's (stateMaxHolds). Both are read off the
	// message's JetStream metadata (deliveryOf).
	seq       int64
	delivered uint64
	// pending is a whole read this message made that is not recorded yet (readWhole), readMade
	// whether one was recorded (makeRead), after which the message calls for no read of its own
	// (ownRead) and its delivery makes no other read (cursor.readFor), and readGood whether that
	// read was a good one, after which a message still past the next sequence is no event of the
	// daemon read (judge).
	pending  *wholeRead
	readMade bool
	readGood bool
	// cursor is the project's cursor as the last batch read it (applyBatch, judgeCrowded), and
	// reason the whole read that batch found the message calls for (judge, judgeCrowded), or the
	// read a message the database refused calls for (readRefused), which the log line of a message
	// held or acknowledged unapplied names.
	cursor cursor
	reason string
	// back is how long the message goes back to the stream for, unacknowledged: above zero where it
	// holds its project's owed read, and 0 where it is acknowledged. released is whether it went
	// back stateMaxHolds times and the cursor no longer names it as the read's holder. holds is
	// which of the hold's hand-backs the cursor counts this one as, where it goes back, and how
	// many it counted, where it is released. answered sets the three.
	back     time.Duration
	released bool
	holds    int
	// refusal is the database's refusal for good of the message's batch, once there is one: the
	// message is not tried again, and one whole read is made of its project (readRefused).
	refusal *pgconn.PgError
}

// answered sets the message's back, released and holds from answer, the read rule's answer for it
// (cursor.readFor, through answerRead or judgeCrowded). applyBatch, readRefused and judgeCrowded
// call it once the write that recorded answer has committed, and never before.
func (a *messageAttempt) answered(answer readAnswer) {
	a.back, a.released, a.holds = answer.back, answer.release, answer.holds
}

// deliveryOf is msg's sequence in the stream (bus.Stream), which tells the message that holds its
// project's owed read from every other one (cursor.readHolder), and how many times this durable
// has delivered it (NumDelivered), which the log lines name. It is 0 and 0 for a message with no
// JetStream metadata, which no read rule holds: no delivery of the durable is one.
func deliveryOf(msg *natsgo.Msg) (seq int64, delivered uint64) {
	meta, err := msg.Metadata()
	if err != nil {
		return 0, 0
	}
	return int64(meta.Sequence.Stream), meta.NumDelivered
}

// applyMessage is one attempt at a message: its batch (applyBatch), or the whole read the batch
// found is to be made first (cursor.readFor). The read waits on the daemon, so it is made outside
// the batch's transaction (makeRead), and the attempt is then made again: the batch decides once
// more on the cursor the read left. A read that is owed and not due is recorded owed by the batch
// (answerRead), which takes the message or leaves it unapplied as judge says, and the message goes
// back to the stream where it holds the owed read (messageAttempt.back). A read that startBlocked
// blocks ends the attempt as that rule says (outcomeEarlierBuild). A batch the database refuses
// for good (refusedForGood) is not tried again: it calls for one whole read of the project
// (readRefused), and deliver logs the refusal.
func (in *Intake) applyMessage(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	if attempt.refusal != nil {
		return in.readRefused(ctx, attempt)
	}
	result, read, err := in.applyBatch(ctx, attempt)
	if refusal := refusedForGood(err); refusal != nil {
		attempt.refusal = refusal
		return in.readRefused(ctx, attempt)
	}
	if err != nil || read == nil {
		return result, err
	}
	if blocked, err := in.startBlocked(ctx, read.reason); err != nil || blocked {
		if blocked && attempt.cursor.heldBy(attempt.seq) {
			err = owe(ctx, in.pool, attempt.message.Project, readAnswer{release: true}, attempt.seq)
		}
		return outcomeEarlierBuild, err
	}
	return in.makeRead(ctx, attempt, *read)
}

// readRefused is the whole read a message the database refused for good calls for
// (ReadReasonRefused): the read's rows then hold the daemon's state, the refused message's events
// among it, since the daemon commits each event before it publishes it. The read rule decides it as
// it decides every read a message calls for (answerRead), and what the rule owes is recorded
// through the pool, since no batch is open. A read that is due is made (makeRead), and readRefused
// then decides again on the cursor the read left, the message calling for no read of its own once
// one was made for it (ownRead). A read that is not due is recorded owed, and once that is written
// the message goes back to the stream where it holds the owed read, and is acknowledged otherwise
// (messageAttempt.answered). Nothing is read where the project has no setting, or a setting whose
// starting point its change queues (a cursor that does not follow it), or where the cursor
// finishes the message unapplied (cursor.finishesUnapplied); a holder of a store the cursor drops
// calls for no reason (cursor.drops). Nothing is read either where the database refuses for good
// the read's record as well, the record of the owed read, or the project's position itself (a key
// it does not take): the message is acknowledged all the same. Any other failure of the database
// is tried again in place, as every database step is.
func (in *Intake) readRefused(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	setting, current, found, err := in.position(ctx, attempt.message.Project)
	finishes, dropped := current.finishesUnapplied(attempt.message, setting.StateURL, attempt.seq)
	if err != nil || !found || !current.follows(setting.StateURL) || finishes {
		if refusedForGood(err) != nil {
			err = nil
		}
		return outcomeDone, err
	}
	attempt.reason = ownRead(ReadReasonRefused, attempt.readMade)
	if dropped {
		attempt.reason = ""
	}
	answer, err := in.answerRead(ctx, in.pool, attempt, current, attempt.reason)
	if err != nil {
		if refusedForGood(err) != nil {
			err = nil
		}
		return outcomeDone, err
	}
	if !answer.read {
		attempt.answered(answer)
		return outcomeDone, nil
	}
	result, err := in.makeRead(ctx, attempt, answer.of(setting))
	if refusedForGood(err) != nil {
		return outcomeDone, nil
	}
	return result, err
}

// refusedForGood is the Postgres error err carries where it is one no retry can write: SQLSTATE
// class 22, a data exception (a value its column's type does not take), or class 23, an
// integrity constraint violation. nil for every other error, a lost connection, a lock timeout
// or a statement a trigger raised among them, which a retry may write.
func refusedForGood(err error) *pgconn.PgError {
	var refusal *pgconn.PgError
	if errors.As(err, &refusal) && (strings.HasPrefix(refusal.Code, "22") || strings.HasPrefix(refusal.Code, "23")) {
		return refusal
	}
	return nil
}

// answerRead runs the read rule (cursor.readFor) on current for reason, the whole read the
// attempt's message calls for, and answers what the rule decides. A read made now for a message of
// another store carries that message's store (readAnswer.store). Where the rule makes no read now,
// answerRead first records what the rule owes (owe) through db: the batch's transaction
// (applyBatch), or the pool (readRefused). The caller sets the message's answer from it afterwards
// (messageAttempt.answered).
func (in *Intake) answerRead(ctx context.Context, db Execer, attempt *messageAttempt, current cursor, reason string) (readAnswer, error) {
	answer := current.readFor(reason, in.now(), attempt.seq, attempt.readMade, nil)
	if answer.read {
		if reason == ReadReasonStore {
			answer.store = attempt.message.StateID
		}
		return answer, nil
	}
	if err := owe(ctx, db, attempt.message.Project, answer, attempt.seq); err != nil {
		return readAnswer{}, err
	}
	return answer, nil
}

// judge is what a message comes to on the position its project's transaction holds: the cursor
// and the parts of a split transaction held for it. It answers the outcome of the message where no
// whole read is made first, outcomeDone for a message whose events above the position are taken,
// and the reason of the whole read the message calls for, "" for none, which applyBatch hands to
// the read rule (answerRead). A message whose read was made (readMade) is judged again on the
// cursor the read left. judge answers every reason but ReadReasonStart through ownRead, so that
// message calls for no read of its own other than a starting point:
//
//   - A message of a store the cursor drops (cursor.drops), the store recorded by a write that
//     named another daemon or by a delete of the setting, or a store marked as no daemon of the
//     project, is never applied, whatever else holds (outcomeForgotten), and calls for no read
//     here, not even a starting point: the setting's daemon may not have published it, and R3
//     says a failed starting read is one that no message holds. What it does from there is
//     drops' rule, which applyBatch follows through cursor.finishesUnapplied.
//   - A project with no starting point calls for a read (ReadReasonStart). It is due
//     ReadRetryAfter after the project's newest whole read began, as every read is, and where it
//     is not due it is owed and held as every read is (cursor.readFor). Nothing of the message
//     is applied where no read is made first (outcomeNoStore): no read since the write has said
//     which store the daemon the setting names counts in, and the starting read holds the
//     message's events.
//   - A cursor that holds no store, one whose every read since the setting was written failed,
//     takes no message (outcomeNoStore), since no good read has said which store the daemon the
//     setting names counts in. The message may be of the daemon the setting named before, which
//     still publishes under the same project, and R3 applies none of its messages after the
//     setting names another. The read owed since the last one failed holds the message's events.
//   - A message whose stateId is not the cursor's is counted in another store, and nothing of it
//     is applied (outcomeOtherStore). It calls for a read (ReadReasonStore), so a daemon whose
//     store was replaced is found again.
//   - After a good whole read the message called for (readGood), a message whose first sequence
//     is still past the next one is no event of the daemon read: the daemon commits each event
//     before it publishes it, so the read holds every event published before it (outcomeBeyond),
//     whatever reason a message past the cap added while the read was in flight (judgeCrowded).
//     Any other message is taken.
//   - Otherwise a message that does not follow the position (stateMessage.unfollowed: a gap, a
//     boot that does not follow, a part that does not continue the held parts) calls for a read,
//     and where no read is made first it is finished unapplied (outcomeUnfollowed). The read holds
//     its events, and taking it would move the cursor past events never applied: R1 applies no
//     message beyond the starting point's stateSeq and what the stream has delivered since. A boot
//     that follows the position and whose schema version moved (stateMessage.movesSchema) calls
//     for a read (ReadReasonSchema) and is taken, as is every message that follows.
func judge(current cursor, held []part, stateURL string, message stateMessage, readMade, readGood bool) (outcome, string) {
	if current.drops(message, stateURL) {
		return outcomeForgotten, ""
	}
	if !current.follows(stateURL) {
		return outcomeNoStore, ReadReasonStart
	}
	result, reason := outcomeDone, ""
	switch {
	case current.stateID == nil:
		result = outcomeNoStore
	case *current.stateID != message.StateID:
		result, reason = outcomeOtherStore, ReadReasonStore
	case readGood:
		if current.lastSeq != nil && message.FirstSeq > *current.lastSeq+1 {
			result = outcomeBeyond
		}
	default:
		reason = message.unfollowed(current, held)
		switch {
		case reason != "":
			result = outcomeUnfollowed
		case message.movesSchema(current, held):
			reason = ReadReasonSchema
		}
	}
	return result, ownRead(reason, readMade)
}

// ownRead is the rule that a message whose whole read was made calls for no read of its own: it
// answers "" where readMade is set, and reason, the read the message calls for, otherwise. The
// read that was made holds the message's events, since the daemon commits each event before it
// publishes it, and the message is judged again on the cursor that read left. A read the cursor
// still owes, the read that was made having failed, is held and not made again in the same
// delivery (cursor.readFor). judge answers every reason but ReadReasonStart through it, and
// readRefused its ReadReasonRefused.
func ownRead(reason string, readMade bool) string {
	if readMade {
		return ""
	}
	return reason
}

// applyBatch takes the attempt's message in one transaction that holds the project's position
// (lockPosition, and the parts held for it, heldParts). A message whose hold of the owed read a
// write of the setting that named another URL, or a delete of the setting, ended (cursor.releases)
// is finished unapplied at this delivery and holds no read, and its store is marked as no daemon of
// the project (endHold). Any other message is judged (judge), and applyBatch hands the read it
// calls for to the read rule (answerRead).
// A message the cursor finishes unapplied (cursor.finishesUnapplied) writes nothing, hands no
// read to the rule, and queues on the project's runner behind it the call for the owed read that
// callRead decides. A holder of a store the cursor drops goes on to the rule with no reason of its
// own (judge) and is never applied (cursor.drops).
// Where the rule makes a read now, the batch writes nothing and answers that read: the caller makes
// it outside the transaction (makeRead) and tries again. Otherwise the transaction records what the
// rule owes (answerRead, owe), and the message comes to judge's outcome: it is acknowledged once
// its batch commits, or goes back to the stream where it holds the owed read, which is set once the
// commit succeeds (messageAttempt.answered). A message judge leaves unapplied writes nothing else
// (outcomeNoStore, outcomeOtherStore, outcomeUnfollowed, outcomeBeyond), and neither does one whose
// project has no setting (outcomeDropped), one at or below the position, or one with more of its
// transaction to come, which is held (holdPart).
//
// The last message of a transaction applies every event of it above the cursor, the held parts'
// and its own, each field at its last value in sequence order, written where it differs from
// legion_current's (recordEvents); one legion_reads row of kind `events` at the message's `at`
// with the bytes of every message of the transaction; the cursor moved to the message's sequence
// and run, and to the schema version of a boot among the events; the parts deleted; and the
// batch's announcement on StateChannel, which Postgres sends at the commit.
func (in *Intake) applyBatch(ctx context.Context, attempt *messageAttempt) (outcome, *owedRead, error) {
	message := attempt.message
	project := message.Project
	tx, err := in.pool.Begin(ctx)
	if err != nil {
		return outcomeDone, nil, fmt.Errorf("open the transaction of the Legion state events of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockPosition(ctx, tx, project)
	if err != nil {
		return outcomeDone, nil, err
	}
	if !found {
		return outcomeDropped, nil, nil
	}
	if current.releases(attempt.seq) {
		attempt.cursor = current
		result, err := endHold(ctx, tx, project, current, attempt.seq, message.StateID)
		return result, nil, err
	}
	held, err := heldParts(ctx, tx, project)
	if err != nil {
		return outcomeDone, nil, err
	}
	result, reason := judge(current, held, stateURL, message, attempt.readMade, attempt.readGood)
	attempt.cursor, attempt.reason = current, reason
	if finishes, _ := current.finishesUnapplied(message, stateURL, attempt.seq); finishes {
		in.callRead(project, current)
		return result, nil, nil
	}
	answer, err := in.answerRead(ctx, tx, attempt, current, reason)
	if err != nil {
		return outcomeDone, nil, err
	}
	if answer.read {
		read := answer.of(Settings{Project: project, StateURL: stateURL})
		return outcomeDone, &read, nil
	}
	// Every return from here commits: a read this batch recorded owed is part of it, and so is the
	// holder it recorded or released, so the message's answer is set once the commit succeeds.
	finish := func(result outcome) (outcome, *owedRead, error) {
		if err := tx.Commit(ctx); err != nil {
			return outcomeDone, nil, fmt.Errorf("commit the Legion state events of %s: %w", project, err)
		}
		attempt.answered(answer)
		return result, nil, nil
	}
	if result != outcomeDone {
		return finish(result)
	}
	// A message judge takes that has parts held before it continues them (unfollowed), or is one
	// of them delivered again.
	if n := len(held); n > 0 && message.Seq <= held[n-1].lastSeq {
		return finish(outcomeDone)
	}
	// The transaction's events in sequence order: the held parts', then the message's own.
	var transaction []stateEvent
	bytes := attempt.bytes
	for _, one := range held {
		transaction = append(transaction, one.events...)
		bytes += one.bytes
	}
	transaction = append(transaction, message.Events...)
	events := eventsAfter(transaction, current.lastSeq)
	if len(events) == 0 {
		return finish(outcomeDone)
	}
	if message.More {
		if err := holdPart(ctx, tx, project, message, attempt.bytes); err != nil {
			return outcomeDone, nil, err
		}
		return finish(outcomeDone)
	}
	if _, err := recordEvents(ctx, tx, project, message.At, lastValues(events)); err != nil {
		return outcomeDone, nil, err
	}
	if _, err := tx.Exec(ctx, insertRead, project, message.At, true, nil, bytes, readKindEvents, nil); err != nil {
		return outcomeDone, nil, fmt.Errorf("store the Legion state events of %s: %w", project, err)
	}
	var run *string
	if message.Run != "" {
		run = &message.Run
	}
	var schemaVersion *int64
	for _, event := range events {
		if event.Kind == eventBoot {
			schemaVersion = &event.SchemaVersion
		}
	}
	// The cursor keeps its store: judge takes only a message of the cursor's own store, and no
	// message at all while the cursor holds none. A message cannot give the cursor a store, since
	// it cannot show it is of the daemon the setting names: after a setting names another daemon,
	// the one it named before may still publish under the same project, and its stateId would then
	// pass for the named daemon's store. Only a good whole read of the daemon the setting names
	// sets the store (cursor.afterRead).
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors
		set last_seq = $2, run = $3, schema_version = coalesce($4, schema_version)
		where project = $1
	`, project, message.Seq, run, schemaVersion); err != nil {
		return outcomeDone, nil, fmt.Errorf("move where the Legion events of %s stand: %w", project, err)
	}
	if len(held) > 0 {
		if _, err := tx.Exec(ctx, `delete from legion_event_parts where project = $1`, project); err != nil {
			return outcomeDone, nil, fmt.Errorf("delete the held state events of %s: %w", project, err)
		}
	}
	if err := NotifyRead(ctx, tx, project); err != nil {
		return outcomeDone, nil, err
	}
	return finish(outcomeDone)
}
