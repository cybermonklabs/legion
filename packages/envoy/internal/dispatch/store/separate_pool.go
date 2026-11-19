package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/pgsetting"
)

// SeparatePool is a pool Pool opens beside the shared one, from the shared pool's configuration,
// and closes with it: the document rooms' (Rooms), their appends' (RoomAppends), the compaction's
// (Compaction), the event horizon's (Horizon) and the loop leases' (LeaseCallers.For), and the
// health probe's and the listener's inside this package. It has *pgxpool.Pool's methods but Close
// and Reset, which are Pool.Close's alone, since every caller of a separate pool shares it.
//
// Each method that takes a connection names a connect that failed under it without DATABASE_URL's
// user and without the words of the server's answer (pgsetting.ConnectFailure), as Pool's own
// methods do: pgxpool dials as its callers need connections, long after Open's ping, and pgx's
// connect error names the user, where a token can stand, and so does the server's answer to a
// refused sign-in. Unlike Pool it keeps no nested-acquire guard (ErrNestedAcquire): a pool of its
// own cannot close the cycle the guard prevents.
type SeparatePool struct{ pool *pgxpool.Pool }

// Acquire takes a connection the caller holds until it releases it.
func (s *SeparatePool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, pgsetting.ConnectFailure(err)
	}
	return conn, nil
}

// AcquireFunc runs fn with a connection, released when fn returns, as pgxpool's AcquireFunc does;
// fn's own error is handed on as fn returned it.
func (s *SeparatePool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	conn, err := s.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return fn(conn)
}

// AcquireAllIdle takes every idle connection the pool holds. It dials none, so no connect of it
// can fail.
func (s *SeparatePool) AcquireAllIdle(ctx context.Context) []*pgxpool.Conn {
	return s.pool.AcquireAllIdle(ctx)
}

// Begin opens a transaction on a connection of the pool.
func (s *SeparatePool) Begin(ctx context.Context) (pgx.Tx, error) {
	return s.BeginTx(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options on a connection of the pool.
func (s *SeparatePool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, pgsetting.ConnectFailure(err)
	}
	return tx, nil
}

// Query runs a query on a connection of the pool, which the rows hold until they close. A failed
// query answers rows whose Err is the failure as pgxpool's do, and the failure named here.
func (s *SeparatePool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	return rows, pgsetting.ConnectFailure(err)
}

// QueryRow runs a single-row query on a connection of the pool. pgxpool takes the connection first
// and answers a failed one at the Scan, which connectFailureRow names.
func (s *SeparatePool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return connectFailureRow{s.pool.QueryRow(ctx, sql, args...)}
}

// Exec runs a statement on a connection of the pool.
func (s *SeparatePool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := s.pool.Exec(ctx, sql, args...)
	return tag, pgsetting.ConnectFailure(err)
}

// SendBatch runs a batch on a connection of the pool. pgxpool takes the connection first and
// answers a failed one from every result, which connectFailureBatch names.
func (s *SeparatePool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	return connectFailureBatch{s.pool.SendBatch(ctx, batch)}
}

// CopyFrom streams rows into a table on a connection of the pool.
func (s *SeparatePool) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	copied, err := s.pool.CopyFrom(ctx, table, columns, source)
	return copied, pgsetting.ConnectFailure(err)
}

// Ping takes a connection of the pool and checks that the server answers on it.
func (s *SeparatePool) Ping(ctx context.Context) error {
	return pgsetting.ConnectFailure(s.pool.Ping(ctx))
}

// Stat reports the pool's current connection counts.
func (s *SeparatePool) Stat() *pgxpool.Stat { return s.pool.Stat() }

// Config returns a copy of the pool's configuration.
func (s *SeparatePool) Config() *pgxpool.Config { return s.pool.Config() }
