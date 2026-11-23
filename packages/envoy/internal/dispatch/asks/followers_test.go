package asks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// refusingDatabase fails every statement and every read with err, as a database that refused
// them does: a statement run alone, and each statement of a batch, which a follow's write sends
// (store.RunWrite). It is a transaction for Follow, and a transaction it never asks anything else
// of, so the rest of pgx.Tx is left nil.
type refusingDatabase struct {
	pgx.Tx
	err error
}

func (d refusingDatabase) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, d.err
}

func (d refusingDatabase) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, d.err
}

func (d refusingDatabase) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	return &refusedBatch{batch: batch, err: d.err}
}

// refusedBatch answers each statement of batch with err, and its Close reads each statement's
// result as pgx's own does, through the statement's callback, which is where a write batch names
// what failed (store.Queued).
type refusedBatch struct {
	batch *pgx.Batch
	err   error
}

func (r *refusedBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }

func (r *refusedBatch) Query() (pgx.Rows, error) { return nil, r.err }

func (r *refusedBatch) QueryRow() pgx.Row { return refusedRow{r.err} }

func (r *refusedBatch) Close() error {
	for _, queued := range r.batch.QueuedQueries {
		if queued.Fn == nil {
			continue
		}
		if err := queued.Fn(r); err != nil {
			return err
		}
	}
	return nil
}

// Tx is nil: the batch began no transaction of its own.
func (r *refusedBatch) Tx() pgx.Tx { return nil }

// refusedRow is a row whose statement was refused.
type refusedRow struct{ err error }

func (r refusedRow) Scan(...any) error { return r.err }

// A follow, a read of whether a session follows, and an unfollow that the database refuses each
// fail naming the ask and carrying the cause, and never name the session: a handler logs the
// failure, and has the session from its own request.
func TestAFollowerWriteOrReadTheDatabaseRefusesNamesTheAskAndNotTheSession(t *testing.T) {
	const ask, session = "ask-5c1e", "ses-unnamed-follower"
	refused := errors.New("the statement was refused")
	database := refusingDatabase{err: refused}
	ctx := context.Background()
	_, follow := Follow(ctx, database, ask, session, model.Actor{Kind: "session", ID: session}, time.Unix(0, 0).UTC(), graphstore.NoEvent)
	_, follows := Follows(ctx, database, ask, session)
	_, unfollow := Unfollow(ctx, database, ask, session)
	for what, err := range map[string]error{
		"a follow":                     follow,
		"a read of whether it follows": follows,
		"an unfollow":                  unfollow,
	} {
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), ask) {
			t.Errorf("%s refused = %v, want the refusal, naming ask %s", what, err, ask)
		} else if strings.Contains(err.Error(), session) {
			t.Errorf("%s refused names the session %s: %v", what, session, err)
		}
	}
}
