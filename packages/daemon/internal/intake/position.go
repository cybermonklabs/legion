package intake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// streamPosition is where a durable consumer's message sits on the notification stream: the
// project whose consumer delivered it, that consumer, its stream sequence and the time JetStream
// stored it. openFailure says the message may have an open failure (consumerRun.mayHaveFailed),
// which the position's record deletes in the same transaction, so a fact that applies never
// leaves one behind.
type streamPosition struct {
	project     string
	consumer    string
	seq         uint64
	time        time.Time
	openFailure bool
}

type streamPositionKey struct{}

// withStreamPosition is ctx carrying position, a delivery's stream position, for ApplyFact to
// record in the fact's transaction (recordStreamPosition). A delivery whose metadata did not parse
// has no position (nil) and carries none.
func withStreamPosition(ctx context.Context, position *streamPosition) context.Context {
	if position == nil {
		return ctx
	}
	return context.WithValue(ctx, streamPositionKey{}, *position)
}

// recordStreamPosition records the stream position ctx carries (withStreamPosition), in tx, so it
// commits with the fact or not at all. A context with no position (a fact from the API or
// supervision) records nothing.
func recordStreamPosition(ctx context.Context, tx pgx.Tx) error {
	position, ok := ctx.Value(streamPositionKey{}).(streamPosition)
	if !ok {
		return nil
	}
	return position.record(ctx, tx)
}

// record writes p as its consumer's position, in tx, and deletes the open failure of its message
// where it may have one. A position only advances: a redelivery of an older message, applied after
// a newer one, leaves the newer one standing. The time is truncated to the microseconds Postgres
// keeps, never rounded up, so a consumer created at it (openConsumer) starts at or before the
// message, never after it. A give-up records its message's position too, so a consumer made again
// later starts past it.
func (p streamPosition) record(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, $2, $3)
		on conflict (consumer) do update set
			stream_seq = greatest(consumer_positions.stream_seq, excluded.stream_seq),
			stream_time = greatest(consumer_positions.stream_time, excluded.stream_time)`,
		p.consumer, int64(p.seq), p.time.Truncate(time.Microsecond)); err != nil {
		return fmt.Errorf("record the position of durable consumer %s at stream sequence %d: %w", p.consumer, p.seq, err)
	}
	if !p.openFailure {
		return nil
	}
	_, err := record.NewStore().ClearIntakeFailure(ctx, tx, p.project, p.consumer, p.seq, p.time)
	return err
}

// storedStreamTime is the stream time of the last message consumer applied, and false when it
// has applied none.
func storedStreamTime(ctx context.Context, pool store.TxPool, consumer string) (time.Time, bool, error) {
	var at time.Time
	err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select stream_time from consumer_positions where consumer = $1`, consumer).Scan(&at)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read the stored position of durable consumer %s: %w", consumer, err)
	}
	return at, true, nil
}
