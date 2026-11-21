package asof_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A route answers as of a cut by running its present statements under a context that carries
// the cut: the store binds each one, so a statement that names events reads the events shadow.
// Every way the pool sends a statement binds it - a single row, a cursor, a batch, a
// transaction opened under the cut and its savepoints - and every way it hands out SQL that
// nothing binds is refused. One event stands at the cut and one a microsecond after it, so
// the read holds the cut's own microsecond and nothing later.
func TestABoundStatementReadsTheEventsShadow(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into events (seq, type, actor, payload, notify, created_at) values
			(0, 'asof.at_the_cut', '{"kind":"user","id":"alice"}', '{}', false, '2026-11-10T00:00:00Z'),
			(0, 'asof.after_the_cut', '{"kind":"user","id":"alice"}', '{}', false, '2026-11-10T00:00:00.000001Z')
	`); err != nil {
		t.Fatalf("seed the events: %v", err)
	}
	// The nanoseconds past the cut's microsecond round down: the cut is the first event's time.
	cut := mustCut(t, "2026-11-10T00:00:00.000000999Z")
	atCut := asof.WithCut(ctx, cut)
	const countEvents = "select count(*) from events"

	count := func(t *testing.T, what string, row pgx.Row, want int) {
		t.Helper()
		var got int
		if err := row.Scan(&got); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got != want {
			t.Errorf("%s counts %d events, want %d", what, got, want)
		}
	}

	count(t, "a read with no cut", database.Pool.QueryRow(ctx, countEvents), 2)
	count(t, "a row read at the cut", database.Pool.QueryRow(atCut, countEvents), 1)
	// Each form that Bind merges the shadows into is SQL that PostgreSQL runs, and its own
	// members read the shadow.
	for _, statement := range []string{
		"-- a comment stays first\nwith seen as (select id from events) select count(*) from seen",
		"with recursive seen(id) as (select id from events) select count(*) from seen",
		"(select count(*) from events) union (select count(*) from events)",
	} {
		count(t, statement, database.Pool.QueryRow(atCut, statement), 1)
	}

	rows, err := database.Pool.Query(atCut, countEvents)
	if err != nil {
		t.Fatalf("open a cursor at the cut: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("a cursor at the cut answers no row: %v", rows.Err())
	}
	count(t, "a cursor at the cut", rows, 1)
	rows.Close()

	batch := &pgx.Batch{}
	batch.Queue(countEvents)
	batch.Queue("select count(*) from events where type = 'asof.after_the_cut'")
	results := database.Pool.SendBatch(atCut, batch)
	count(t, "a batch's first statement at the cut", results.QueryRow(), 1)
	count(t, "a batch's second statement at the cut", results.QueryRow(), 0)
	if err := results.Close(); err != nil {
		t.Fatalf("close the batch at the cut: %v", err)
	}

	t.Run("a transaction opened at the cut binds its statements and its savepoints'", func(t *testing.T) {
		// The transaction keeps the cut it was opened with, whatever context a statement names.
		tracked := store.WithTransactionTracking(atCut)
		tx, err := database.Pool.BeginTx(tracked, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("begin at the cut: %v", err)
		}
		defer tx.Rollback(ctx)
		count(t, "a transaction's statement", tx.QueryRow(ctx, countEvents), 1)

		savepoint, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("open a savepoint at the cut: %v", err)
		}
		count(t, "a savepoint's statement", savepoint.QueryRow(ctx, countEvents), 1)
		if err := savepoint.Rollback(ctx); err != nil {
			t.Fatalf("roll back to the savepoint: %v", err)
		}

		// The transaction still marks its caller as holding a connection, as one opened with
		// no cut does.
		var one int
		if err := database.Pool.QueryRow(tracked, "select 1").Scan(&one); !errors.Is(err, store.ErrNestedAcquire) {
			t.Errorf("a pool read inside the transaction: %v, want ErrNestedAcquire", err)
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"events"}, []string{"type"}, pgx.CopyFromRows(nil)); !refusedAtCut(err, "Tx.CopyFrom") {
			t.Errorf("a copy inside the transaction: %v, want it refused at the cut", err)
		}
		if _, err := tx.Prepare(ctx, "events_at_cut", countEvents); !refusedAtCut(err, "Tx.Prepare") {
			t.Errorf("a prepare inside the transaction: %v, want it refused at the cut", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := database.Pool.QueryRow(tracked, "select 1").Scan(&one); err != nil {
			t.Errorf("a pool read after the transaction committed: %v", err)
		}
	})

	t.Run("nothing that sends unbound SQL is handed out at the cut", func(t *testing.T) {
		if _, err := database.Pool.CopyFrom(atCut, pgx.Identifier{"events"}, []string{"type"}, pgx.CopyFromRows(nil)); !refusedAtCut(err, "CopyFrom") {
			t.Errorf("a copy at the cut: %v, want it refused", err)
		}
		if connection, err := database.Pool.Acquire(atCut); !refusedAtCut(err, "Acquire") {
			if connection != nil {
				connection.Release()
			}
			t.Errorf("a connection acquired at the cut: %v, want it refused", err)
		}
		if err := database.Pool.AcquireFunc(atCut, func(*pgxpool.Conn) error { return nil }); !refusedAtCut(err, "AcquireFunc") {
			t.Errorf("a connection acquired for a function at the cut: %v, want it refused", err)
		}
		if connection, err := database.Pool.Listener(atCut); !refusedAtCut(err, "Listener") {
			if connection != nil {
				_ = connection.Close(ctx)
			}
			t.Errorf("a listening connection at the cut: %v, want it refused", err)
		}
		if connection, err := database.Pool.Dedicated(atCut, "asof-test"); !refusedAtCut(err, "Dedicated") {
			if connection != nil {
				_ = connection.Close(ctx)
			}
			t.Errorf("a dedicated connection at the cut: %v, want it refused", err)
		}
	})

	t.Run("a write at the cut is refused before it is sent", func(t *testing.T) {
		_, err := database.Pool.Exec(atCut, "delete from events")
		var refused *asof.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("a delete at the cut: %v, want an *asof.RefusedError", err)
		}
		count(t, "a read with no cut after the refused delete", database.Pool.QueryRow(ctx, countEvents), 2)
	})
}

// A statement bound to a cut holds the cut in its text, so each new cut is a new text. The store
// sends such a statement with its description cached on the client alone
// (pgx.QueryExecModeCacheDescribe): a read at a cut leaves no prepared statement on its
// connection, where pgx's default mode would keep one for each text and push the present
// statements out of the connection's cache.
func TestABoundStatementLeavesNoPreparedStatement(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	atCut := asof.WithCut(ctx, mustCut(t, "2026-11-10T00:00:00Z"))
	tx, err := database.Pool.BeginTx(atCut, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin at the cut: %v", err)
	}
	defer tx.Rollback(ctx)
	var events, prepared int
	if err := tx.QueryRow(ctx, "select count(*) from events where type <> $1", "asof.none").Scan(&events); err != nil {
		t.Fatalf("read the events at the cut: %v", err)
	}
	if _, err := tx.Exec(ctx, "select count(*) from events where type = $1", "asof.none"); err != nil {
		t.Fatalf("run a statement at the cut: %v", err)
	}
	if err := tx.QueryRow(ctx, "select count(*) from pg_prepared_statements where strpos(statement, $1) > 0", "public.events where created_at <=").Scan(&prepared); err != nil {
		t.Fatalf("read the connection's prepared statements: %v", err)
	}
	if prepared != 0 {
		t.Errorf("the connection holds %d prepared statements bound to the cut, want none", prepared)
	}
}

// refusedAtCut reports whether err is the store's refusal of method under a cut.
func refusedAtCut(err error, method string) bool {
	var refused *store.RefusedAtCutError
	return errors.As(err, &refused) && refused.Method == method
}
