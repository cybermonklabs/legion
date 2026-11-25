package legion

// cursor.go holds the intake's cursor of one project (dispatch://INFRA-3931): the cursor type, which
// is legion_event_cursors' row, and what it says of a message and of a read (follows, forgets,
// drops, finishesUnapplied, marks, holdsStore, heldBy, releases, skips and the rest); the one read
// rule every call for a whole read decides by (cursor.readFor, readAnswer, owedRead), with the
// whole read in flight it reads (readInFlight), the bounds of a message's hold of an owed read
// (holdAtLeast, stateMaxHolds) and what the rule owes, recorded on the row (owe); the cursor a
// whole read leaves (cursor.afterRead) and the one a write of the setting resets it to
// (cursor.reset, resetCursor), with the helpers of the row's arrays (with, marked, without,
// owedSince); and the row's columns, its reads and its writes (cursorColumns, scanCursor,
// writeCursor and the narrow writes beside it, endHold, writeSkipped and writeReadStart;
// readPosition, Intake.position, lockPosition, lockCursor).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// holdAtLeast is the shortest time a message that holds its project's owed read goes back to the
// stream for (cursor.readFor), so a message that comes back a moment before the read is due on
// Dispatch's clock does not come straight back again.
const holdAtLeast = time.Second

// stateMaxHolds is how many times one message goes back to the stream holding its project's owed
// whole read (cursor.readFor): the hand-back that waits for the read to be due, where the message
// arrived inside ReadRetryAfter of the start of the last read, and one for each read its
// deliveries made that failed, each until ReadRetryAfter after that read began. The cursor counts
// the hand-backs beside the holder (cursor.readHolds): 1 when it records the message as the
// holder, one more at each hand-back after it (owe), and 0 once it names no holder. A take-over
// creates the durable again, which starts the message's own NumDelivered again at 1, and keeps
// the cursor's count, so the bound holds for the message across every take-over. Neither a
// take-over's call for the read nor a settings change's makes a read while the message holds it,
// a starting read's included, and neither defers a call for it (cursor.readFor,
// Intake.startingPoint), so neither adds a read to the hold. A write of the setting, a delete
// included, either leaves the holder and its count as they are or ends the hold, by the rule that
// resetCursor and cursor.reset hold whole (read-rule-cases rows 6.1 to 6.5), so no write
// starts the count again. A message whose hold a write ended is acknowledged unapplied at its next
// delivery (outcomeReleased), and another message may then hold the read from a count of 1. A
// holder that has gone back stateMaxHolds times holds the read no more: that delivery is
// acknowledged, the cursor names no holder from then on (readAnswer.release), and the read stays
// owed (ReadReasonRetry) for the project's next message, boot, settings change or take-over, which
// makes it once it is due, as R3 makes every failed read. So each message holds the durable's ack
// floor below it for about 11 minutes at the most, however many take-overs and writes of the
// setting there are, and a take-over replays at most that long of messages (consumerConfig). One
// case is past that bound: a write of another URL or a delete that ends a hold while
// maxReleasedHolders messages released before it are all still out. The oldest record then goes,
// and that message is judged as any message at its next delivery: it may hold a read again from a
// count of 1, for about 11 minutes more.
const stateMaxHolds = 10

// owedRead is a whole read to make: the setting whose daemon it reads, the reason its legion_reads
// row records, and every reason it is made for in the order they called for it, reason first
// (joined), which its log line names (finishWhole). clears is the reasons the cursor owed when the
// read was decided, which its record clears (cursor.afterRead). A reason that a message adds while
// the read is in flight (judgeCrowded) stays owed after a failed read. A good read clears it too,
// but for two cases:
//   - The cursor's skipped record holds a sequence of the read's store above the read's stateSeq
//     (cursor.skippedAbove): the reason stays owed, and gap is owed with it.
//   - That record holds another store, which the cursor the read leaves does not drop
//     (cursor.skippedElsewhere): the reason stays owed, and store is owed with it.
//
// store is the store of the message that makes the read where that message is of another store
// than the cursor's (ReadReasonStore), and "" for every other read, a read that no message makes
// among them: a good read that finds the cursor's store unchanged marks it as no daemon of the
// project (cursor.afterRead, foreign_state_ids).
type owedRead struct {
	setting Settings
	reason  string
	joined  []string
	clears  []string
	store   string
}

// readOf is the whole read of setting's daemon made for reason alone, decided while the cursor owed
// no read: its record clears no reason.
func readOf(setting Settings, reason string) owedRead {
	return owedRead{setting: setting, reason: reason, joined: []string{reason}}
}

// owe records on project's cursor, through db (a batch's transaction, or the pool), what answer
// owes (cursor.readFor): its reason after the reasons the read is owed for already; the message at
// stream sequence seq as the owed read's holder where answer records it so (readAnswer.hold); the
// hand-backs of the holder counted to answer's holds where answer sends the message back; and no
// holder and no hand-back at all where answer releases the message that held the read. It writes
// nothing where answer does none of these.
func owe(ctx context.Context, db Execer, project string, answer readAnswer, seq int64) error {
	if answer.owe == "" && answer.back == 0 && !answer.release {
		return nil
	}
	reasons := []string{}
	if answer.owe != "" {
		reasons = append(reasons, answer.owe)
	}
	var holder *int64
	if answer.hold {
		holder = &seq
	}
	var holds *int
	if answer.back > 0 {
		holds = &answer.holds
	}
	if _, err := db.Exec(ctx, `
		update legion_event_cursors
		set read_reasons = read_reasons || $2::text[],
			read_holder = case when $4::boolean then null else coalesce($3, read_holder) end,
			read_holds = case when $4::boolean then 0 else coalesce($5, read_holds) end
		where project = $1
	`, project, reasons, holder, answer.release, holds); err != nil {
		return fmt.Errorf("record the whole read %s owes: %w", project, err)
	}
	return nil
}

// cursor is one project's legion_event_cursors row: how far its observations have followed its
// daemon (migration 20261118212431). exists is false for a project with no row.
type cursor struct {
	exists   bool
	stateURL string
	run      *string
	lastSeq  *int64
	// readReasons is every reason a whole read is owed for, in the order they called for it (owe),
	// and empty while none is owed (readOwed). A read's record clears the reasons owed when the read
	// was decided (afterRead). After a failed read it holds retry and each reason a message added
	// while the read was in flight. A good read clears those reasons too, but for two cases. Where
	// the skipped record holds a sequence of the read's store above its stateSeq (skippedAbove), they
	// stay, and gap with them. Where that record holds another store that the cursor the read leaves
	// does not drop (skippedElsewhere), they stay, and store with them. lastReadStartedAt is when the
	// project's newest whole read began, whatever its reason and whatever it came to: the next read
	// waits ReadRetryAfter after it (readDueIn). Every whole read writes it before it calls the
	// daemon (Intake.readWhole), so a read that a stop of the intake cuts short keeps its start
	// (read-rule-cases row 7.6), and the read's record writes it again. A write of the setting does
	// not reset it, so it holds across a write's reset of the cursor. It is nil only for a project
	// with no whole read begun since its cursor was created.
	// readHolder is the stream sequence of the message that holds the owed read, which goes back to
	// the stream until the read is due, and again after each read it makes that fails,
	// stateMaxHolds times at the most (readFor), and nil where no message holds it. readHolds is
	// how many times that message has gone back holding the read, which neither a take-over nor a
	// write of the URL the setting named already resets, and 0 where no message holds it.
	readReasons       []string
	lastReadStartedAt *time.Time
	readHolder        *int64
	readHolds         int
	// skippedStateIDs and skippedSeqs are, pair by pair, a store and the highest sequence of a
	// message of that store that judgeCrowded acknowledged unapplied past stateMaxWaiting (skips),
	// the store skipped last at the end, maxSkippedStores at the most. Every whole read empties
	// both as it begins, since it holds every event published before it (readWhole), so what they
	// hold as it lands was skipped while it was in flight. A good read owes the gap read where they
	// hold a sequence of its own store above its stateSeq (skippedAbove), and the store read where
	// they hold another store that the cursor it leaves does not drop (skippedElsewhere); otherwise
	// it leaves no reason owed for them. Either way it empties both, and a failed read keeps them
	// (afterRead). Every write that resets the cursor empties them (settingWritten).
	skippedStateIDs []string
	skippedSeqs     []int64
	// stateID is the store the cursor's sequence is counted in: nil until a good read sets it, and
	// again once a write of the setting clears it (settingWritten). schemaVersion is the store's
	// schema version as the cursor last saw it, nil for none. previousStateID is the store of the
	// daemon the setting named before a write that named another daemon, or before a delete of the
	// setting (settingWritten), nil for none. A write back to that daemon's URL clears it, and so
	// does a good read that gives the cursor that store (afterRead). previousStateURL is the URL
	// the cursor followed when the write recorded previousStateID, and goes with it: that URL is
	// the one a write back names.
	// foreignStateIDs is each store that is no daemon of the project, the newest last,
	// maxForeignStores at the most: the store of a message whose good store read found the cursor's
	// store unchanged (afterRead), and the store of a message whose hold a write of the setting
	// ended (endHold). Every write that resets the cursor empties it (settingWritten), and a good
	// read whose store is in it takes that store out. The intake finishes each message of a store
	// in either unapplied while the cursor holds no store or another store (forgets).
	stateID          *string
	schemaVersion    *int64
	previousStateID  *string
	previousStateURL *string
	foreignStateIDs  []string
	// releasedHolders is the stream sequence of each message whose hold of the owed read a write of
	// the setting that named another URL, or a delete of the setting, ended (settingWritten),
	// whatever daemon the cursor followed, none included, and which has not come back since: the
	// newest last, maxReleasedHolders at the most, and empty, never nil, for none. Each is finished
	// unapplied at its next delivery, which takes its own sequence out and no other (releases,
	// endHold). Past maxReleasedHolders the oldest record goes (reset): its message is judged as
	// any other.
	releasedHolders []int64
}

// maxForeignStores is how many stores a cursor marks as no daemon of its project
// (foreignStateIDs). Past it the oldest mark goes, and that store's next message makes one store
// read again, at most once a ReadRetryAfter.
const maxForeignStores = 8

// maxReleasedHolders is how many messages a cursor records as released (releasedHolders): each a
// message whose hold a write of another URL or a delete of the setting ended, until it comes back.
// Past it the oldest record goes (reset). That message is judged as any message at its next
// delivery, which applies nothing of a store other than the cursor's, and it may hold a read again
// from a count of 1.
const maxReleasedHolders = 8

// maxSkippedStores is how many stores a cursor keeps the highest sequence skipped past
// stateMaxWaiting of (skippedStateIDs). During a whole read that may verify a store the cursor does
// not hold (read-rule-cases rows 4.4, 5.2 and 5.6), messages of the cursor's store, of the store a
// write recorded and of the stores marked as no daemon of the project can all arrive past the cap,
// so the cursor keeps several, bounded as the marks are (maxForeignStores). Past it the store
// skipped longest ago goes: a read that then finds that store owes no gap read for it, and that
// store's next message calls for one.
const maxSkippedStores = maxForeignStores

// readOwed reports whether the cursor owes a whole read: whether it names a reason for one, which
// is what the column read_owed, generated from read_reasons, says too.
func (c cursor) readOwed() bool {
	return len(c.readReasons) > 0
}

// readDueIn is how long after now a whole read of the project may start, whatever its reason, a
// starting read's included: until ReadRetryAfter after the project's newest whole read began
// (lastReadStartedAt). It is 0 where that moment has passed, or where the project has no read to
// wait after.
func (c cursor) readDueIn(now time.Time) time.Duration {
	if c.lastReadStartedAt == nil {
		return 0
	}
	return dueAfter(*c.lastReadStartedAt, now)
}

// dueAfter is how long after now a whole read is due where the project's newest whole read began
// at started: ReadRetryAfter after started, and 0 once that moment has passed.
func dueAfter(started, now time.Time) time.Duration {
	return max(started.Add(ReadRetryAfter).Sub(now), 0)
}

// readInFlight is the whole read in flight on one project's runner (Intake.reading). started is
// when it began, on the intake's clock, so a message past stateMaxWaiting that holds the read after
// it goes back to the stream until ReadRetryAfter after that start. pastCap is the stream sequence
// of the first message past stateMaxWaiting the intake judged since it began, 0 for none: that
// message takes the hold of the read after it (judgeCrowded, Intake.inFlight, cursor.readFor).
type readInFlight struct {
	started time.Time
	pastCap int64
}

// readAnswer is what the read rule answers a call for a whole read (cursor.readFor).
type readAnswer struct {
	// read is a whole read made now, for reason, the one its legion_reads row records, joined,
	// every reason it is made for in the order they called for it, reason first, and clears, the
	// reasons the cursor owes now, which the read's record clears (owedRead.clears). store is the
	// store of the message that makes the read, where it is of another store (answerRead).
	read   bool
	reason string
	joined []string
	clears []string
	store  string
	// owe is the reason the call adds to the owed read, and "" where it adds none: no read is owed,
	// the read is made now, or the cursor owes it for that reason already.
	owe string
	// back is how long the message that called goes back to the stream for, unacknowledged, where it
	// holds the owed read: the time left until the read is due, holdAtLeast at the least and
	// ReadRetryAfter at the most. It is 0 where the message is acknowledged. hold is whether the
	// cursor records the message as the read's holder, which it does where it names none yet, and
	// where the message takes the hold over from the holder of a read in flight (readFor, row
	// 8.1). release is whether the cursor stops naming the message as the holder, which it does
	// once the message has gone back stateMaxHolds times. holds is the hand-backs the cursor counts
	// once back is recorded, which this one makes, and the hand-backs it counted where release is
	// set.
	back    time.Duration
	hold    bool
	release bool
	holds   int
	// later is, for a call no message makes, the time left until the starting read is due, where
	// the call is for that read (ReadReasonStart), the read is not due and no message holds it: a
	// wake's or a take-over's call then defers one call of the starting point for when the minute
	// ends (startingPoint). It is 0 for every other call.
	later time.Duration
}

// of is the read answer makes now, of setting's daemon.
func (a readAnswer) of(setting Settings) owedRead {
	return owedRead{setting: setting, reason: a.reason, joined: a.joined, clears: a.clears, store: a.store}
}

// readFor is the one rule of every whole read, which each call for one decides by: a message's
// (applyBatch, readRefused, judgeCrowded) and a starting point's (startingPoint). reason is the
// read the call needs, "" for none (a call that only makes a read owed already), now is the
// intake's clock, seq is the stream sequence of the message that calls, 0 for a call no message
// makes, and made whether the delivery of that message has made a whole read already
// (messageAttempt.readMade). flight is the whole read in flight on the project's runner where a
// message is judged beside the runner while that read is in flight (judgeCrowded,
// Intake.inFlight), and nil for every other call. A message of a store the cursor drops
// (outcomeForgotten) calls it as a call no message makes, to decide whether to queue the project's
// starting point, which calls it again on the project's runner (callRead): so such a message never
// makes a read for itself or owes one. Where the cursor names it as the holder, which it can be
// after it owed a gap read past stateMaxWaiting while a read was in flight, it calls it with no
// reason as the holder, so it makes the held read once that read is due and goes back to the
// stream until then (applyBatch). A message on its project's crowded key calls it as a delivery
// that made its read already, so it never makes the read (judgeCrowded).
//
// A read is due ReadRetryAfter after the project's newest whole read began (readDueIn). Where
// flight names a read in flight, that read is the newest: the read after it is due ReadRetryAfter
// after the start of the read in flight.
//
//   - ReadReasonStart is made now and alone where it is due and the delivery has made no read
//     yet, but for a call no message makes where the cursor names a holder (below): each reason
//     its cursor owes is for a daemon the setting no longer names, or for events the read's
//     snapshot holds, if it has a cursor at all. Where it is not due, the project's newest whole
//     read, of any kind, began less than ReadRetryAfter ago (read-rule-cases v11, row 3.2): the
//     starting read is owed and held as every read below. A call no message makes that finds it
//     so, and that no message holds, answers the time left until it is due (readAnswer.later).
//   - A call with no reason, where no read is owed, makes no read and owes none.
//   - A call no message makes, where the cursor names a holder, makes no read, due or not,
//     whatever its reason, ReadReasonStart included: it joins the holder's read, which the
//     holder's redelivery makes, the cursor counting its hand-backs on from readHolds. So neither
//     a take-over nor a settings change (startingPoint) adds a read to the hold, whether the
//     cursor follows the setting's daemon or no daemon yet, and the hold's bound (stateMaxHolds)
//     stands however many of them come.
//   - A read that is due is made now, for every reason the cursor owes it for, in the order they
//     called for it, then for reason where it is not among them. It is recorded under the first.
//     Its record clears the reasons the cursor owes now (readAnswer.clears). A reason that a
//     message adds while the read is in flight stays owed where the read fails. A good read clears
//     it too, but for the two cases of owedRead.clears, which owe gap or store with it
//     (cursor.afterRead). A delivery makes one read at the most: where it made one already, the
//     read is held as one that is not due. The minute counts from the read's start, so a read that
//     the daemon answers only after the minute, or never answers within RequestTimeout, is due
//     again as soon as it fails, and its message goes back to the stream rather than reading at
//     once.
//   - Otherwise the read is owed, and reason joins it once: where it is not among the reasons the
//     cursor owes it for. The message that holds the owed read goes back to the stream for the
//     time left until the read is due, holdAtLeast at the least, so that its redelivery makes the
//     read. Every message that holds no read is acknowledged.
//   - A read in flight changes the holder (read-rule-cases row 8.1). The first message past
//     stateMaxWaiting since that read began (flight.pastCap) holds the read after it, from a count
//     of 1, whatever message the cursor names as the holder, unless the cursor names that message
//     already. The holder the cursor names holds the read in flight, which clears the reasons it
//     was made for. The read after it is for the events past its snapshot, and only the messages
//     past the cap since it began can carry them.
//   - In every other case, the message that holds the owed read is the message the cursor names as
//     the holder, or the message that calls where the cursor names none. The cursor counts each
//     hand-back (readHolds), and a holder that has gone back stateMaxHolds times holds the read no
//     more: the cursor names no holder from then on (release).
//
// So each project that owes a read holds one message of the durable's window. Where a message
// holds the read, a project that goes quiet is ReadRetryAfter behind at the most, and a read that
// fails sends the message back until ReadRetryAfter after the read began, stateMaxHolds times at
// the most in all, across take-overs too, which make no read of their own meanwhile. A failed
// read that no message holds, a starting point's or one whose message went back stateMaxHolds
// times, is made at the project's next message, boot, settings change or take-over, once it is
// due: the next message to call holds it. A starting read that a write's wake or a take-over finds
// not due, and that no message holds, is made when it is due by one deferred call (startingPoint),
// which readAnswer.later times. A message of a store the cursor drops neither makes nor holds that
// read unless the cursor names it as the holder: whatever store the cursor holds, it queues a call
// for it on the project's runner, which makes it as a call no message makes (callRead).
func (c cursor) readFor(reason string, now time.Time, seq int64, made bool, flight *readInFlight) readAnswer {
	dueIn := c.readDueIn(now)
	if flight != nil {
		dueIn = dueAfter(flight.started, now)
	}
	// A call no message makes joins the read a holder holds rather than making it, a starting
	// read included: the holder's redelivery makes it.
	reads := dueIn == 0 && !made && (seq != 0 || c.readHolder == nil)
	switch {
	case reason == ReadReasonStart && reads:
		return readAnswer{read: true, reason: ReadReasonStart, joined: []string{ReadReasonStart}, clears: c.readReasons}
	case reason == "" && !c.readOwed():
		return readAnswer{}
	case reads:
		// A call with no reason reaches here only where a read is owed, so the read has a reason
		// to be recorded under: the first it was owed for, or the call's own.
		answer := readAnswer{read: true, reason: reason, joined: []string{reason}, clears: c.readReasons}
		if c.readOwed() {
			answer.reason, answer.joined = c.readReasons[0], c.readReasons
			if reason != "" && !slices.Contains(c.readReasons, reason) {
				answer.joined = append(slices.Clone(c.readReasons), reason)
			}
		}
		return answer
	}
	var answer readAnswer
	if reason != "" && !slices.Contains(c.readReasons, reason) {
		answer.owe = reason
	}
	// back is the hand-back delay of a message that holds the read: the time left in the minute,
	// at least holdAtLeast and at most ReadRetryAfter.
	back := min(max(dueIn, holdAtLeast), ReadRetryAfter)
	switch {
	case seq == 0:
		// No message calls: none holds the read. A starting read that no message holds is due
		// dueIn from now, and a wake defers its call for it that long (startingPoint).
		if reason == ReadReasonStart && c.readHolder == nil {
			answer.later = dueIn
		}
	case flight != nil && flight.pastCap == seq && !c.heldBy(seq):
		// Row 8.1: the first message past the cap since the read in flight began takes the hold
		// of the read after it, from a count of 1, whatever message the cursor names.
		answer.back = back
		answer.hold, answer.holds = true, 1
	case c.readHolder != nil && *c.readHolder != seq:
		// The cursor names another message as the holder: this one holds none.
	case c.readHolder != nil && c.readHolds >= stateMaxHolds:
		answer.release, answer.holds = true, c.readHolds
	default:
		answer.back = back
		answer.hold, answer.holds = c.readHolder == nil, c.readHolds+1
	}
	return answer
}

// follows reports whether the cursor follows the daemon at stateURL. A cursor that is missing, or
// that follows another daemon than the setting names, or none at all, as a write of the setting
// leaves it (settingWritten), is no starting point.
func (c cursor) follows(stateURL string) bool {
	return c.exists && c.stateURL == stateURL
}

// forgets reports whether a message of the store stateID is one of a daemon the setting no longer
// names: its store is the one recorded by a write that named another daemon or by a delete of the
// setting (previousStateID), or one marked as no daemon of the project (foreignStateIDs), and the
// cursor holds no store or another store. R3 applies no message of a daemon the setting named
// before once the setting names another.
// Such a message calls for no read of its own, but for a boot (drops), and makes one only as the
// holder of a read it took past stateMaxWaiting (judgeCrowded, applyBatch). It waits on the
// project's runner behind a read in flight, as every message does, and is judged against the
// cursor that read leaves. The message may be the setting's own daemon's, behind the setting's URL
// in this store, so it calls for the owed read where that read is due and no message holds it,
// whatever store the cursor holds, which the runner makes after it, and adds no reason (callRead).
// A write back to the URL recorded with that store clears the record, every write that resets the
// cursor clears the marks (settingWritten), and a good read whose store is that one clears the
// record or the mark (afterRead): the daemon's messages are then the setting's daemon's again,
// taken as every message of the cursor's store is.
func (c cursor) forgets(stateID string) bool {
	if c.holdsStore(stateID) {
		return false
	}
	return c.isPrevious(stateID) || c.marks(stateID)
}

// drops reports whether message is one of a daemon the setting no longer names, which the cursor
// finishes unapplied with no read of its own (outcomeForgotten) unless it names the message as the
// owed read's holder (finishesUnapplied): a message of a store it forgets, but a boot of such a
// store while the cursor follows stateURL, the setting's URL, in a store a read verified
// (read-rule-cases row 3.5). Such a boot may say that a redeploy started the setting's daemon in
// that store with no write of the setting. R3 reads another store so that a daemon whose store was
// replaced is found again, so the boot is judged as a message of another store (ReadReasonStore):
// on the runner (judge), past stateMaxWaiting (judgeCrowded) and when the database refuses it
// (readRefused).
func (c cursor) drops(message stateMessage, stateURL string) bool {
	return c.forgets(message.StateID) && !(message.boots() && c.follows(stateURL) && c.stateID != nil)
}

// finishesUnapplied reports, for message at stream sequence seq, whether the cursor finishes it
// unapplied with no read of its own (finishes, outcomeForgotten), and whether it drops it
// (dropped, drops). finishes is dropped but for the owed read's holder: a message the cursor names
// as the holder (heldBy), which a message past stateMaxWaiting of any store can become
// (judgeCrowded), is never finished unapplied while it holds the read, whatever store it is of,
// since a held read is made by its holder's redelivery. Such a message goes on to the read rule as
// the holder, with no reason of its own, and is never applied. applyBatch, judgeCrowded and
// readRefused decide by finishes, so no caller checks the holder beside drops. dropped is drops'
// answer alone, for the two callers that need it beside finishes: judgeCrowded, which owes no
// store read for a message the cursor drops, and readRefused, whose holder of a store the cursor
// drops calls for no reason.
func (c cursor) finishesUnapplied(message stateMessage, stateURL string, seq int64) (finishes, dropped bool) {
	dropped = c.drops(message, stateURL)
	return dropped && !c.heldBy(seq), dropped
}

// marks reports whether the cursor marks the store stateID as no daemon of the project
// (foreignStateIDs).
func (c cursor) marks(stateID string) bool {
	return slices.Contains(c.foreignStateIDs, stateID)
}

// holdsStore reports whether the cursor's sequence is counted in the store stateID.
func (c cursor) holdsStore(stateID string) bool {
	return c.stateID != nil && *c.stateID == stateID
}

// isPrevious reports whether stateID is the store that a write of the setting recorded as the
// store of the daemon the setting named before (previousStateID).
func (c cursor) isPrevious(stateID string) bool {
	return c.previousStateID != nil && *c.previousStateID == stateID
}

// marking is the cursor's marks (foreignStateIDs) with the store stateID marked as no daemon of
// the project, as the newest (marked). The cursor's own store and the store a write recorded
// (isPrevious) are never marked: for either of them, marking is the marks without stateID. Two
// steps mark a store. A good store read that finds the cursor's store unchanged marks the store
// of the message that called for it (afterRead, read-rule-cases row 5.7). The message whose hold a
// write of the setting ended marks its own store at its next delivery (endHold, row 6.3b).
func (c cursor) marking(stateID string) []string {
	if c.holdsStore(stateID) || c.isPrevious(stateID) {
		return without(c.foreignStateIDs, stateID)
	}
	return marked(c.foreignStateIDs, stateID, maxForeignStores)
}

// releases reports whether the message at stream sequence seq is one whose hold of the owed read a
// write of the setting ended and which has not come back since (releasedHolders). 0, a message
// with no JetStream metadata, is none.
func (c cursor) releases(seq int64) bool {
	return seq != 0 && slices.Contains(c.releasedHolders, seq)
}

// heldBy reports whether the message at stream sequence seq is the one that holds the owed read
// (readHolder). 0, a message with no JetStream metadata, holds none.
func (c cursor) heldBy(seq int64) bool {
	return seq != 0 && c.readHolder != nil && *c.readHolder == seq
}

// skippedAbove reports whether a message of the store stateID that the intake acknowledged
// unapplied past stateMaxWaiting carried a sequence above seq (skippedStateIDs, skippedSeqs).
func (c cursor) skippedAbove(stateID string, seq int64) bool {
	i := slices.Index(c.skippedStateIDs, stateID)
	return i >= 0 && c.skippedSeqs[i] > seq
}

// skippedElsewhere reports whether the cursor's skipped record holds a store other than the one
// left holds that left does not drop (forgets): left is the cursor a good read leaves.
func (c cursor) skippedElsewhere(left cursor) bool {
	return slices.ContainsFunc(c.skippedStateIDs, func(stateID string) bool {
		return !left.holdsStore(stateID) && !left.forgets(stateID)
	})
}

// with is reasons with each of more after it that it does not hold yet, in more's order.
func with(reasons []string, more ...string) []string {
	out := slices.Clone(reasons)
	for _, reason := range more {
		if !slices.Contains(out, reason) {
			out = append(out, reason)
		}
	}
	return out
}

// skips is the cursor's skipped sequences with seq skipped in the store stateID: that store's
// highest, at the end as the store skipped last, and once. Past maxSkippedStores the stores
// skipped longest ago go. Never nil: the array columns are not null.
func (c cursor) skips(stateID string, seq int64) (stateIDs []string, seqs []int64) {
	stateIDs, seqs = make([]string, 0, len(c.skippedStateIDs)+1), make([]int64, 0, len(c.skippedSeqs)+1)
	for i, one := range c.skippedStateIDs {
		if one == stateID {
			seq = max(seq, c.skippedSeqs[i])
			continue
		}
		stateIDs, seqs = append(stateIDs, one), append(seqs, c.skippedSeqs[i])
	}
	stateIDs, seqs = append(stateIDs, stateID), append(seqs, seq)
	if n := len(stateIDs); n > maxSkippedStores {
		stateIDs, seqs = stateIDs[n-maxSkippedStores:], seqs[n-maxSkippedStores:]
	}
	return stateIDs, seqs
}

// marked is values with value as the newest: at the end, and once. Past limit the oldest go. A
// cursor so keeps the stores it marks as no daemon of its project (foreignStateIDs,
// maxForeignStores) and the messages it records as released (releasedHolders, maxReleasedHolders).
func marked[T comparable](values []T, value T, limit int) []T {
	kept := without(values, value)
	kept = append(kept, value)
	if n := len(kept); n > limit {
		kept = kept[n-limit:]
	}
	return kept
}

// without is values without value, in their order and never nil: the array columns
// foreign_state_ids, released_holders and read_reasons are not null.
func without[T comparable](values []T, value T) []T {
	kept := make([]T, 0, len(values)+1)
	for _, one := range values {
		if one != value {
			kept = append(kept, one)
		}
	}
	return kept
}

// owedSince is reasons, the reasons a cursor owes as a read's record is written, less one of each
// reason in clears, the reasons it owed as the read was decided: the reasons a message added while
// the read was in flight, in the order they were added. Never nil: the column read_reasons is not
// null.
func owedSince(reasons, clears []string) []string {
	left := slices.Clone(clears)
	since := []string{}
	for _, reason := range reasons {
		if index := slices.Index(left, reason); index >= 0 {
			left = slices.Delete(left, index, index+1)
			continue
		}
		since = append(since, reason)
	}
	return since
}

// emptyCursor is a cursor that follows no daemon (an empty stateURL), with every column at its
// empty value: no run, sequence, reason, read start, holder, store, schema version, recorded
// store, mark, released holder or skipped sequence. Each array is empty and never nil, since the
// array columns are not null. It is the cursor of a project with no row (scanCursor), and the
// base of the cursor that a write of the setting resets a row to (cursor.reset).
func emptyCursor() cursor {
	return cursor{readReasons: []string{}, foreignStateIDs: []string{}, releasedHolders: []int64{}, skippedStateIDs: []string{}, skippedSeqs: []int64{}}
}

// cursorColumns is the legion_event_cursors select list scanCursor reads, in scan order.
const cursorColumns = `state_url, run, last_seq, read_reasons, last_read_started_at, read_holder, read_holds, state_id,
	schema_version, previous_state_id, previous_state_url, foreign_state_ids, released_holders, skipped_state_ids, skipped_seqs`

// scanCursor decodes one cursorColumns row, or no row as a cursor that does not exist
// (emptyCursor). The array columns are not null, so a row's arrays are never nil either. So no
// caller guards a cursor's array against nil, and no cursor computed from one writes NULL into an
// array column (writeCursor).
func scanCursor(row pgx.Row) (cursor, error) {
	current := cursor{exists: true}
	err := row.Scan(&current.stateURL, &current.run, &current.lastSeq, &current.readReasons, &current.lastReadStartedAt,
		&current.readHolder, &current.readHolds, &current.stateID, &current.schemaVersion, &current.previousStateID,
		&current.previousStateURL, &current.foreignStateIDs, &current.releasedHolders, &current.skippedStateIDs, &current.skippedSeqs)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyCursor(), nil
	}
	return current, err
}

// writeCursor writes c as project's legion_event_cursors row inside tx, in one statement: every
// column of cursorColumns from c, but last_read_started_at, which takes the later of the row's
// value and c's. So no write moves the start of the project's newest whole read back. A project
// with no row is given one. recordWhole writes the cursor a read leaves (cursor.afterRead), and
// settingWritten writes the cursor a write of the setting resets a row to (cursor.reset).
func writeCursor(ctx context.Context, tx pgx.Tx, project string, c cursor) error {
	_, err := tx.Exec(ctx, `
		insert into legion_event_cursors (project, state_url, run, last_seq, read_reasons, last_read_started_at, read_holder,
			read_holds, state_id, schema_version, previous_state_id, previous_state_url, foreign_state_ids, released_holders,
			skipped_state_ids, skipped_seqs)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		on conflict (project) do update set
			state_url = excluded.state_url, run = excluded.run, last_seq = excluded.last_seq,
			read_reasons = excluded.read_reasons,
			last_read_started_at = greatest(legion_event_cursors.last_read_started_at, excluded.last_read_started_at),
			read_holder = excluded.read_holder, read_holds = excluded.read_holds, state_id = excluded.state_id,
			schema_version = excluded.schema_version, previous_state_id = excluded.previous_state_id,
			previous_state_url = excluded.previous_state_url, foreign_state_ids = excluded.foreign_state_ids,
			released_holders = excluded.released_holders, skipped_state_ids = excluded.skipped_state_ids,
			skipped_seqs = excluded.skipped_seqs
	`, project, c.stateURL, c.run, c.lastSeq, c.readReasons, c.lastReadStartedAt, c.readHolder, c.readHolds, c.stateID,
		c.schemaVersion, c.previousStateID, c.previousStateURL, c.foreignStateIDs, c.releasedHolders, c.skippedStateIDs,
		c.skippedSeqs)
	return err
}

// endHold finishes, inside tx, the message at stream sequence seq whose hold of project's owed read
// a write of the setting ended (cursor.releases): it takes seq out of released_holders and keeps
// every other message recorded there, each of which its own next delivery finishes; it marks
// stateID, the message's store, as no daemon of the project (foreign_state_ids, cursor.marking)
// unless it is the store the write recorded or the cursor's own store, and commits. current is the
// cursor tx locked, so no other write of the row lands between. The message is acknowledged
// unapplied and holds no read (outcomeReleased).
func endHold(ctx context.Context, tx pgx.Tx, project string, current cursor, seq int64, stateID string) (outcome, error) {
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors set released_holders = $2, foreign_state_ids = $3 where project = $1
	`, project, without(current.releasedHolders, seq), current.marking(stateID)); err != nil {
		return outcomeDone, fmt.Errorf("end the hold of a state message of %s: %w", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return outcomeDone, fmt.Errorf("commit the end of the hold of a state message of %s: %w", project, err)
	}
	return outcomeReleased, nil
}

// writeSkipped writes stateIDs and seqs, pair by pair, as project's skipped record inside tx
// (skipped_state_ids, skipped_seqs): the record that cursor.skips computes for a message that
// judgeCrowded acknowledged unapplied past stateMaxWaiting, so the read in flight leaves the gap
// read or the store read owed where its snapshot does not answer that message (cursor.afterRead).
func writeSkipped(ctx context.Context, tx pgx.Tx, project string, stateIDs []string, seqs []int64) error {
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors set skipped_state_ids = $2, skipped_seqs = $3 where project = $1
	`, project, stateIDs, seqs); err != nil {
		return fmt.Errorf("record the state message of %s skipped past the cap: %w", project, err)
	}
	return nil
}

// writeReadStart writes started, the start of a whole read of project, on project's cursor through
// db, the pool, before the read calls the daemon (readWhole): last_read_started_at takes the later
// of the row's start and started, as every write of the start keeps it (writeCursor). It gives a
// project that has a setting and no cursor yet a cursor that follows no daemon, as lockCursor does,
// and a project with no setting no cursor. A project whose setting was deleted keeps its cursor
// (read-rule-cases row 6.5), so its start is written as any project's. The start is written before
// the call because a read that a stop of the intake cuts short records nothing else. The next
// holder of the durable finds that start, at its take-over and at the redelivery of the message
// that made the read, and makes no read until ReadRetryAfter after it (R3-g, read-rule-cases row
// 7.6). The same statement empties the sequences the cursor skipped past stateMaxWaiting
// (cursor.skippedStateIDs): the daemon commits each event before it publishes it, so the read holds
// every event of a message published before it began, and only a message skipped while it is in
// flight can be past its snapshot (recordWhole). So a read owes no gap read for a message skipped
// before it, whatever sequence that message claims.
func writeReadStart(ctx context.Context, db Execer, project string, started time.Time) error {
	if _, err := db.Exec(ctx, `
		insert into legion_event_cursors (project, state_url, last_read_started_at)
		select $1, '', $2
		where exists (select 1 from legion_settings where project = $1)
			or exists (select 1 from legion_event_cursors where project = $1)
		on conflict (project) do update set
			last_read_started_at = greatest(legion_event_cursors.last_read_started_at,
				excluded.last_read_started_at),
			skipped_state_ids = '{}', skipped_seqs = '{}'
	`, project, started); err != nil {
		return fmt.Errorf("record the start of a whole read of %s and empty the state messages it skipped past the cap: %w", project, err)
	}
	return nil
}

// resetCursor is what settingWritten does to project's cursor for a write of stateURL, inside tx,
// where before is the URL the setting named before the write ("" for a new setting) and stateURL
// is "" for a delete. With cursor.reset it holds the whole rule of what a write of the setting, a
// delete included, does to the cursor (read-rule-cases rows 6.1 to 6.5): it locks the cursor, and
// then either leaves it as it is or writes the cursor the write resets it to (cursor.reset,
// writeCursor) and deletes the parts held for it.
//   - It writes nothing for a project with no cursor, which has no starting point already.
//   - It leaves the cursor as it is for a write of the URL the setting named already (before)
//     while a message holds the owed read, whether the cursor follows that URL or no daemon yet
//     (rows 6.1 and 6.1b): the cursor keeps its holder, its count of hand-backs, its reasons, its
//     skipped record and its held parts. The held read is a whole read of the same URL, so it is
//     the read the write calls for: the holder's redelivery makes it, the write's wake joins it
//     and defers no call (cursor.readFor, Intake.startingPoint), and the hold's bound
//     (stateMaxHolds) stands.
//   - It resets the cursor for every other write: a write of the same URL that finds no holder
//     (row 6.2), a write of another URL (rows 6.3, 6.3b and 6.4) and a delete (row 6.5). The
//     cursor it leaves follows no daemon, so it is no starting point (cursor.follows), and the
//     intake reads the setting's daemon whole once that read is due (ReadReasonStart): at the
//     write's wake, or at the redelivery of a message that holds the read meanwhile
//     (Intake.startingPoint, cursor.readFor). A delete resets the cursor as a write of another URL
//     does, since the URL a setting names is never "", and keeps its row with what cursor.reset
//     keeps. Its wake reads nothing, since the intake reads no project that has no setting, and a
//     write after it is a new setting (before ""), which resets the cursor as a write of another
//     URL does.
func resetCursor(ctx context.Context, tx pgx.Tx, project, before, stateURL string) error {
	current, err := scanCursor(tx.QueryRow(ctx, `select `+cursorColumns+` from legion_event_cursors where project = $1 for update`, project))
	if err != nil || !current.exists || (before == stateURL && current.readHolder != nil) {
		return err
	}
	if err := writeCursor(ctx, tx, project, current.reset(stateURL)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `delete from legion_event_parts where project = $1`, project)
	return err
}

// reset is the cursor that a write of the setting naming stateURL, "" for a delete, resets c to
// (resetCursor): a cursor that follows no daemon, with every column at its empty value
// (emptyCursor) but these. Among the columns it empties are the marks of stores that are no
// daemon of the project (foreignStateIDs), since a mark says only that its store is no daemon the
// setting named before the write (read-rule-cases row 6.4b).
//   - lastReadStartedAt is c's. Whole reads of one project start ReadRetryAfter apart whatever
//     their reasons (R3-g), so the starting read after a write within a minute of the project's
//     newest whole read, with a delete between them or none, is made when that minute ends
//     (Intake.startingPoint, rows 3.2 and 6.5).
//   - releasedHolders is c's with c's readHolder added as the newest, where a message held the
//     read, and c's otherwise. resetCursor resets a cursor that names a holder only for a write of
//     another URL than the setting named, or for a delete, either of which ends that hold whatever
//     daemon c followed, none included (rows 6.3b and 6.5). That message is finished unapplied at
//     its next delivery, which takes its own record out and no other, and marks its store as no
//     daemon of the project unless it is the store recorded below or the cursor's own
//     (cursor.releases, endHold). While the project has no setting, its messages are dropped
//     before the cursor is read, so such a record stays (DeleteSettings). Past maxReleasedHolders
//     the oldest record goes (marked): that message is judged as any message at its next delivery.
//   - previousStateID and previousStateURL are none where the write names the URL recorded there
//     (row 6.4): the daemon recorded is the setting's again, and its messages are judged as the
//     setting's daemon's. They are c's store and URL where c followed a URL other than the
//     write's and held a store (rows 6.3 and 6.5), and c's own pair otherwise. The intake finishes
//     each message of the store recorded there unapplied while the cursor holds no store or
//     another store (cursor.forgets), and a good read that gives the cursor that store clears the
//     pair (afterRead). A cursor holds a store only after a good read of the setting's URL, which
//     is never "", so the URL recorded there is never "" either: a delete never clears the pair,
//     and records c's store and URL where c held a store.
func (c cursor) reset(stateURL string) cursor {
	next := emptyCursor()
	next.exists, next.lastReadStartedAt = true, c.lastReadStartedAt
	next.releasedHolders = c.releasedHolders
	if c.readHolder != nil {
		next.releasedHolders = marked(c.releasedHolders, *c.readHolder, maxReleasedHolders)
	}
	next.previousStateID, next.previousStateURL = c.previousStateID, c.previousStateURL
	switch {
	case c.previousStateURL != nil && *c.previousStateURL == stateURL:
		next.previousStateID, next.previousStateURL = nil, nil
	case c.stateURL != stateURL && c.stateID != nil:
		next.previousStateID, next.previousStateURL = c.stateID, &c.stateURL
	}
	return next
}

// readPosition reads project's setting and then its cursor (cursorColumns, scanCursor) through db,
// the pool or a transaction (RowQueryer): inside a transaction with the setting `for share` and the
// cursor `for update` where locked is set (lockPosition), and outside any transaction with no lock
// otherwise (position). found is false for a project with no setting.
func readPosition(ctx context.Context, db RowQueryer, project string, locked bool) (stateURL string, current cursor, found bool, err error) {
	settingLock, cursorLock := "", ""
	if locked {
		settingLock, cursorLock = " for share", " for update"
	}
	switch err := db.QueryRow(ctx, `select state_url from legion_settings where project = $1`+settingLock, project).Scan(&stateURL); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", cursor{}, false, nil
	case err != nil:
		return "", cursor{}, false, fmt.Errorf("read the Legion setting of %s: %w", project, err)
	}
	current, err = scanCursor(db.QueryRow(ctx, `select `+cursorColumns+` from legion_event_cursors where project = $1`+cursorLock, project))
	if err != nil {
		return "", cursor{}, false, fmt.Errorf("read where the Legion events of %s stand: %w", project, err)
	}
	return stateURL, current, true, nil
}

// position reads project's setting and its cursor outside any transaction (readPosition), for a
// decision whether to read the daemon whole (startingPoint, readRefused), which the read's record
// checks again under the locks it takes (recordWhole); found is false for a project with no
// setting.
func (in *Intake) position(ctx context.Context, project string) (setting Settings, current cursor, found bool, err error) {
	stateURL, current, found, err := readPosition(ctx, in.pool, project, false)
	if err != nil || !found {
		return Settings{}, cursor{}, false, err
	}
	return Settings{Project: project, StateURL: stateURL}, current, true, nil
}

// lockPosition reads project's setting `for share` and its cursor `for update` inside tx
// (readPosition), which is how each write of the project's observations opens (applyBatch,
// recordWhole): a write of the setting (settingWritten) waits for the transaction, and so does
// each other write of the cursor. found is false for a project with no setting.
func lockPosition(ctx context.Context, tx pgx.Tx, project string) (stateURL string, current cursor, found bool, err error) {
	return readPosition(ctx, tx, project, true)
}

// lockCursor is lockPosition for a write that needs the project's cursor row locked whether or not
// the project has one yet (judgeCrowded, recordWhole). Where the project has a setting and no
// cursor, it inserts one that follows no daemon (an empty state_url, as a write of the setting
// leaves it), does nothing where another transaction inserted the row first, and locks the row
// again. So two such writes of one project serialize on the row: neither computes what it writes
// from no row while the other inserts one, so no write takes the place of the other.
func lockCursor(ctx context.Context, tx pgx.Tx, project string) (stateURL string, current cursor, found bool, err error) {
	stateURL, current, found, err = lockPosition(ctx, tx, project)
	if err != nil || !found || current.exists {
		return stateURL, current, found, err
	}
	if _, err := tx.Exec(ctx, `
		insert into legion_event_cursors (project, state_url) values ($1, '') on conflict (project) do nothing
	`, project); err != nil {
		return "", cursor{}, false, fmt.Errorf("start where the Legion events of %s stand: %w", project, err)
	}
	return lockPosition(ctx, tx, project)
}

// afterRead is the cursor that read, made for owed, leaves (recordWhole). c is the cursor as the
// read's record finds it, and read.stateURL is the setting's URL: the cursor left follows the
// setting's daemon. Every column that the rules below do not name keeps c's value,
// released_holders among them.
//
// Every read clears the reasons the cursor owed when it was decided (owed.clears). A failed one
// owes the read for retry (ReadReasonRetry), and after it each reason that a message added while
// the read was in flight (owedSince). A good one clears only the reasons its stateSeq answers
// (read-rule-cases row 5.1). readWhole empties the skipped record as the read begins, so each
// sequence the record holds is of a message that judgeCrowded acknowledged unapplied past
// stateMaxWaiting while the read was in flight:
//   - Where such a message is of the read's store and carried a sequence above its stateSeq
//     (skippedAbove), its events are past the read's snapshot. The gap read stays owed, whatever
//     reasons were owed when the read was decided, and so does each reason added while the read
//     was in flight.
//   - Where such a message is of another store, one the cursor the read leaves does not drop
//     (skippedElsewhere), it came after the read began, so the read's snapshot does not answer the
//     read it calls for. The store read stays owed, and so does each reason added while the read
//     was in flight.
//   - Otherwise the read clears those reasons too. The messages that added them are at or below
//     its snapshot or of a store the cursor drops, but for the one that holds the read, whose
//     redelivery is then judged as any message.
//
// A good read empties the skipped record once it has so decided, and a failed one keeps it.
//
// Where a good read leaves no reason owed, it clears the message that held the read with its count
// of hand-backs. Where it leaves one owed, the message that holds the read, the first message past
// stateMaxWaiting since the read began (judgeCrowded), holds that reason, so it stays the holder
// with its count. A failed read keeps the message that holds the read with its count: the holder
// comes back to make the read again ReadRetryAfter after this one began, until it has gone back
// stateMaxHolds times (readFor). Then the cursor names no holder, and the read waits for the
// project's next message, boot, settings change or take-over. The holder and its count go only
// with a good read that leaves no reason owed, or with the message: its release after
// stateMaxHolds hand-backs (owe), or a write of the setting that resets the cursor
// (settingWritten).
//
// A good read sets the cursor to its stateSeq, its stateId and its schemaVersion. A failed read
// keeps the cursor's sequence, store and schema version where c follows the same daemon, and sets
// none where it does not. A good read whose state serves a stateRun sets the cursor's run to it:
// the run of the daemon that served the read, which is the run of every message that daemon run
// publishes. So a boot of that run at or below the read's stateSeq, one that reaches Dispatch
// after the read, is one the read holds and not a daemon whose counter went back
// (stateMessage.unfollowed). Any other read keeps the cursor's run where c follows the same
// daemon, and sets none where it does not.
//
// Every read, a good starting read's included, sets last_read_started_at to the read's start, and
// the next read waits ReadRetryAfter after it (readDueIn). readWhole wrote that start before the
// call to the daemon already, so a read that a stop cuts short keeps it too (read-rule-cases row
// 7.6), and writeCursor keeps the later of the two. So the minute of a read the daemon is slow to
// answer, or never answers, counts from when the read began and not from when it ended.
// And a gap or a boot that a message calls for right after a good starting read is made when that
// minute ends, through the delay of the message that holds it (read-rule-cases v11).
//
// A good read whose store is the one a write recorded (isPrevious) clears that store and its URL
// (previous_state_id, previous_state_url): the setting's daemon is the daemon the setting named
// before, behind another URL, so its messages are taken from the read on as every message of the
// cursor's store is. Every other read keeps both (forgets). A good read takes its own store out of
// the stores marked as no daemon of the project (foreign_state_ids). A good read that a message of
// another store made (owed.store), and that finds the cursor's store unchanged, marks that
// message's store (marking): the store's later messages are finished unapplied and call for no
// read of their own.
func (c cursor) afterRead(owed owedRead, read *wholeRead) cursor {
	next := c
	next.exists, next.stateURL, next.lastReadStartedAt = true, read.stateURL, &read.started
	if !c.follows(read.stateURL) {
		next.run, next.lastSeq, next.stateID, next.schemaVersion = nil, nil, nil, nil
	}
	since := owedSince(c.readReasons, owed.clears)
	if read.failure != nil {
		next.readReasons = append([]string{ReadReasonRetry}, without(since, ReadReasonRetry)...)
		return next
	}
	next.lastSeq, next.stateID, next.schemaVersion = read.stateSeq, read.stateID, read.schemaVersion
	if read.stateRun != nil {
		next.run = read.stateRun
	}
	if c.isPrevious(*read.stateID) {
		next.previousStateID, next.previousStateURL = nil, nil
	}
	next.foreignStateIDs = without(c.foreignStateIDs, *read.stateID)
	if owed.store != "" && c.follows(read.stateURL) && c.holdsStore(*read.stateID) {
		next.foreignStateIDs = next.marking(owed.store)
	}
	var more []string
	if c.skippedAbove(*read.stateID, *read.stateSeq) {
		more = append(more, ReadReasonGap)
	}
	if c.skippedElsewhere(next) {
		more = append(more, ReadReasonStore)
	}
	next.readReasons = []string{}
	if len(more) > 0 {
		next.readReasons = with(since, more...)
	}
	if len(next.readReasons) == 0 {
		next.readHolder, next.readHolds = nil, 0
	}
	next.skippedStateIDs, next.skippedSeqs = []string{}, []int64{}
	return next
}
