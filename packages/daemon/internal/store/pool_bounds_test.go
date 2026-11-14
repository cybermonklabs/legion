package store

// The bound every connection of the daemon's pool carries (Open's idleBound), measured against
// the PostgreSQL LEGION_TEST_PG_DSN names: that a pooled connection has it, that a DSN naming a
// value of its own keeps that one, and the two things the bound does and does not do — it ends a
// transaction that sits with no statement running, and it does not touch a statement that runs
// long. This is the failure INFRA-3169 was: every connection of the production pool sat `idle in
// transaction`, and nothing ended any of them.

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// storeWith is emptyStore with query written into the DSN: a database of this test's own, opened
// through Open itself, with the pool parameters or connection settings the test needs. Open takes
// nothing but a DSN, so the DSN is where a test says what it wants.
func storeWith(t *testing.T, query map[string]string) *Store {
	t.Helper()
	dsn, err := url.Parse(testdb.Database(t, "legion_store_bounds_test"))
	if err != nil {
		t.Fatalf("parse the test database's DSN: %v", err)
	}
	values := dsn.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	dsn.RawQuery = values.Encode()
	store, err := Open(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// requireSetting fails unless the server reports setting as want on a connection of the store's
// pool. The server makes the comparison, as intervals, because the two sides spell the same
// duration differently: PostgreSQL prints a time setting in the largest unit exact for it (30000
// ms reads `30s`, 60000 ms reads `1min`), and Go's own spelling is its own. Comparing what the
// two mean rather than what they read keeps this honest for any value the bound is ever given.
func requireSetting(t *testing.T, store *Store, setting string, want time.Duration) {
	t.Helper()
	var reported string
	var same bool
	err := store.Pool().QueryRow(context.Background(),
		"select current_setting($1::text), current_setting($1::text)::interval = $2::text::interval",
		setting, want.String(),
	).Scan(&reported, &same)
	if err != nil {
		t.Fatalf("read %s on a pooled connection: %v", setting, err)
	}
	if !same {
		t.Fatalf("the pool's connection reports %s = %s, want %s", setting, reported, want)
	}
}

// Every connection the daemon hands out carries the idle bound, because Open sets it on the pool
// and no call site can forget it.
func TestThePooledConnectionCarriesTheIdleBound(t *testing.T) {
	requireSetting(t, emptyStore(t), idleBoundParam, idleBound)
}

// A deployment that wants another bound writes it into its DSN and keeps it, so raising it takes
// no release of the daemon.
func TestADsnThatNamesTheIdleBoundKeepsItsValue(t *testing.T) {
	const named = 5 * time.Second
	store := storeWith(t, map[string]string{idleBoundParam: "5s"})
	requireSetting(t, store, idleBoundParam, named)
}

// A transaction that sits past the bound with no statement running loses its connection, and the
// pool serves the next caller all the same: one stalled transaction costs one connection, which
// is the whole of what R3 asks.
//
// The DSN names a bound of its own, a second, so this test waits a second rather than the
// daemon's half minute; the daemon's own 30 seconds is what
// TestThePooledConnectionCarriesTheIdleBound pins. One connection in the pool, so the connection
// the bound ends is the only connection there is and the read at the end cannot be served by
// another.
func TestTheIdleBoundEndsAStalledTransactionAndFreesTheConnection(t *testing.T) {
	store := storeWith(t, map[string]string{idleBoundParam: "1s", "pool_max_conns": "1"})
	ctx := context.Background()
	tx, err := store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the transaction that stalls: %v", err)
	}
	if _, err := tx.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the first statement of the stalling transaction: %v", err)
	}

	// Three seconds of Go work and no statement — the gap the bound was measured with on this
	// pod's PostgreSQL 18.6 on 2026-11-12 — which is past a one-second bound with room to spare.
	time.Sleep(3 * time.Second)

	// 25P03 is idle_in_transaction_session_timeout: the server's own word for what it did, and
	// not merely a connection that went away, which is what tells this test from a flake.
	_, err = tx.Exec(ctx, "select 1")
	var ended *pgconn.PgError
	if !errors.As(err, &ended) || ended.Code != "25P03" {
		t.Fatalf("the statement after the stall = %v, want the server to have ended the connection (SQLSTATE 25P03)", err)
	}

	// The stalled transaction gives up its connection, and the pool answers the next caller:
	// with one connection configured, the pool had to make good the loss to answer at all.
	_ = tx.Rollback(ctx)
	var served int
	if err := store.Pool().QueryRow(ctx, "select 1").Scan(&served); err != nil {
		t.Fatalf("the read after the stalled transaction lost its connection: %v", err)
	}
	if served != 1 {
		t.Fatalf("the read after the stalled transaction answered %d, want 1", served)
	}
}

// The idle bound ends a transaction that sits between statements and nothing else: a statement
// that runs longer than the bound runs to its end. That is why it is safe on the pool, and why
// bounding a statement is a statement_timeout's job and set where one statement's length is known
// (the state route's own, internal/api).
func TestALongStatementSurvivesTheIdleBound(t *testing.T) {
	store := storeWith(t, map[string]string{idleBoundParam: "1s"})
	ctx := context.Background()
	tx, err := store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the transaction with the long statement: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Three seconds in one statement against a one-second idle bound.
	if _, err := tx.Exec(ctx, "select pg_sleep(3)"); err != nil {
		t.Fatalf("the statement longer than the idle bound = %v, want it to run to its end", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the transaction whose statement outran the idle bound: %v", err)
	}
}
