package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// replayBatch is the most give-ups one replay claims, all at its start, and then runs one after
// another. Each claim holds for HandlerLimit, ten minutes or more in production, so a hundred
// replays end before the last claim goes stale while each takes under six seconds, as a fact's
// handling does unless one of its reads waits out its AckWait; and the answer, a line per record,
// stays one a person reads. A selector of more open records takes more calls, each answering how
// many are left (ReplayAnswer.Remaining).
const replayBatch = 100

// discardReasonRunes bounds a discard's reason at the bound of a far side's error the record keeps
// (storetext.FarSideErrorRunes): a reason says in a sentence or a link why a message is let go,
// and one longer is refused rather than cut, so the record keeps the whole of what its caller
// wrote.
const discardReasonRunes = storetext.FarSideErrorRunes

// Records is what the replay and the discard of give-ups read of the intake (Consumers.Records):
// the pool the records and every fact are kept in, the consumers' spec, and the repositories
// GitHub intake hears now, which a replay decodes a GitHub event against as a delivery is decoded.
type Records struct {
	Pool         Pool
	Spec         ConsumerSpec
	Repositories func() []ghrepo.Repository
}

// GiveUps replays and discards a project's give-ups, the messages the intake gave up and keeps
// whole (record.IntakeFailure). A replay applies the stored message through the handling a live
// delivery takes, with the handlers a live delivery runs, and publishes nothing to the stream, so
// GiveUps needs no NATS. dropped is the replay's own memory of the checks settlements whose drop
// it logged (droppedSettlements), as a consumer keeps one, so a settlement replayed again names
// its drop once.
type GiveUps struct {
	pool         Pool
	spec         ConsumerSpec
	repositories func() []ghrepo.Repository
	handlers     []Handler
	dropped      *droppedSettlements
}

// NewGiveUps is the replay and the discard of the give-ups of records' project, a replay applying
// each fact with handlers. It refuses records that name no pool or no Repositories.
func NewGiveUps(records Records, handlers ...Handler) (*GiveUps, error) {
	if records.Pool == nil {
		return nil, errors.New("a replay of give-ups needs the Postgres pool they and every fact are kept in")
	}
	if records.Repositories == nil {
		return nil, errors.New("a replay of give-ups needs the repositories GitHub intake hears, which it decodes a GitHub event against")
	}
	spec, err := normalizedSpec(records.Spec)
	if err != nil {
		return nil, err
	}
	return &GiveUps{pool: records.Pool, spec: spec, repositories: records.Repositories, handlers: handlers, dropped: newDroppedSettlements()}, nil
}

// RequestError is a replay or a discard refused for what it asks, before any statement: a
// selector that names neither form or both (record.IntakeSelector.Validate), or a discard whose
// reason is blank or longer than discardReasonRunes.
type RequestError struct{ Err error }

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }

// ReplayOutcome is how the replay of one give-up ended. Its fact applied, its fact was applied
// already, or the message holds no fact, the handling acknowledging it with no transaction: each
// closes the record as the record.IntakeClosedHow of the same name. Or it failed, which leaves the
// record open or discarded as it stood, keeping the new error.
type ReplayOutcome string

const (
	ReplayApplied        = ReplayOutcome(record.IntakeApplied)
	ReplayAlreadyApplied = ReplayOutcome(record.IntakeAlreadyApplied)
	ReplayNoFact         = ReplayOutcome(record.IntakeNoFact)
	ReplayFailed         = ReplayOutcome("failed")
)

// ReplayedGiveUp is one record's answer: its id, its consumer and stream sequence, how its replay
// ended, and, for a replay that failed, the error.
type ReplayedGiveUp struct {
	ID        int64
	Consumer  string
	StreamSeq uint64
	Outcome   ReplayOutcome
	Error     string
}

// ReplayAnswer is a replay's answer: each record it took, in stream order, and Remaining, the open
// records of its selector left after it: those its claim did not take, past replayBatch or under
// another replay's claim, and those it took that were open and failed.
type ReplayAnswer struct {
	Replayed  []ReplayedGiveUp
	Remaining int
}

// Replay replays the give-ups selector names, for caller, whom the record keeps as the one who
// closed it: the operator, or the controller's session. It claims them first (ClaimIntakeReplay),
// in a transaction that commits before any apply, so no other replay and no discard takes them
// while this one runs: one id when it is open or discarded, or a consumer's open records at or
// below a stream sequence, in stream order, at most replayBatch. Then it replays each in turn
// (replayOne), going on past one that fails. A replay runs to its end when its caller goes, on a
// context the caller's end does not cancel, so no record it claimed is left to wait for its claim
// to go stale. It refuses a selector Validate refuses (RequestError), and passes on the claim's
// refusals: an id of no give-up of the project (record.ErrIntakeGiveUpNotFound), one a replay
// closed (record.IntakeGiveUpClosedError), and one another replay holds
// (record.ErrIntakeGiveUpBeingReplayed).
func (g *GiveUps) Replay(ctx context.Context, selector record.IntakeSelector, caller string) (ReplayAnswer, error) {
	if err := selector.Validate(); err != nil {
		return ReplayAnswer{}, &RequestError{Err: err}
	}
	// A replay is one entry point of the daemon's pool, with a hold mark of its own, as one
	// message's handling is (consumeMessage).
	ctx = store.ForConcurrentWorker(context.WithoutCancel(ctx))
	var claim record.IntakeReplayClaim
	if err := g.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		claim, err = record.NewStore().ClaimIntakeReplay(ctx, tx, g.spec.Project, selector, replayBatch, g.spec.HandlerLimit, g.spec.Now())
		return err
	}); err != nil {
		return ReplayAnswer{}, err
	}
	answer := ReplayAnswer{Replayed: make([]ReplayedGiveUp, 0, len(claim.Records)), Remaining: claim.Unclaimed}
	for _, given := range claim.Records {
		replayed := g.replayOne(ctx, given, caller)
		if replayed.Outcome == ReplayFailed && given.ClosedAt.IsZero() {
			answer.Remaining++
		}
		answer.Replayed = append(answer.Replayed, replayed)
	}
	return answer, nil
}

// replayOne replays given, which this replay claimed, within HandlerLimit, and answers how it
// ended. The message is decoded against the repositories GitHub intake hears now, then handled as
// a live delivery is (handleMessage), with no message and no stream position: the read of whether
// a review's or a settlement's pull request is recorded, the preparations, each with the view of
// Dispatch it reads now, and the review's reads all run as they do live, so a late fact is judged
// against the present. An applied fact and a duplicate
// close the record inside ApplyFact's own transaction (withReplayClose); a message the handling
// acknowledges with no transaction, or one that decodes to no fact, closes it in a statement of
// its own (noFact). A failure keeps its error (failed).
func (g *GiveUps) replayOne(ctx context.Context, given record.IntakeFailure, caller string) ReplayedGiveUp {
	replayed := ReplayedGiveUp{ID: given.ID, Consumer: given.Consumer, StreamSeq: given.StreamSeq}
	decoded, err := decodeMessage(given.Subject, g.spec.Project, g.repositories(), given.Payload)
	if err != nil {
		return g.failed(ctx, replayed, err)
	}
	if decoded.Fact == nil {
		return g.noFact(ctx, replayed, caller)
	}
	log := func(level slog.Level, line string, attributes ...any) {
		g.spec.Logger.Log(context.Background(), level, line,
			append(attributes, "subject", storetext.Text(given.Subject), "stream_seq", given.StreamSeq, "replay", given.ID)...)
	}
	work, cancel := context.WithTimeout(withReplayClose(ctx, replayClose{project: g.spec.Project, id: given.ID, by: caller, now: g.spec.Now}),
		g.spec.HandlerLimit)
	defer cancel()
	outcome := handleMessage(work, log, nil, g.spec, g.pool, decoded, g.handlers, g.dropped)
	switch outcome.kind {
	case outcomeNak:
		return g.failed(ctx, replayed, outcome.err)
	case outcomeApplied:
		replayed.Outcome = ReplayApplied
	case outcomeDuplicate:
		logDuplicate(log, decoded, given.Consumer, uint64(given.Deliveries))
		replayed.Outcome = ReplayAlreadyApplied
	case outcomeAcked:
		return g.noFact(ctx, replayed, caller)
	}
	return replayed
}

// noFact closes replayed's record as a replay that found no fact in its message, in a statement
// of its own: nothing was applied, so no fact's transaction carries the close.
func (g *GiveUps) noFact(ctx context.Context, replayed ReplayedGiveUp, caller string) ReplayedGiveUp {
	var closed bool
	err := g.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closed, err = record.NewStore().CloseIntakeGiveUp(ctx, tx, g.spec.Project, replayed.ID, record.IntakeNoFact, caller, g.spec.Now())
		return err
	})
	if err == nil && !closed {
		err = closedByAnotherReplay(replayed.ID)
	}
	if err != nil {
		return g.failed(ctx, replayed, err)
	}
	replayed.Outcome = ReplayNoFact
	return replayed
}

// failed answers replayed as failed with cause, and keeps cause as the record's error, giving
// back the claim (FailIntakeReplay); the record stays open or discarded, as it stood. A write
// that fails is logged, since the answer still says why the replay failed: the record keeps its
// earlier error, and its claim until the claim goes stale.
func (g *GiveUps) failed(ctx context.Context, replayed ReplayedGiveUp, cause error) ReplayedGiveUp {
	replayed.Outcome, replayed.Error = ReplayFailed, cause.Error()
	if err := g.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := record.NewStore().FailIntakeReplay(ctx, tx, g.spec.Project, replayed.ID, replayed.Error, g.spec.Now())
		return err
	}); err != nil {
		g.spec.Logger.Warn("intake: the error of a failed replay was not kept; the give-up keeps its earlier error, and its claim until the claim goes stale",
			"record", replayed.ID, "consumer", replayed.Consumer, "stream_seq", replayed.StreamSeq, "replay_error", replayed.Error, "error", err)
	}
	return replayed
}

// Discard closes, as discarded by caller for reason, the open give-ups selector names that no
// replay's live claim holds, and answers the id of each it closed, in stream order. It applies
// nothing, and every row stays in the table with its discard's time, caller and reason, so a
// replay of it by id later undoes a wrong discard. The reason is taken as the store keeps text
// (storetext.Text), and one that is blank, or longer than discardReasonRunes, is refused before
// any statement (RequestError), as is a selector Validate refuses. One id refuses a closed record,
// discarded or replayed (record.IntakeGiveUpClosedError), one a replay holds
// (record.ErrIntakeGiveUpBeingReplayed) and an id of no give-up of the project
// (record.ErrIntakeGiveUpNotFound); the bulk form passes over a claimed record.
func (g *GiveUps) Discard(ctx context.Context, selector record.IntakeSelector, caller, reason string) ([]int64, error) {
	if err := selector.Validate(); err != nil {
		return nil, &RequestError{Err: err}
	}
	reason = storetext.Text(reason)
	if strings.TrimSpace(reason) == "" {
		return nil, &RequestError{Err: errors.New("a discard names its reason")}
	}
	if runes := utf8.RuneCountInString(reason); runes > discardReasonRunes {
		return nil, &RequestError{Err: fmt.Errorf("a discard's reason is at most %d characters, and this one has %d", discardReasonRunes, runes)}
	}
	var ids []int64
	if err := g.write(store.ForConcurrentWorker(ctx), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ids, err = record.NewStore().DiscardIntakeGiveUps(ctx, tx, g.spec.Project, selector, caller, reason, g.spec.HandlerLimit, g.spec.Now())
		return err
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

// write runs fn in a transaction of its own, bounded at AckWait, the bound each of a handling's
// own reads has: the claim, a close and a kept error are each a statement or two on the record.
func (g *GiveUps) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	bounded, cancel := context.WithTimeout(ctx, g.spec.AckWait)
	defer cancel()
	return pgx.BeginTxFunc(bounded, g.pool, pgx.TxOptions{}, func(tx pgx.Tx) error { return fn(bounded, tx) })
}

// replayClose is the give-up whose stored message a replay applies: project's record id, closed by
// the caller by at the time now reads.
type replayClose struct {
	project string
	id      int64
	by      string
	now     func() time.Time
}

type replayCloseKey struct{}

// withReplayClose is ctx carrying replay, for ApplyFact to close the replayed give-up in the
// fact's own transaction (closeReplayed). The close cannot be a transaction of its own held open
// across the apply, since the daemon's pool refuses a second connection under the hold mark the
// replay takes (store.ErrNestedAcquire), and one after the commit, which can fail, would leave the
// fact applied and its record open.
func withReplayClose(ctx context.Context, replay replayClose) context.Context {
	return context.WithValue(ctx, replayCloseKey{}, replay)
}

// closeReplayed closes, in tx, the give-up whose replay ctx carries (withReplayClose), as how:
// applied, or already applied for a duplicate. A context with none, every fact's but a replay's,
// closes nothing. A record no longer open or discarded is one another replay closed, having taken
// this one's claim once it went stale: the error rolls the fact back, and this replay answers
// failed.
func closeReplayed(ctx context.Context, tx pgx.Tx, how record.IntakeClosedHow) error {
	replay, ok := ctx.Value(replayCloseKey{}).(replayClose)
	if !ok {
		return nil
	}
	closed, err := record.NewStore().CloseIntakeGiveUp(ctx, tx, replay.project, replay.id, how, replay.by, replay.now())
	if err != nil {
		return err
	}
	if !closed {
		return closedByAnotherReplay(replay.id)
	}
	return nil
}

// closedByAnotherReplay is the error of a replay whose record another replay closed first.
func closedByAnotherReplay(id int64) error {
	return fmt.Errorf("give-up %d was closed by another replay while this one ran", id)
}
