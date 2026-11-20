package record

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// giveUpNoticeDelay is how long the intake-gave-up notice a give-up opens waits for its publish,
// so the give-ups after it join it rather than wake the controller one by one. Give-ups come in
// bursts of one cause: on 2026-11-18 one project's daemon terminated 2,293 messages that did not
// decode. A minute folds such a burst into a few notices and puts off the controller's first word
// of a give-up by no more than that minute.
const giveUpNoticeDelay = time.Minute

// giveUpNoticeNames bounds the give-ups one notice names; its count goes on past the bound. A name
// is a record id, a consumer and a subject, a few hundred bytes at most, so a hundred keep the
// notice to tens of kilobytes, far below the 1 MiB NATS takes in one message (max_payload), and
// small in the controller's context. The state lists the open records, and a bulk replay or
// discard takes them by consumer and stream sequence rather than by name.
const giveUpNoticeNames = 100

// ConsumerMovedPastError is the error a sweep's give-up keeps (SweepPassedFailures): the message
// stopped coming back because its consumer's ack floor moved to or past it, which a durable made
// again past it or the stream's deletion of it does, and not because its handling stopped failing.
const ConsumerMovedPastError = "the consumer moved past it without delivering it again"

// IntakeGiveUpReason is why the intake gave a message up (intake_failures.give_up_reason).
type IntakeGiveUpReason string

const (
	// GiveUpBound is the count of deliveries and the hour since the first failure both reached.
	GiveUpBound IntakeGiveUpReason = "bound"
	// GiveUpDidNotEnd is a delivery whose handling never ended, found by the next delivery once the
	// bound was reached, which gives the message up before its handler runs again.
	GiveUpDidNotEnd IntakeGiveUpReason = "did not end"
	// GiveUpRetention is a message the stream would delete before its next delivery.
	GiveUpRetention IntakeGiveUpReason = "retention"
	// GiveUpUndecodable is a message that did not decode, given up at its first delivery.
	GiveUpUndecodable IntakeGiveUpReason = "undecodable"
	// GiveUpConsumerMovedPast is an open failure the consumer moved past without delivering it
	// again, which the sweep finds.
	GiveUpConsumerMovedPast IntakeGiveUpReason = "consumer moved past"
)

// IntakeClosedHow is how a give-up's latest close ended it (intake_failures.closed_how): a replay
// that applied its fact, found it applied already, or found no fact in it, or a discard.
type IntakeClosedHow string

const (
	IntakeApplied        IntakeClosedHow = "applied"
	IntakeAlreadyApplied IntakeClosedHow = "already applied"
	IntakeNoFact         IntakeClosedHow = "no fact"
	IntakeDiscarded      IntakeClosedHow = "discarded"
)

// IntakeMessage is one message of a durable consumer as the record keeps it: whose project it is,
// the consumer that delivered it, its subject, its stream sequence and the time JetStream stored
// it, which together with the consumer are its key, its header block (IntakeHeaders, nil for
// none) and its payload, both kept byte for byte, and the fact's source and event id, empty for a
// message that did not decode.
type IntakeMessage struct {
	Project     string
	Consumer    string
	Subject     string
	StreamSeq   uint64
	StoredAt    time.Time
	Headers     []byte
	Payload     []byte
	EventSource string
	EventID     string
}

// IntakeFailure is one row of intake_failures. It is an open failure while GivenUpAt is zero, and
// a give-up after: open while ClosedAt is zero, and closed by a replay or a discard after.
// ClosedHow, ClosedAt and ClosedBy are its latest close; DiscardedAt, DiscardedBy and
// DiscardReason its discard, which a replay's close after it keeps. NoticeID is the outbox row of
// the notice that counted the give-up, 0 where no controller was registered. ReplayClaimedAt is
// the claim of the replay that runs it, zero for none. A zero time is a null column.
type IntakeFailure struct {
	ID int64
	IntakeMessage
	FirstFailedAt   time.Time
	UpdatedAt       time.Time
	Deliveries      int
	EndedDelivery   int
	LastError       string
	GivenUpAt       time.Time
	GiveUpReason    IntakeGiveUpReason
	NoticeID        int64
	ReplayClaimedAt time.Time
	ClosedAt        time.Time
	ClosedHow       IntakeClosedHow
	ClosedBy        string
	DiscardedAt     time.Time
	DiscardedBy     string
	DiscardReason   string
}

// GivenUp says whether the intake gave the message up: a record, where false is an open failure.
func (f IntakeFailure) GivenUp() bool { return !f.GivenUpAt.IsZero() }

// OpenFailure is an open failure as the sweep lists it (ListPassedFailures): its id, where it
// stands on the stream, and when a statement last wrote it.
type OpenFailure struct {
	ID        int64
	StreamSeq uint64
	StoredAt  time.Time
	UpdatedAt time.Time
}

// IntakeGiveUpCount is one consumer's open give-ups: how many, and the highest stream sequence
// among them, which a bulk replay or discard of all of them names.
type IntakeGiveUpCount struct {
	Consumer  string
	Open      int
	NewestSeq uint64
}

// IntakeGiveUps is what the state reads of a project's open give-ups (OpenIntakeGiveUps).
type IntakeGiveUps struct {
	Consumers []IntakeGiveUpCount
	Newest    []IntakeFailure
}

// IntakeSelector names the give-ups a replay or a discard takes: one record by its id, or every
// open record of a consumer at or below a stream sequence. It names exactly one form (Validate).
type IntakeSelector struct {
	ID         int64
	Consumer   string
	ThroughSeq uint64
}

// Validate refuses a selector that names neither form or both, a negative id, and a bulk form
// with no consumer or a stream sequence below 1.
func (s IntakeSelector) Validate() error {
	bulk := s.Consumer != "" || s.ThroughSeq != 0
	switch {
	case s.ID != 0 && bulk:
		return errors.New("a selector names one give-up id or a consumer and a stream sequence, not both")
	case s.ID < 0:
		return fmt.Errorf("a give-up id is positive, not %d", s.ID)
	case s.ID > 0:
		return nil
	case !bulk:
		return errors.New("a selector names one give-up id or a consumer and a stream sequence")
	case s.Consumer == "" || s.ThroughSeq < 1:
		return errors.New("a selector of a consumer's give-ups names the consumer and a stream sequence of 1 or more")
	}
	return nil
}

// throughSeq is the selector's stream sequence as the bigint column compares it.
func (s IntakeSelector) throughSeq() int64 {
	return int64(min(s.ThroughSeq, math.MaxInt64))
}

// IntakeReplayClaim is what a replay's claim took (ClaimIntakeReplay): the records, in stream
// order, and how many open records of the selector it did not take, past the caller's batch or
// under another replay's live claim.
type IntakeReplayClaim struct {
	Records   []IntakeFailure
	Unclaimed int
}

// ErrIntakeGiveUpNotFound is a selector's id that names no give-up of the project: no record, an
// open failure, which is no give-up, or another project's record.
var ErrIntakeGiveUpNotFound = errors.New("no give-up of this project has that id")

// ErrIntakeGiveUpBeingReplayed is a selector's id whose record a live replay claims.
var ErrIntakeGiveUpBeingReplayed = errors.New("a replay running now holds that give-up")

// IntakeGiveUpClosedError is a selector's id whose record is closed in a way the action may not
// take: a replay of a record a replay closed, or a discard of a closed one. It names how and when.
type IntakeGiveUpClosedError struct {
	ID  int64
	How IntakeClosedHow
	At  time.Time
}

func (e *IntakeGiveUpClosedError) Error() string {
	return fmt.Sprintf("give-up %d is closed: %s at %s", e.ID, e.How, e.At.UTC().Format(time.RFC3339))
}

// IntakeHeaders is a message's header block as intake_failures.headers keeps it, in the NATS/1.0
// form: "NATS/1.0\r\n", then "Key: value\r\n" for each key in sorted order and each of its values
// in order, then "\r\n". Each key and value is written as its raw bytes, so a header is kept byte
// for byte and no encoding replaces a byte that is no UTF-8. It is nil for a message with no
// header, which the column keeps as null. It takes a nats.Header as it is.
func IntakeHeaders(headers map[string][]string) []byte {
	if len(headers) == 0 {
		return nil
	}
	const version, end = "NATS/1.0\r\n", "\r\n"
	keys := slices.Sorted(maps.Keys(headers))
	size := len(version) + len(end)
	for _, key := range keys {
		for _, value := range headers[key] {
			size += len(key) + len(": ") + len(value) + len(end)
		}
	}
	block := make([]byte, 0, size)
	block = append(block, version...)
	for _, key := range keys {
		for _, value := range headers[key] {
			block = append(block, key...)
			block = append(block, ": "...)
			block = append(block, value...)
			block = append(block, end...)
		}
	}
	return append(block, end...)
}

// The columns of a record as scanIntakeFailure reads them, and the same with its headers and
// payload read as null, for a read that shows neither.
const (
	intakeFailureKey  = `id, project, consumer, subject, stream_seq, stored_at, `
	intakeFailureRest = `, coalesce(event_source, ''), coalesce(event_id, ''), first_failed_at, updated_at, deliveries,
	ended_delivery, last_error, given_up_at, coalesce(give_up_reason, ''), coalesce(notice_id, 0), replay_claimed_at, closed_at,
	coalesce(closed_how, ''), coalesce(closed_by, ''), discarded_at, coalesce(discarded_by, ''), coalesce(discard_reason, '')`
	intakeFailureColumns = intakeFailureKey + `headers, payload` + intakeFailureRest
	intakeGiveUpColumns  = intakeFailureKey + `null::bytea, null::bytea` + intakeFailureRest
)

// scanIntakeFailure reads one row of intakeFailureColumns, each time in UTC and each null time
// as the zero time.
func scanIntakeFailure(row scanner) (IntakeFailure, error) {
	var f IntakeFailure
	var seq int64
	var givenUp, claimed, closed, discarded *time.Time
	var reason, how string
	if err := row.Scan(&f.ID, &f.Project, &f.Consumer, &f.Subject, &seq, &f.StoredAt, &f.Headers, &f.Payload, &f.EventSource,
		&f.EventID, &f.FirstFailedAt, &f.UpdatedAt, &f.Deliveries, &f.EndedDelivery, &f.LastError, &givenUp, &reason, &f.NoticeID,
		&claimed, &closed, &how, &f.ClosedBy, &discarded, &f.DiscardedBy, &f.DiscardReason); err != nil {
		return IntakeFailure{}, err
	}
	f.StreamSeq = uint64(seq)
	f.StoredAt, f.FirstFailedAt, f.UpdatedAt = f.StoredAt.UTC(), f.FirstFailedAt.UTC(), f.UpdatedAt.UTC()
	f.GivenUpAt, f.ReplayClaimedAt, f.ClosedAt, f.DiscardedAt = utcOrZero(givenUp), utcOrZero(claimed), utcOrZero(closed), utcOrZero(discarded)
	f.GiveUpReason, f.ClosedHow = IntakeGiveUpReason(reason), IntakeClosedHow(how)
	return f, nil
}

// utcOrZero is *at in UTC, or the zero time for a null column.
func utcOrZero(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}
	return at.UTC()
}

// messageArgs is message's key and contents as the statements below take them, $1 to $9: the
// project, the consumer, the subject, the stream sequence, the stored time truncated to the
// microseconds PostgreSQL keeps (never rounded up, as intake's recordStreamPosition truncates
// it), the header block, the payload, the event source and the event id. An empty payload is
// written as no bytes rather than as null, which the column refuses: a NATS message may carry
// none.
func messageArgs(message IntakeMessage) []any {
	payload := message.Payload
	if payload == nil {
		payload = []byte{}
	}
	return []any{message.Project, message.Consumer, message.Subject, int64(message.StreamSeq),
		message.StoredAt.Truncate(time.Microsecond), message.Headers, payload, message.EventSource, message.EventID}
}

// noteIntakeFailure runs statement, an insert of message's record that updates an open failure
// of the same message on conflict and leaves one given up as it is, and answers the row it
// returns; where it returns none, the record is given up, and it answers that record as it stands.
func noteIntakeFailure(ctx context.Context, tx pgx.Tx, message IntakeMessage, statement string, args ...any) (IntakeFailure, error) {
	failure, err := scanIntakeFailure(tx.QueryRow(ctx, statement, args...))
	if !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return IntakeFailure{}, fmt.Errorf("note the failure of %s's message %d: %w", message.Consumer, message.StreamSeq, err)
		}
		return failure, nil
	}
	failure, err = scanIntakeFailure(tx.QueryRow(ctx, `select `+intakeFailureColumns+` from intake_failures
		where project = $1 and consumer = $2 and stream_seq = $3 and stored_at = $4`,
		message.Project, message.Consumer, int64(message.StreamSeq), message.StoredAt.Truncate(time.Microsecond)))
	if errors.Is(err, pgx.ErrNoRows) {
		return IntakeFailure{}, fmt.Errorf("note the failure of %s's message %d: another project's record holds its key", message.Consumer, message.StreamSeq)
	}
	if err != nil {
		return IntakeFailure{}, fmt.Errorf("read the record of %s's message %d: %w", message.Consumer, message.StreamSeq, err)
	}
	return failure, nil
}

// NoteRedelivery notes delivery delivered (JetStream's count, 2 or more) of message before its
// handling: it inserts the open failure when the message has none, with the first failure at now,
// which starts the clock for a message whose handling never ended, and otherwise sets the count
// and the time of the note. It answers the record as it stands, so the intake decides from it
// before the handler runs: a record given up already, which this leaves as it is, is a
// termination that was lost.
func (s *Postgres) NoteRedelivery(ctx context.Context, tx pgx.Tx, message IntakeMessage, delivered int, now time.Time) (IntakeFailure, error) {
	return noteIntakeFailure(ctx, tx, message, `insert into intake_failures (project, consumer, subject, stream_seq, stored_at,
		headers, payload, event_source, event_id, first_failed_at, updated_at, deliveries)
		values ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), nullif($9, ''), $10, $10, $11)
		on conflict (consumer, stream_seq, stored_at) do update set updated_at = excluded.updated_at, deliveries = excluded.deliveries
		where intake_failures.project = excluded.project and intake_failures.given_up_at is null
		returning `+intakeFailureColumns, append(messageArgs(message), now, delivered)...)
}

// NoteFailure records that delivery delivered of message failed with lastError, a far side's
// error text kept as storetext.FarSideError keeps it (the caller logs it whole): it inserts the
// open failure at the message's first failed delivery, and sets the count, the delivery that
// ended, the error and the time of the note on it after. It answers the record, whose first
// failure the bound reads; a record given up already it leaves as it is and answers, and the
// intake terminates the message.
func (s *Postgres) NoteFailure(ctx context.Context, tx pgx.Tx, message IntakeMessage, delivered int, lastError string, now time.Time) (IntakeFailure, error) {
	return noteIntakeFailure(ctx, tx, message, `insert into intake_failures (project, consumer, subject, stream_seq, stored_at,
		headers, payload, event_source, event_id, first_failed_at, updated_at, deliveries, ended_delivery, last_error)
		values ($1, $2, $3, $4, $5, $6, $7, nullif($8, ''), nullif($9, ''), $10, $10, $11, $11, $12)
		on conflict (consumer, stream_seq, stored_at) do update set updated_at = excluded.updated_at,
			deliveries = excluded.deliveries, ended_delivery = excluded.ended_delivery, last_error = excluded.last_error
		where intake_failures.project = excluded.project and intake_failures.given_up_at is null
		returning `+intakeFailureColumns, append(messageArgs(message), now, delivered, storetext.FarSideError(lastError))...)
}

// GiveUpIntakeMessage is the whole of a give-up's step in the record, in the transaction that
// decides it: the open failure id of project is marked given up at now for reason, with lastError
// as storetext.FarSideError keeps it, and, where a controller is registered, counted in a notice
// whose outbox id the record keeps (CountIntakeGiveUp). It answers the record as it left it, and
// false, with nothing written, where project has no open failure of that id: one given up
// already, or none.
func (s *Postgres) GiveUpIntakeMessage(ctx context.Context, tx pgx.Tx, project string, id int64, reason IntakeGiveUpReason, lastError string, now time.Time) (IntakeFailure, bool, error) {
	switch reason {
	case GiveUpBound, GiveUpDidNotEnd, GiveUpRetention, GiveUpUndecodable, GiveUpConsumerMovedPast:
	default:
		return IntakeFailure{}, false, fmt.Errorf("give up intake record %d: %q is no reason to give a message up", id, reason)
	}
	failure, err := scanIntakeFailure(tx.QueryRow(ctx, `update intake_failures
		set given_up_at = $3, give_up_reason = $4, last_error = $5, updated_at = $3
		where id = $1 and project = $2 and given_up_at is null
		returning `+intakeFailureColumns, id, project, now, string(reason), storetext.FarSideError(lastError)))
	if errors.Is(err, pgx.ErrNoRows) {
		return IntakeFailure{}, false, nil
	}
	if err != nil {
		return IntakeFailure{}, false, fmt.Errorf("give up intake record %d: %w", id, err)
	}
	given := []IntakeFailure{failure}
	if err := s.countGiveUps(ctx, tx, project, given, now); err != nil {
		return IntakeFailure{}, false, err
	}
	return given[0], true, nil
}

// RecordUndecodable records message, which did not decode, as given up at its delivery delivered
// with decodeError, holding its payload and headers and no event id, and counts it in a notice as
// a give-up does. An open failure of the same message is given up with it. It answers the record
// and true; and false, with nothing written, for a message recorded as given up already, which is
// a termination that was lost: no second record and no second count.
func (s *Postgres) RecordUndecodable(ctx context.Context, tx pgx.Tx, message IntakeMessage, delivered int, decodeError string, now time.Time) (IntakeFailure, bool, error) {
	args := messageArgs(message)[:7]
	failure, err := scanIntakeFailure(tx.QueryRow(ctx, `insert into intake_failures (project, consumer, subject, stream_seq, stored_at,
		headers, payload, first_failed_at, updated_at, deliveries, ended_delivery, last_error, given_up_at, give_up_reason)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9, $9, $10, $8, $11)
		on conflict (consumer, stream_seq, stored_at) do update set given_up_at = excluded.given_up_at,
			give_up_reason = excluded.give_up_reason, last_error = excluded.last_error, deliveries = excluded.deliveries,
			ended_delivery = excluded.ended_delivery, updated_at = excluded.updated_at
		where intake_failures.project = excluded.project and intake_failures.given_up_at is null
		returning `+intakeFailureColumns,
		append(args, now, delivered, storetext.FarSideError(decodeError), string(GiveUpUndecodable))...))
	if errors.Is(err, pgx.ErrNoRows) {
		return IntakeFailure{}, false, nil
	}
	if err != nil {
		return IntakeFailure{}, false, fmt.Errorf("record %s's undecodable message %d: %w", message.Consumer, message.StreamSeq, err)
	}
	given := []IntakeFailure{failure}
	if err := s.countGiveUps(ctx, tx, message.Project, given, now); err != nil {
		return IntakeFailure{}, false, err
	}
	return given[0], true, nil
}

// countGiveUps counts each of failures, project's records given up in this transaction, in the
// project's notice where a controller is registered, and keeps that notice's id on each record and
// in failures. With no controller registered it writes nothing, and the records keep no notice for
// good: a controller that registers later reads them in the state.
func (s *Postgres) countGiveUps(ctx context.Context, tx pgx.Tx, project string, failures []IntakeFailure, now time.Time) error {
	if len(failures) == 0 {
		return nil
	}
	registered, err := s.ControllerRegistered(ctx, tx, project)
	if err != nil || !registered {
		return err
	}
	for i := range failures {
		failure := &failures[i]
		notice, err := s.CountIntakeGiveUp(ctx, tx, project, NoticeGiveUp{ID: failure.ID, Consumer: failure.Consumer, Subject: failure.Subject}, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update intake_failures set notice_id = $2, updated_at = $3 where id = $1`, failure.ID, notice, now); err != nil {
			return fmt.Errorf("keep notice %d on intake record %d: %w", notice, failure.ID, err)
		}
		failure.NoticeID, failure.UpdatedAt = notice, now
	}
	return nil
}

// CountIntakeGiveUp counts giveUp in project's intake-gave-up notice and answers that notice's
// outbox id. It takes the project's transaction-scoped lock first, so the give-ups of the three
// consumers, each in a transaction of its own, join one row rather than each opening one. The
// notice it joins is the oldest of the project's that no lease holds: its count rises by one, and
// it names giveUp while it names fewer than giveUpNoticeNames. A leased row is being published and
// a count raised in it may never reach the controller, so with none unleased it opens a row on the
// project key, due giveUpNoticeDelay after now, counting and naming giveUp. A row the outbox gave
// back to retry holds no lease, so it takes give-ups again rather than leaving one more row behind.
func (s *Postgres) CountIntakeGiveUp(ctx context.Context, tx pgx.Tx, project string, giveUp NoticeGiveUp, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('legion.intake_give_up_notice:' || $1::text))`, project); err != nil {
		return 0, fmt.Errorf("lock %s's intake-gave-up notice: %w", project, err)
	}
	row, err := scanOutbox(tx.QueryRow(ctx, `select `+outboxColumns+` from outbox
		where kind = $1 and split_part(issue, '-', 1) = $2 and payload->>'kind' = $3 and lease_until is null
		order by id limit 1 for update`, string(OutboxKindControllerNotice), project, string(IntakeGaveUpNotice)))
	if errors.Is(err, pgx.ErrNoRows) {
		opened, err := NewOutboxRow(project, ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{giveUp}}, now.Add(giveUpNoticeDelay))
		if err != nil {
			return 0, fmt.Errorf("open %s's intake-gave-up notice: %w", project, err)
		}
		return s.EnqueueID(ctx, tx, opened)
	}
	if err != nil {
		return 0, fmt.Errorf("read %s's intake-gave-up notice: %w", project, err)
	}
	payload, err := DecodeOutboxPayload(row)
	if err != nil {
		return 0, fmt.Errorf("read %s's intake-gave-up notice: %w", project, err)
	}
	notice, _ := payload.(ControllerNotice)
	notice.Count++
	if len(notice.GiveUps) < giveUpNoticeNames {
		notice.GiveUps = append(notice.GiveUps, giveUp)
	}
	counted, err := NewOutboxRow(row.Issue, notice, row.NextAt)
	if err != nil {
		return 0, fmt.Errorf("count a give-up in %s's intake-gave-up notice %d: %w", project, row.ID, err)
	}
	if _, err := tx.Exec(ctx, `update outbox set payload = $2 where id = $1`, row.ID, string(counted.Payload)); err != nil {
		return 0, fmt.Errorf("count a give-up in %s's intake-gave-up notice %d: %w", project, row.ID, err)
	}
	return row.ID, nil
}

// ClearIntakeFailure deletes project's open failure of the message consumer delivered at
// streamSeq, stored at storedAt, once a delivery of it succeeded. A record given up stays. It
// answers whether it deleted one.
func (s *Postgres) ClearIntakeFailure(ctx context.Context, tx pgx.Tx, project, consumer string, streamSeq uint64, storedAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `delete from intake_failures
		where project = $1 and consumer = $2 and stream_seq = $3 and stored_at = $4 and given_up_at is null`,
		project, consumer, int64(streamSeq), storedAt.Truncate(time.Microsecond))
	if err != nil {
		return false, fmt.Errorf("clear the open failure of %s's message %d: %w", consumer, streamSeq, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListPassedFailures is project's open failures of consumer at or below throughSeq, in stream
// order: what the sweep lists at or below the consumer's ack floor, and, with the highest
// sequence, every open failure of a consumer made again.
func (s *Postgres) ListPassedFailures(ctx context.Context, tx pgx.Tx, project, consumer string, throughSeq uint64) ([]OpenFailure, error) {
	rows, err := tx.Query(ctx, `select id, stream_seq, stored_at, updated_at from intake_failures
		where project = $1 and consumer = $2 and stream_seq <= $3 and given_up_at is null
		order by stream_seq, stored_at, id`, project, consumer, int64(min(throughSeq, math.MaxInt64)))
	if err != nil {
		return nil, fmt.Errorf("list the open failures of %s through %d: %w", consumer, throughSeq, err)
	}
	failures, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenFailure, error) {
		var failure OpenFailure
		var seq int64
		err := row.Scan(&failure.ID, &seq, &failure.StoredAt, &failure.UpdatedAt)
		failure.StreamSeq, failure.StoredAt, failure.UpdatedAt = uint64(seq), failure.StoredAt.UTC(), failure.UpdatedAt.UTC()
		return failure, err
	})
	if err != nil {
		return nil, fmt.Errorf("list the open failures of %s through %d: %w", consumer, throughSeq, err)
	}
	return failures, nil
}

// SweepPassedFailures gives up, at now, each of project's open failures among ids that no
// statement wrote since listedAt, the sweep's previous pass, which listed them at or below the
// consumer's ack floor too: the reason is GiveUpConsumerMovedPast and the error
// ConsumerMovedPastError, and each is counted in a notice as a give-up is. A failure written since
// is one a delivery reached, which the next pass lists again or finds gone. It answers the records
// it gave up, in stream order.
func (s *Postgres) SweepPassedFailures(ctx context.Context, tx pgx.Tx, project string, ids []int64, listedAt, now time.Time) ([]IntakeFailure, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `update intake_failures
		set given_up_at = $3, give_up_reason = $4, last_error = $5, updated_at = $3
		where project = $1 and id = any($2) and given_up_at is null and updated_at < $6
		returning `+intakeFailureColumns, project, ids, now, string(GiveUpConsumerMovedPast), ConsumerMovedPastError, listedAt)
	if err != nil {
		return nil, fmt.Errorf("give up %s's open failures its consumers moved past: %w", project, err)
	}
	given, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntakeFailure, error) { return scanIntakeFailure(row) })
	if err != nil {
		return nil, fmt.Errorf("give up %s's open failures its consumers moved past: %w", project, err)
	}
	slices.SortFunc(given, inStreamOrder)
	if err := s.countGiveUps(ctx, tx, project, given, now); err != nil {
		return nil, err
	}
	return given, nil
}

// inStreamOrder orders records by consumer, stream sequence and stored time, then id.
func inStreamOrder(a, b IntakeFailure) int {
	return cmp.Or(cmp.Compare(a.Consumer, b.Consumer), cmp.Compare(a.StreamSeq, b.StreamSeq), a.StoredAt.Compare(b.StoredAt), cmp.Compare(a.ID, b.ID))
}

// OpenIntakeGiveUps is what the state shows of project's open give-ups: each consumer's count and
// highest stream sequence, by consumer, and the newest of them, at most newest, latest given up
// first. The records are read without their headers and payload, which the state does not show.
func (s *Postgres) OpenIntakeGiveUps(ctx context.Context, tx pgx.Tx, project string, newest int) (IntakeGiveUps, error) {
	rows, err := tx.Query(ctx, `select consumer, count(*), max(stream_seq) from intake_failures
		where project = $1 and given_up_at is not null and closed_at is null
		group by consumer order by consumer`, project)
	if err != nil {
		return IntakeGiveUps{}, fmt.Errorf("count %s's open give-ups: %w", project, err)
	}
	consumers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntakeGiveUpCount, error) {
		var count IntakeGiveUpCount
		var seq int64
		err := row.Scan(&count.Consumer, &count.Open, &seq)
		count.NewestSeq = uint64(seq)
		return count, err
	})
	if err != nil {
		return IntakeGiveUps{}, fmt.Errorf("count %s's open give-ups: %w", project, err)
	}
	rows, err = tx.Query(ctx, `select `+intakeGiveUpColumns+` from intake_failures
		where project = $1 and given_up_at is not null and closed_at is null
		order by given_up_at desc, id desc limit $2`, project, max(newest, 0))
	if err != nil {
		return IntakeGiveUps{}, fmt.Errorf("list %s's newest open give-ups: %w", project, err)
	}
	listed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntakeFailure, error) { return scanIntakeFailure(row) })
	if err != nil {
		return IntakeGiveUps{}, fmt.Errorf("list %s's newest open give-ups: %w", project, err)
	}
	return IntakeGiveUps{Consumers: consumers, Newest: listed}, nil
}

// giveUpForUpdate is project's give-up id, locked for the rest of the transaction, and
// ErrIntakeGiveUpNotFound where project has none of that id: no record, or an open failure.
func giveUpForUpdate(ctx context.Context, tx pgx.Tx, project string, id int64) (IntakeFailure, error) {
	failure, err := scanIntakeFailure(tx.QueryRow(ctx, `select `+intakeFailureColumns+` from intake_failures
		where id = $1 and project = $2 for update`, id, project))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !failure.GivenUp() {
		return IntakeFailure{}, ErrIntakeGiveUpNotFound
	}
	if err != nil {
		return IntakeFailure{}, fmt.Errorf("read give-up %d: %w", id, err)
	}
	return failure, nil
}

// closedError is the refusal of failure, which is closed.
func closedError(failure IntakeFailure) error {
	return &IntakeGiveUpClosedError{ID: failure.ID, How: failure.ClosedHow, At: failure.ClosedAt}
}

// claimHolds says whether a replay's claim of failure holds at the moment live: one made at or
// after it.
func claimHolds(failure IntakeFailure, live time.Time) bool {
	return !failure.ReplayClaimedAt.IsZero() && !failure.ReplayClaimedAt.Before(live)
}

// ClaimIntakeReplay claims for a replay, at now, the give-ups of project selector names, and
// commits with the caller's transaction, so the replay runs each outside it. A claim made less
// than staleAfter before now holds its record (the intake passes its HandlerLimit, the longest a
// replay of one record runs); an older one is stale and is taken again. One id takes its record
// when it is open or discarded, and refuses one a replay closed (IntakeGiveUpClosedError), one a
// claim holds (ErrIntakeGiveUpBeingReplayed) and an id of no give-up of project
// (ErrIntakeGiveUpNotFound). The bulk form takes the consumer's open records at or below its
// stream sequence that no claim holds, in stream order, at most batch, and leaves discarded ones.
func (s *Postgres) ClaimIntakeReplay(ctx context.Context, tx pgx.Tx, project string, selector IntakeSelector, batch int, staleAfter time.Duration, now time.Time) (IntakeReplayClaim, error) {
	if err := selector.Validate(); err != nil {
		return IntakeReplayClaim{}, err
	}
	if batch < 1 || staleAfter <= 0 {
		return IntakeReplayClaim{}, fmt.Errorf("claim a replay: a batch of %d and a claim that holds %s take nothing", batch, staleAfter)
	}
	live := now.Add(-staleAfter)
	if selector.ID > 0 {
		failure, err := giveUpForUpdate(ctx, tx, project, selector.ID)
		if err != nil {
			return IntakeReplayClaim{}, err
		}
		if !failure.ClosedAt.IsZero() && failure.ClosedHow != IntakeDiscarded {
			return IntakeReplayClaim{}, closedError(failure)
		}
		if claimHolds(failure, live) {
			return IntakeReplayClaim{}, ErrIntakeGiveUpBeingReplayed
		}
		claimed, err := scanIntakeFailure(tx.QueryRow(ctx, `update intake_failures set replay_claimed_at = $2, updated_at = $2
			where id = $1 returning `+intakeFailureColumns, failure.ID, now))
		if err != nil {
			return IntakeReplayClaim{}, fmt.Errorf("claim give-up %d for a replay: %w", failure.ID, err)
		}
		return IntakeReplayClaim{Records: []IntakeFailure{claimed}}, nil
	}
	// The rows another transaction is claiming are skipped rather than waited on: that claim holds
	// them once it commits, and this one counts them among the records it did not take.
	rows, err := tx.Query(ctx, `update intake_failures set replay_claimed_at = $6, updated_at = $6
		where id in (select id from intake_failures
			where project = $1 and consumer = $2 and stream_seq <= $3 and given_up_at is not null and closed_at is null
			and (replay_claimed_at is null or replay_claimed_at < $4)
			order by stream_seq, stored_at, id limit $5
			for update skip locked)
		returning `+intakeFailureColumns, project, selector.Consumer, selector.throughSeq(), live, batch, now)
	if err != nil {
		return IntakeReplayClaim{}, fmt.Errorf("claim %s's give-ups through %d for a replay: %w", selector.Consumer, selector.ThroughSeq, err)
	}
	claimed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntakeFailure, error) { return scanIntakeFailure(row) })
	if err != nil {
		return IntakeReplayClaim{}, fmt.Errorf("claim %s's give-ups through %d for a replay: %w", selector.Consumer, selector.ThroughSeq, err)
	}
	slices.SortFunc(claimed, inStreamOrder)
	var open int
	if err := tx.QueryRow(ctx, `select count(*) from intake_failures
		where project = $1 and consumer = $2 and stream_seq <= $3 and given_up_at is not null and closed_at is null`,
		project, selector.Consumer, selector.throughSeq()).Scan(&open); err != nil {
		return IntakeReplayClaim{}, fmt.Errorf("count %s's give-ups through %d: %w", selector.Consumer, selector.ThroughSeq, err)
	}
	return IntakeReplayClaim{Records: claimed, Unclaimed: open - len(claimed)}, nil
}

// CloseIntakeGiveUp closes project's give-up id as a replay ended it, how being applied, already
// applied or no fact, by the caller by, at now, and gives back its claim. A discarded record is
// closed again and keeps its discard. It answers false, with nothing written, where project has no
// such record open or discarded.
func (s *Postgres) CloseIntakeGiveUp(ctx context.Context, tx pgx.Tx, project string, id int64, how IntakeClosedHow, by string, now time.Time) (bool, error) {
	switch how {
	case IntakeApplied, IntakeAlreadyApplied, IntakeNoFact:
	default:
		return false, fmt.Errorf("close give-up %d: %q is no close a replay makes", id, how)
	}
	tag, err := tx.Exec(ctx, `update intake_failures
		set closed_at = $4, closed_how = $3, closed_by = $5, replay_claimed_at = null, updated_at = $4
		where id = $1 and project = $2 and given_up_at is not null and (closed_at is null or closed_how = 'discarded')`,
		id, project, string(how), now, by)
	if err != nil {
		return false, fmt.Errorf("close give-up %d as %s: %w", id, how, err)
	}
	return tag.RowsAffected() == 1, nil
}

// FailIntakeReplay keeps lastError, the error of a replay of project's give-up id that failed, as
// storetext.FarSideError keeps it, and gives back its claim; the record stays open or discarded,
// as it stood. It answers false, with nothing written, where project has no such record open or
// discarded.
func (s *Postgres) FailIntakeReplay(ctx context.Context, tx pgx.Tx, project string, id int64, lastError string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `update intake_failures set last_error = $3, replay_claimed_at = null, updated_at = $4
		where id = $1 and project = $2 and given_up_at is not null and (closed_at is null or closed_how = 'discarded')`,
		id, project, storetext.FarSideError(lastError), now)
	if err != nil {
		return false, fmt.Errorf("keep the failed replay of give-up %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DiscardIntakeGiveUps closes, as discarded by the caller by at now for reason, the open give-ups
// of project selector names that no replay's claim holds (staleAfter as ClaimIntakeReplay takes
// it), keeping the discard's time, caller and reason beside the close, and answers their ids in
// stream order. The reason is taken as the store keeps text (storetext.Text), and one that is
// empty is refused before any statement. One id refuses a closed record, discarded or replayed
// (IntakeGiveUpClosedError), one a claim holds (ErrIntakeGiveUpBeingReplayed) and an id of no
// give-up of project (ErrIntakeGiveUpNotFound); the bulk form passes over a claimed record.
func (s *Postgres) DiscardIntakeGiveUps(ctx context.Context, tx pgx.Tx, project string, selector IntakeSelector, by, reason string, staleAfter time.Duration, now time.Time) ([]int64, error) {
	if err := selector.Validate(); err != nil {
		return nil, err
	}
	if reason = storetext.Text(reason); reason == "" {
		return nil, errors.New("a discard names its reason")
	}
	live := now.Add(-staleAfter)
	const discard = `update intake_failures set closed_at = $3, closed_how = 'discarded', closed_by = $4, discarded_at = $3,
		discarded_by = $4, discard_reason = $5, replay_claimed_at = null, updated_at = $3`
	if selector.ID > 0 {
		failure, err := giveUpForUpdate(ctx, tx, project, selector.ID)
		if err != nil {
			return nil, err
		}
		if !failure.ClosedAt.IsZero() {
			return nil, closedError(failure)
		}
		if claimHolds(failure, live) {
			return nil, ErrIntakeGiveUpBeingReplayed
		}
		if _, err := tx.Exec(ctx, discard+` where id = $1 and project = $2`, failure.ID, project, now, by, reason); err != nil {
			return nil, fmt.Errorf("discard give-up %d: %w", failure.ID, err)
		}
		return []int64{failure.ID}, nil
	}
	rows, err := tx.Query(ctx, discard+`
		where project = $1 and consumer = $2 and stream_seq <= $6 and given_up_at is not null and closed_at is null
		and (replay_claimed_at is null or replay_claimed_at < $7)
		returning id, stream_seq, stored_at`, project, selector.Consumer, now, by, reason, selector.throughSeq(), live)
	if err != nil {
		return nil, fmt.Errorf("discard %s's give-ups through %d: %w", selector.Consumer, selector.ThroughSeq, err)
	}
	discarded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IntakeFailure, error) {
		var failure IntakeFailure
		var seq int64
		err := row.Scan(&failure.ID, &seq, &failure.StoredAt)
		failure.StreamSeq = uint64(seq)
		return failure, err
	})
	if err != nil {
		return nil, fmt.Errorf("discard %s's give-ups through %d: %w", selector.Consumer, selector.ThroughSeq, err)
	}
	slices.SortFunc(discarded, inStreamOrder)
	ids := make([]int64, len(discarded))
	for i, failure := range discarded {
		ids[i] = failure.ID
	}
	return ids, nil
}
