package intake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// intakeRecords is every row of intake_failures, in id order, as the record keeps it: the open
// failures and the give-ups alike. It reads through pool, a connection of its own, so it sees
// only what committed.
func intakeRecords(t *testing.T, pool *pgxpool.Pool) []record.IntakeFailure {
	t.Helper()
	records, err := readIntakeRecords(context.Background(), pool)
	if err != nil {
		t.Fatalf("read intake_failures: %v", err)
	}
	return records
}

// readIntakeRecords is intakeRecords for a caller that cannot fail the test, such as a handler or
// a termination running on a goroutine of the consumer's. It reads through the record's own read
// (record.Postgres.IntakeRecords), so every column is read as the statements read it.
func readIntakeRecords(ctx context.Context, pool *pgxpool.Pool) ([]record.IntakeFailure, error) {
	var records []record.IntakeFailure
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		records, err = record.NewStore().IntakeRecords(ctx, tx)
		return err
	})
	return records, err
}

// intakeRecordOf is the row of consumer's message at stream sequence seq, and false for none.
func intakeRecordOf(t *testing.T, pool *pgxpool.Pool, consumer string, seq uint64) (record.IntakeFailure, bool) {
	t.Helper()
	for _, f := range intakeRecords(t, pool) {
		if f.Consumer == consumer && f.StreamSeq == seq {
			return f, true
		}
	}
	return record.IntakeFailure{}, false
}

// waitGivenUp waits for the give-up record of consumer's message at stream sequence seq and
// answers it.
func waitGivenUp(t *testing.T, pool *pgxpool.Pool, consumer string, seq uint64) record.IntakeFailure {
	t.Helper()
	var given record.IntakeFailure
	testwait.Eventually(t, fmt.Sprintf("the give-up record of %s's message %d", consumer, seq), func() bool {
		found, ok := intakeRecordOf(t, pool, consumer, seq)
		given = found
		return ok && found.GivenUp()
	})
	return given
}

// committedGiveUp says whether a give-up record of consumer's message at stream sequence seq has
// committed, read through pool, a connection of its own.
func committedGiveUp(ctx context.Context, pool *pgxpool.Pool, consumer string, seq uint64) (bool, error) {
	var given bool
	err := pool.QueryRow(ctx, `select exists (select 1 from intake_failures
		where consumer = $1 and stream_seq = $2 and given_up_at is not null)`, consumer, int64(seq)).Scan(&given)
	return given, err
}

// failingRecordPool is a Pool that stands for a PostgreSQL refusing the intake's record: while
// refuse is on, every statement on intake_failures fails before it reaches the server, and while
// refuseCommit is on, the commit of a transaction that ran one fails and the transaction is rolled
// back; refuseNextCommit refuses one such commit and is then off again. Every other statement and
// commit passes through. refused and commitsRefused count what it refused.
type failingRecordPool struct {
	Pool
	refuse, refuseCommit, refuseNextCommit atomic.Bool
	refused, commitsRefused                atomic.Int32
}

var errRecordRefused = errors.New("the test refuses the intake's record")

func (p *failingRecordPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.Pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &failingRecordTx{Tx: tx, pool: p}, nil
}

// refuses says whether sql is a statement p refuses now, and counts it when it is.
func (p *failingRecordPool) refuses(sql string) bool {
	if !strings.Contains(sql, "intake_failures") || !p.refuse.Load() {
		return false
	}
	p.refused.Add(1)
	return true
}

// failingRecordTx is a transaction of failingRecordPool's.
type failingRecordTx struct {
	pgx.Tx
	pool *failingRecordPool
	// recorded is whether a statement on intake_failures ran in it.
	recorded bool
}

func (tx *failingRecordTx) note(sql string) bool {
	if strings.Contains(sql, "intake_failures") {
		tx.recorded = true
	}
	return tx.pool.refuses(sql)
}

func (tx *failingRecordTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.note(sql) {
		return pgconn.CommandTag{}, errRecordRefused
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *failingRecordTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx.note(sql) {
		return nil, errRecordRefused
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *failingRecordTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.note(sql) {
		return refusedRow{}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *failingRecordTx) Commit(ctx context.Context) error {
	if tx.recorded && (tx.pool.refuseCommit.Load() || tx.pool.refuseNextCommit.CompareAndSwap(true, false)) {
		tx.pool.commitsRefused.Add(1)
		_ = tx.Tx.Rollback(ctx)
		return errRecordRefused
	}
	return tx.Tx.Commit(ctx)
}

// refusedRow is the row of a statement failingRecordPool refused.
type refusedRow struct{}

func (refusedRow) Scan(...any) error { return errRecordRefused }
