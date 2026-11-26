package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

const (
	// defaultNakDelayCap is the longest wait before a failed message's next delivery (nakDelay):
	// a message whose cause clears is applied within 5 minutes of the fix, and an hour of failure
	// costs about 20 deliveries rather than the 3,600 a flat second costs. The longest run measured
	// in production, 850 deliveries of one message in 15.4 minutes, becomes about 11.
	defaultNakDelayCap = 5 * time.Minute
	// defaultGiveUpDeliveries is the delivery count a failing message is given up at once its first
	// failure is defaultGiveUpAfter old (giveUpDue): a message that only waits out GitHub's rate
	// limit is then given up after 20 such waits, and not at its second delivery after one long
	// wait.
	defaultGiveUpDeliveries = 21
	// defaultGiveUpAfter is how old a failing message's first failure is before it is given up:
	// longer than a PostgreSQL failover or a daemon restart, which take minutes, and equal to
	// GitHub's primary rate-limit window, the cap githubrest's retryAfter puts on a wait it reads
	// from the reset time. With defaultNakDelayCap and a NakDelay of 1 s the count and the hour
	// meet at the 21st delivery, about 64 minutes after the first.
	defaultGiveUpAfter = time.Hour
	// defaultRetentionMargin is how long before the stream would delete a message its next
	// delivery has to come for the daemon not to give the message up at once (retentionEnds). The
	// give-up's commit and termination take milliseconds; the minute covers the server's clock,
	// which stamps the stored time and deletes by it, standing apart from the daemon's.
	defaultRetentionMargin = time.Minute
	// failureSweepEvery is how often the sweep (failureSweep) reads each consumer's ack floor.
	// It gives up an open failure at its second pass below the floor, so a message the consumer
	// moved past is given up within two of these and one floor read (consumerLagBound), under the
	// 5 minutes R1 of dispatch://INFRA-1575 allows; a pass costs one CONSUMER.INFO and one indexed
	// read per consumer.
	failureSweepEvery = time.Minute
	// stopGiveUpBound bounds the give-up a daemon stop makes of a message the stream would delete
	// before the daemon is back (consumerRun.failed): one transaction of a few statements, which
	// takes milliseconds, so a stop waits no longer than this for a PostgreSQL that does not answer.
	stopGiveUpBound = 2 * time.Second
)

const (
	// giveUpLine is the one line at error level each give-up writes.
	giveUpLine = "intake: a message was given up"
	// giveUpErrorInFull is the message of the lines storetext.LogWhole writes of a give-up's
	// error, which the record and giveUpLine cut at storetext.FarSideErrorRunes.
	giveUpErrorInFull = "intake: a given-up message's error in full; the record and the give-up line keep its start"
)

// nakDelay is how long JetStream waits before it delivers again a message whose delivery
// delivered failed: spec.NakDelay doubled for each delivery before this one while that is below
// spec.NakDelayCap, and the cap once it reaches it, so no delivery count overflows the doubling. A
// count of 0, a delivery whose metadata did not parse, waits the cap.
func nakDelay(delivered uint64, spec ConsumerSpec) time.Duration {
	if delivered == 0 {
		return spec.NakDelayCap
	}
	delay := spec.NakDelay
	for range delivered - 1 {
		if delay > spec.NakDelayCap-delay {
			return spec.NakDelayCap
		}
		delay *= 2
	}
	return min(delay, spec.NakDelayCap)
}

// retryDelay is the nak delay of delivery delivered, which failed with err: nakDelay, or the wait
// a RetryLater in err names when that is longer, whole, with no cap.
func retryDelay(delivered uint64, spec ConsumerSpec, err error) time.Duration {
	delay := nakDelay(delivered, spec)
	var later *RetryLater
	if errors.As(err, &later) && later.After > delay {
		delay = later.After
	}
	return delay
}

// giveUpDue says whether a decoded message JetStream has delivered delivered times, whose first
// failure was at firstFailed, is given up at now: both the count and the hour are reached.
func giveUpDue(delivered uint64, firstFailed, now time.Time, spec ConsumerSpec) bool {
	return delivered >= uint64(spec.GiveUpDeliveries) && now.Sub(firstFailed) >= spec.GiveUpAfter
}

// retentionEnds says whether the stream, which keeps a message for maxAge from stored, would delete
// it, less margin, before its next delivery at next. A stream that keeps messages for ever (a
// MaxAge of 0) deletes none.
func retentionEnds(stored, next time.Time, maxAge, margin time.Duration) bool {
	return maxAge > 0 && stored.Add(maxAge-margin).Before(next)
}

// didNotEndError is the last error of a message given up because delivery did not end in a
// recorded failure: the daemon's own sentence, then the last error a delivery that ended recorded,
// where there is one. The sentence names both ways it arises: the daemon stopped or crashed while
// the delivery ran, or the delivery's failure was handled and its record not written (recordFailure
// could not begin its transaction or note the failure, or noteRedelivery's give-up rolled back).
func didNotEndError(delivery uint64, recorded string) string {
	sentence := fmt.Sprintf("delivery %d did not end in a recorded failure: the daemon stopped or crashed while it ran, or the record of its failure was not written", delivery)
	if recorded == "" {
		return sentence
	}
	return sentence + "; the last recorded error: " + recorded
}

// remadeFailures is, for each durable consumer createConsumer made again at a stored position in
// this boot, the stream sequences of the open failures it had then. Such a consumer delivers
// those messages again with a delivery count of 1, and this is what says one may have an open
// failure for its success to delete (consumerRun.mayHaveFailed). It is read once, as the consumer
// is made again, so the ordinary delivery costs no statement. SetRepositories can make the GitHub
// consumer again while Run reads, so it is kept under mu.
type remadeFailures struct {
	mu   sync.Mutex
	seqs map[remadeFailure]struct{}
}

type remadeFailure struct {
	consumer string
	seq      uint64
}

func (r *remadeFailures) add(consumer string, failures []record.OpenFailure) {
	if len(failures) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seqs == nil {
		r.seqs = map[remadeFailure]struct{}{}
	}
	for _, failure := range failures {
		r.seqs[remadeFailure{consumer, failure.StreamSeq}] = struct{}{}
	}
}

func (r *remadeFailures) has(consumer string, seq uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.seqs[remadeFailure{consumer, seq}]
	return ok
}

// forget drops consumer's seq once its message succeeded and its open failure is gone.
func (r *remadeFailures) forget(consumer string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seqs, remadeFailure{consumer, seq})
}

// readRemadeFailures adds to remade every open failure consumer has, which createConsumer is
// making again at a stored position.
func readRemadeFailures(ctx context.Context, pool store.TxPool, project, consumer string, remade *remadeFailures) error {
	var failures []record.OpenFailure
	if err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		failures, err = record.NewStore().ListPassedFailures(ctx, tx, project, consumer, math.MaxUint64)
		return err
	}); err != nil {
		return fmt.Errorf("read the open failures of durable consumer %s: %w", consumer, err)
	}
	remade.add(consumer, failures)
	return nil
}

// consumerRun is what consumeMessage reads beside the message: the consumers' spec, the
// repositories GitHub intake hears now, the pool, how long the stream keeps a message (its
// MaxAge, 0 for ever), the issue names the consumer read (issueNames), the open failures of the
// consumers made again in this boot (remadeFailures), and the checks settlements whose drop the
// consumer logged (droppedSettlements).
type consumerRun struct {
	spec         ConsumerSpec
	repositories []ghrepo.Repository
	pool         store.TxPool
	maxAge       time.Duration
	names        *issueNames
	remade       *remadeFailures
	dropped      *droppedSettlements
}

// intakeMessage is the message of a delivery whose metadata parsed as the record keeps it, with
// the fact's source and event id where it decoded.
func (r consumerRun) intakeMessage(message jetstream.Msg, metadata *jetstream.MsgMetadata, source, eventID string) record.IntakeMessage {
	return record.IntakeMessage{Project: r.spec.Project, Consumer: metadata.Consumer, Subject: message.Subject(),
		StreamSeq: metadata.Sequence.Stream, StoredAt: metadata.Timestamp, Headers: record.IntakeHeaders(message.Headers()),
		Payload: message.Data(), EventSource: source, EventID: eventID}
}

// mayHaveFailed says whether a delivery's message may have an open failure: JetStream has
// delivered it before, or a consumer made again in this boot delivers a message that had one.
func (r consumerRun) mayHaveFailed(metadata *jetstream.MsgMetadata) bool {
	return metadata != nil && (metadata.NumDelivered >= 2 || r.remade.has(metadata.Consumer, metadata.Sequence.Stream))
}

// position is the stream position of a delivery, which ApplyFact and a give-up record, carrying
// whether its message may have an open failure; nil for metadata that did not parse.
func (r consumerRun) position(metadata *jetstream.MsgMetadata, mayHaveFailed bool) *streamPosition {
	if metadata == nil {
		return nil
	}
	return &streamPosition{project: r.spec.Project, consumer: metadata.Consumer, seq: metadata.Sequence.Stream,
		time: metadata.Timestamp, openFailure: mayHaveFailed}
}

// cleared is a delivery that succeeded with no transaction, acknowledged: its open failure is
// deleted in a statement of its own. A delete that fails leaves the row to the sweep, which may
// give it up later, and a replay of it answers no fact, which closes it.
func (r consumerRun) cleared(ctx context.Context, metadata *jetstream.MsgMetadata) {
	ctx = store.ForConcurrentWorker(ctx)
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := record.NewStore().ClearIntakeFailure(ctx, tx, r.spec.Project, metadata.Consumer, metadata.Sequence.Stream, metadata.Timestamp)
		return err
	}); err != nil {
		r.spec.Logger.Warn("intake: the open failure of a message acknowledged with no transaction was not deleted; the sweep may give it up",
			"consumer", metadata.Consumer, "stream_seq", metadata.Sequence.Stream, "error", err)
		return
	}
	r.remade.forget(metadata.Consumer, metadata.Sequence.Stream)
}

// undecodable records message, which did not decode with decodeErr, as given up at its delivery
// (RecordUndecodable) and then terminates it, answering whether the termination was sent. A
// record of the same message given up already is a termination that was lost: it is terminated
// with no second record, no second notice count and no second line. A failed write naks it at
// the delay, and a later delivery records and terminates it.
func (r consumerRun) undecodable(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, decodeErr error) bool {
	spec := r.spec
	var attributes []any
	if errors.Is(decodeErr, errIssueNameInNeitherForm) {
		attributes = append(attributes, "neither_form_count", r.names.neither.Add(1))
	}
	delivered := deliveredCount(metadata)
	if metadata == nil {
		nakResolve(spec, message, delivered, "", nakOutcome("a message that does not decode has no metadata to record it under",
			"nak the message that does not decode", decodeErr))
		return false
	}
	terminate, err := r.recordUndecodable(store.ForConcurrentWorker(ctx), message, metadata, decodeErr, attributes)
	if terminate {
		return termMessage(spec.Logger, message)
	}
	nakResolve(spec, message, delivered, "", nakOutcome("record the give-up of a message that does not decode",
		"nak the message that does not decode", errors.Join(err, decodeErr)))
	return false
}

// recordUndecodable is undecodable's transaction: it records message as given up and commits, and
// answers whether the message is to be terminated, recorded now or given up already. The
// termination follows the transaction's end. A record given up already writes nothing, so its
// message is terminated however its transaction ends.
func (r consumerRun) recordUndecodable(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, decodeErr error, attributes []any) (bool, error) {
	now := r.spec.Now()
	givenUp := false
	err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		given, recorded, err := record.NewStore().RecordUndecodable(ctx, tx, r.intakeMessage(message, metadata, "", ""), int(metadata.NumDelivered), decodeErr.Error(), now)
		if err != nil {
			return err
		}
		if !recorded {
			givenUp = true
			return nil
		}
		return r.recordGiveUp(ctx, tx, metadata, given, decodeErr.Error(), attributes...)
	})
	if givenUp {
		return true, nil
	}
	return err == nil, err
}

// noteRedelivery notes a redelivery of a decoded message before its handling (NoteRedelivery)
// and decides from the record whether the handler runs. decided is true when it does not: the
// record was given up already, a termination that was lost, and the message is terminated with
// no handling; or the delivery before this one did not end in a recorded failure and the bound is
// reached, and the message is given up now, or naked at the delay when that give-up does not
// commit, since the handler that did not end would end the daemon again. handled is then whether
// the termination was sent. A note that fails decides nothing, and the handler runs. A record
// given up already writes nothing, so its message is terminated however its transaction ends.
func (r consumerRun) noteRedelivery(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, decoded decodedMessage) (decided, handled bool) {
	spec := r.spec
	delivered := metadata.NumDelivered
	ctx = store.ForConcurrentWorker(ctx)
	now := spec.Now()
	var givenUp, givingUp bool
	err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		failure, err := record.NewStore().NoteRedelivery(ctx, tx, r.intakeMessage(message, metadata, decoded.Source, decoded.EventID), int(delivered), now)
		if err != nil {
			return err
		}
		switch {
		case failure.GivenUp():
			givenUp = true
		case uint64(failure.EndedDelivery) < delivered-1 && giveUpDue(delivered-1, failure.FirstFailedAt, now, spec):
			givingUp = true
			return r.giveUpOpen(ctx, tx, metadata, failure.ID, record.GiveUpDidNotEnd, didNotEndError(delivered-1, failure.LastError), now)
		}
		return nil
	})
	switch {
	case givenUp:
		return true, termMessage(spec.Logger, message)
	case givingUp && err != nil:
		nakResolve(spec, message, delivered, decoded.EventID, nakOutcome("give up a message whose delivery did not end",
			"nak the message whose give-up was not recorded", err))
		return true, false
	case givingUp:
		return true, termMessage(spec.Logger, message)
	case err != nil:
		r.noteFailed(message, decoded, "note a redelivery", err)
	}
	return false, false
}

// failed decides what becomes of a decoded message whose handling asked for a nak: outcome names
// the failure. A delivery a daemon stop ended (ctx done) is naked at the delay with no write, so
// the next delivery finds it did not end; but a message the stream would delete before its next
// delivery is given up all the same, on a context the stop does not end, bounded at
// stopGiveUpBound. Any other failure is recorded (NoteFailure), and the message given up in the
// same transaction once the bound is reached or the stream would delete it before its next
// delivery, and naked at the delay otherwise. A record given up already is a message a consumer
// made again at an older stored time delivers once more: it is terminated with the record left
// as it is. A write that fails naks the message at the delay. It answers whether the message left
// its consumer: terminated, with the termination sent.
func (r consumerRun) failed(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, decoded decodedMessage, outcome messageOutcome) bool {
	spec := r.spec
	delivered := deliveredCount(metadata)
	if metadata == nil {
		nakResolve(spec, message, delivered, decoded.EventID, outcome)
		return false
	}
	now := spec.Now()
	retention := retentionEnds(metadata.Timestamp, now.Add(retryDelay(delivered, spec, outcome.err)), r.maxAge, spec.RetentionMargin)
	stopping := ctx.Err() != nil
	if stopping {
		if !retention {
			nakResolve(spec, message, delivered, decoded.EventID, outcome)
			return false
		}
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopGiveUpBound)
		defer cancel()
		ctx = detached
	}
	ctx = store.ForConcurrentWorker(ctx)
	terminate, err := r.recordFailure(ctx, message, metadata, decoded, outcome, now, retention, stopping)
	if err != nil {
		r.noteFailed(message, decoded, "record a failed delivery", err)
	}
	if terminate {
		return termMessage(spec.Logger, message)
	}
	nakResolve(spec, message, delivered, decoded.EventID, outcome)
	return false
}

// recordFailure is failed's transaction: it records the failure and gives the message up where
// the bound or the retention says so, and answers whether the message is to be terminated, given
// up now or given up already. The termination follows the transaction's end. A give-up that does
// not commit takes the failure's write back with it, so the failure is recorded again alone
// (noteFailureAlone): the next delivery then reads this one as ended with its error, rather than
// giving the message up as a delivery that did not end. A delivery a stop ended is left with no
// write, as failed leaves such a delivery when the stream keeps its message. A record given up
// already writes nothing, so its message is terminated however its transaction ends.
func (r consumerRun) recordFailure(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, decoded decodedMessage,
	outcome messageOutcome, now time.Time, retention, stopping bool) (bool, error) {
	spec := r.spec
	delivered := metadata.NumDelivered
	lastError := outcome.err.Error()
	failing := r.intakeMessage(message, metadata, decoded.Source, decoded.EventID)
	var givenUp, givingUp bool
	err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		failure, err := record.NewStore().NoteFailure(ctx, tx, failing, int(delivered), lastError, now)
		if err != nil {
			return err
		}
		var reason record.IntakeGiveUpReason
		switch {
		case failure.GivenUp():
			givenUp = true
			return nil
		case !stopping && giveUpDue(delivered, failure.FirstFailedAt, now, spec):
			reason = record.GiveUpBound
		case retention:
			reason = record.GiveUpRetention
		default:
			return nil
		}
		givingUp = true
		return r.giveUpOpen(ctx, tx, metadata, failure.ID, reason, lastError, now)
	})
	switch {
	case givenUp:
		return true, nil
	case err == nil:
		return givingUp, nil
	case givingUp && !stopping:
		return false, errors.Join(err, r.noteFailureAlone(ctx, failing, int(delivered), lastError, now))
	}
	return false, err
}

// noteFailureAlone records, in a transaction of its own, that delivery delivered of failing, the
// message as recordFailure built it, failed with lastError at now (NoteFailure).
func (r consumerRun) noteFailureAlone(ctx context.Context, failing record.IntakeMessage, delivered int, lastError string, now time.Time) error {
	return store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := record.NewStore().NoteFailure(ctx, tx, failing, delivered, lastError, now)
		return err
	})
}

// giveUpOpen gives up the open failure id in tx for reason, with lastError, at now
// (GiveUpIntakeMessage), and records the give-up with it (recordGiveUp), which commits with tx. A
// record that is no open failure any more is an error, which rolls tx back, and the caller naks
// the message.
func (r consumerRun) giveUpOpen(ctx context.Context, tx pgx.Tx, metadata *jetstream.MsgMetadata, id int64, reason record.IntakeGiveUpReason, lastError string, now time.Time) error {
	given, gaveUp, err := record.NewStore().GiveUpIntakeMessage(ctx, tx, r.spec.Project, id, reason, lastError, now)
	if err != nil {
		return err
	}
	if !gaveUp {
		return fmt.Errorf("intake record %d was no open failure to give up", id)
	}
	return r.recordGiveUp(ctx, tx, metadata, given, lastError)
}

// noteFailed logs that step, a write of the give-up record of a delivery, failed, which leaves the
// message to the nak its handling asks for, or to the handling itself.
func (r consumerRun) noteFailed(message jetstream.Msg, decoded decodedMessage, step string, err error) {
	logMessage(r.spec.Logger, slog.LevelWarn, "intake: the give-up record of a delivery was not written; the delivery goes on without it", message,
		"step", step, "event_id", decoded.EventID, "error", err)
}

// recordGiveUp records in tx, in which given was given up, the stream position of its delivery,
// and registers the hooks its commit runs: the wake of the outbox lane of the notice that counted
// it (wakeOnGiveUps), then the line that logs the give-up with JetStream's delivery count
// (logGiveUp). whole is the error before the record cut it.
func (r consumerRun) recordGiveUp(ctx context.Context, tx pgx.Tx, metadata *jetstream.MsgMetadata, given record.IntakeFailure, whole string, attributes ...any) error {
	if err := r.position(metadata, false).record(ctx, tx); err != nil {
		return err
	}
	wakeOnGiveUps(ctx, r.spec, []record.IntakeFailure{given})
	delivered := deliveredCount(metadata)
	store.OnCommit(ctx, func() { logGiveUp(r.spec.Logger, given, delivered, whole, attributes...) })
	return nil
}

// wakeOnGiveUps registers, on the transaction of ctx in which given were given up, the commit
// hook that wakes the outbox lane of the project's controller notices once, where any of them was
// counted in one (ConsumerSpec.WakeOutbox).
func wakeOnGiveUps(ctx context.Context, spec ConsumerSpec, given []record.IntakeFailure) {
	for _, failure := range given {
		if failure.NoticeID != 0 && spec.WakeOutbox != nil {
			store.OnCommit(ctx, func() { spec.WakeOutbox(spec.Project, record.OutboxKindControllerNotice) })
			return
		}
	}
}

// logGiveUp writes giveUpLine for given, at error, with the record as it keeps it, its subject as
// the state shows it (storetext.Text), and delivered, JetStream's count, then the whole of whole
// through storetext.LogWhole, where the record cut it.
func logGiveUp(logger *slog.Logger, given record.IntakeFailure, delivered uint64, whole string, attributes ...any) {
	logger.Error(giveUpLine, append([]any{
		"project", given.Project, "consumer", given.Consumer, "subject", storetext.Text(given.Subject), "stream_seq", given.StreamSeq,
		"stored_at", given.StoredAt.Format(time.RFC3339Nano), "event_id", given.EventID, "delivered", delivered,
		"first_failed_at", given.FirstFailedAt.Format(time.RFC3339Nano), "reason", string(given.GiveUpReason), "record", given.ID,
		"error", given.LastError,
	}, attributes...)...)
	storetext.LogWhole(logger, giveUpErrorInFull, whole, "record", given.ID, "consumer", given.Consumer, "stream_seq", given.StreamSeq)
}

// termMessage terminates message, which a committed record holds as given up, and reports
// whether the client sent the termination. One that fails is logged: the message comes back, and
// its delivery finds the record given up and terminates it with no second record.
func termMessage(logger *slog.Logger, message jetstream.Msg) bool {
	if err := message.Term(); err != nil {
		logMessage(logger, slog.LevelError, "intake: a given-up message was not terminated; it comes back and is terminated then", message, "error", err)
		return false
	}
	return true
}

// deliveredCount is JetStream's delivery count of a delivery, 0 for metadata that did not parse.
func deliveredCount(metadata *jetstream.MsgMetadata) uint64 {
	if metadata == nil {
		return 0
	}
	return metadata.NumDelivered
}

// failureSweep gives up the open failures a consumer moved past without delivering them again:
// a durable made again past one (createConsumer), or the stream's deletion of its message, both of
// which move the consumer's ack floor to or past it. Each pass reads each consumer's ack floor and
// lists that consumer's open failures at or below it; an open failure the previous pass listed
// too, which no statement wrote since that pass, is given up (SweepPassedFailures). The second
// pass is what spares a message just acknowledged: the floor moves at its acknowledgement, and its
// row's delete follows in the same handling, so by the next pass the row is gone. The lists are
// kept in memory: a restart starts the count again.
type failureSweep struct {
	consumers *Consumers
	previous  map[string]sweptPass
}

// sweptPass is one consumer's last pass: when it began, and the open failures it listed.
type sweptPass struct {
	at     time.Time
	listed map[int64]bool
}

// sweepFailures runs a failureSweep every sweepEvery until ctx ends.
func (c *Consumers) sweepFailures(ctx context.Context) error {
	ctx = store.ForConcurrentWorker(ctx)
	sweep := &failureSweep{consumers: c}
	ticker := time.NewTicker(c.sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			sweep.pass(ctx)
		}
	}
}

// pass is one pass of the sweep over the four durable consumers.
func (s *failureSweep) pass(ctx context.Context) {
	if s.previous == nil {
		s.previous = map[string]sweptPass{}
	}
	project := s.consumers.spec.Project
	for _, name := range []string{dispatchConsumerName(project), githubConsumerName(project), wakeConsumerName(project), readyConsumerName(project)} {
		if pass, ok := s.sweepConsumer(ctx, name); ok {
			s.previous[name] = pass
		}
	}
}

// sweepConsumer sweeps one consumer, answering the pass it made, and false where it could not
// read the floor, list the open failures or give up those it found: the pass before it then
// stands, and the next pass compares against it.
func (s *failureSweep) sweepConsumer(ctx context.Context, name string) (sweptPass, bool) {
	c := s.consumers
	spec := c.spec
	at := spec.Now()
	read, cancel := context.WithTimeout(ctx, consumerLagBound)
	consumer, err := c.stream.Consumer(read, name)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			spec.Logger.Warn("intake: the failure sweep could not read a consumer's ack floor", "consumer", name, "error", err)
		}
		return sweptPass{}, false
	}
	floor := consumer.CachedInfo().AckFloor.Stream
	pass := sweptPass{at: at, listed: map[int64]bool{}}
	if floor == 0 {
		return pass, true
	}
	records := record.NewStore()
	var listed []record.OpenFailure
	if err := store.TransactWith(ctx, c.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var listErr error
		listed, listErr = records.ListPassedFailures(ctx, tx, spec.Project, name, floor)
		return listErr
	}); err != nil {
		if ctx.Err() == nil {
			spec.Logger.Warn("intake: the failure sweep could not list a consumer's open failures", "consumer", name, "error", err)
		}
		return sweptPass{}, false
	}
	previous := s.previous[name]
	var passed []int64
	for _, failure := range listed {
		pass.listed[failure.ID] = true
		if previous.listed[failure.ID] {
			passed = append(passed, failure.ID)
		}
	}
	if len(passed) == 0 {
		return pass, true
	}
	if err := s.giveUpPassed(ctx, passed, previous.at, floor); err != nil {
		if ctx.Err() == nil {
			spec.Logger.Warn("intake: the failure sweep could not give up the open failures a consumer moved past; the next pass tries again",
				"consumer", name, "error", err)
		}
		return sweptPass{}, false
	}
	return pass, true
}

// giveUpPassed gives up passed, the open failures of one consumer that both this pass and the
// previous one, which began at listedAt, listed at or below its ack floor (SweepPassedFailures),
// in one transaction, and logs each give-up with floor, this pass's ack floor, once it commits.
func (s *failureSweep) giveUpPassed(ctx context.Context, passed []int64, listedAt time.Time, floor uint64) error {
	c := s.consumers
	spec := c.spec
	now := spec.Now()
	return store.Transact(ctx, c.pool, func(ctx context.Context, tx pgx.Tx) error {
		given, err := record.NewStore().SweepPassedFailures(ctx, tx, spec.Project, passed, listedAt, now)
		if err != nil {
			return err
		}
		wakeOnGiveUps(ctx, spec, given)
		store.OnCommit(ctx, func() {
			for _, failure := range given {
				logGiveUp(spec.Logger, failure, uint64(failure.Deliveries), failure.LastError, "ack_floor", floor)
			}
		})
		return nil
	})
}
