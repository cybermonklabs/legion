package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// idleBoundReader is what readIdleBound asks: the request pool, a wrapped pool and a separate
// pool all answer a one-row query the same way.
type idleBoundReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readIdleBound is idle_in_transaction_session_timeout as the connection q hands out carries it.
// pg_settings reports the value in its own unit, which the unit column names, so the reading is a
// number rather than Postgres's formatted "1min10s".
func readIdleBound(t *testing.T, ctx context.Context, q idleBoundReader) time.Duration {
	t.Helper()
	var milliseconds int64
	var unit string
	if err := q.QueryRow(ctx, `
		select setting::bigint, unit from pg_settings where name = $1
	`, idleInTransactionParam).Scan(&milliseconds, &unit); err != nil {
		t.Fatalf("read %s: %v", idleInTransactionParam, err)
	}
	if unit != "ms" {
		t.Fatalf("%s is reported in %q, want milliseconds", idleInTransactionParam, unit)
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// The request pool's connections carry the bound this package computes - one cold read, the
// rollback that ends the transaction after it, and a margin (RequestIdleInTransactionTimeout) -
// and the pools opened from a copy of that configuration carry none, wrapped (Background) and
// separate (Rooms) alike: a background loop, a room load, a settlement or a lease may sit in a
// transaction for as long as its own work takes.
func TestOnlyTheRequestPoolsConnectionsCarryTheComputedIdleBound(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	if got := readIdleBound(t, ctx, database.Pool); got != RequestIdleInTransactionTimeout {
		t.Errorf("a request connection's %s = %v, want the computed %v (%v cold read + %v rollback + margin)",
			idleInTransactionParam, got, RequestIdleInTransactionTimeout, ColdReadBound, RollbackTimeout)
	}
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	if got := readIdleBound(t, ctx, background); got != 0 {
		t.Errorf("a background connection's %s = %v, want none", idleInTransactionParam, got)
	}
	rooms, err := database.Pool.Rooms()
	if err != nil {
		t.Fatalf("open the rooms pool: %v", err)
	}
	if got := readIdleBound(t, ctx, rooms); got != 0 {
		t.Errorf("a rooms connection's %s = %v, want none", idleInTransactionParam, got)
	}
}

// Postgres ends a request transaction left idle past that bound, so an orphaned one - a
// transaction whose pgx connection was abandoned, whose close reached Postgres late or never -
// cannot hold the rows it locked and the event horizon until Postgres notices the socket is gone.
// A background connection idle just as long keeps its transaction.
//
// The bound is shortened here (requestIdleInTransaction) rather than waited out, so the test
// takes seconds and not the seventy production runs.
func TestPostgresEndsARequestTransactionLeftIdlePastItsBound(t *testing.T) {
	const bound = 300 * time.Millisecond
	production := requestIdleInTransaction
	requestIdleInTransaction = bound
	t.Cleanup(func() { requestIdleInTransaction = production })

	database := openTestStore(t)
	ctx := context.Background()

	// Each transaction is marked on a context of its own, as two callers' transactions are: one
	// context holding both would refuse the second pool's connection (ErrNestedAcquire).
	request, err := database.Pool.Begin(WithTransactionTracking(ctx))
	if err != nil {
		t.Fatalf("begin the request transaction: %v", err)
	}
	defer request.Rollback(ctx)
	requestPID := backendPID(t, ctx, request)

	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	loop, err := background.Begin(WithTransactionTracking(ctx))
	if err != nil {
		t.Fatalf("begin the background transaction: %v", err)
	}
	defer loop.Rollback(ctx)
	loopPID := backendPID(t, ctx, loop)

	// Both sit idle in their transactions well past the request pool's bound.
	time.Sleep(10 * bound)

	_, err = request.Exec(ctx, "select 1")
	if err == nil {
		t.Errorf("a request transaction idle %v past its %v bound still runs statements, want it ended by Postgres",
			9*bound, bound)
	} else if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code != "25P03" {
		t.Errorf("a request transaction idle past its bound answered SQLSTATE %s (%s), want 25P03", pgErr.Code, pgErr.Message)
	}
	if alive(t, ctx, database.Pool, requestPID) {
		t.Errorf("the request connection (pid %d) is still in pg_stat_activity, want its session ended", requestPID)
	}

	if _, err := loop.Exec(ctx, "select 1"); err != nil {
		t.Errorf("a background transaction idle just as long: %v, want it still open", err)
	}
	if !alive(t, ctx, database.Pool, loopPID) {
		t.Errorf("the background connection (pid %d) is gone from pg_stat_activity, want it untouched", loopPID)
	}
}

// backendPID is the Postgres backend tx runs on.
func backendPID(t *testing.T, ctx context.Context, tx pgx.Tx) int32 {
	t.Helper()
	var pid int32
	if err := tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read the transaction's backend pid: %v", err)
	}
	return pid
}

// alive reports whether pid still holds a session, read on a connection of its own.
func alive(t *testing.T, ctx context.Context, q idleBoundReader, pid int32) bool {
	t.Helper()
	var open bool
	if err := q.QueryRow(ctx, "select exists(select 1 from pg_stat_activity where pid = $1)", pid).Scan(&open); err != nil {
		t.Fatalf("read pg_stat_activity for pid %d: %v", pid, err)
	}
	return open
}
