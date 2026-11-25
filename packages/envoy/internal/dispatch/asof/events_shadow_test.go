package asof_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	// A cursor at the cut runs in a read-only transaction of its own. It marks its caller as
	// holding a connection while it is open, as a cursor with no cut does, and its close ends the
	// transaction and gives the connection back.
	t.Run("a cursor at the cut holds its connection until it closes", func(t *testing.T) {
		tracked := store.WithTransactionTracking(atCut)
		rows, err := database.Pool.Query(tracked, countEvents)
		if err != nil {
			t.Fatalf("open a cursor at the cut: %v", err)
		}
		var one int
		if err := database.Pool.QueryRow(tracked, "select 1").Scan(&one); !errors.Is(err, store.ErrNestedAcquire) {
			t.Errorf("a pool read while the cursor is open: %v, want ErrNestedAcquire", err)
		}
		rows.Close()
		if err := database.Pool.QueryRow(tracked, "select 1").Scan(&one); err != nil {
			t.Errorf("a pool read after the cursor closed: %v", err)
		}
		if held := database.Pool.Stat().AcquiredConns(); held != 0 {
			t.Errorf("the pool holds %d connections once the cursor and the row have closed, want none", held)
		}
	})

	// A batch at the cut runs in a read-only transaction of its own on one connection. Its close
	// runs the callback of each statement not read yet and gives the connection back.
	t.Run("a batch at the cut runs its callbacks and gives its connection back when it closes", func(t *testing.T) {
		batch := &pgx.Batch{}
		var counted int
		batch.Queue(countEvents).QueryRow(func(row pgx.Row) error { return row.Scan(&counted) })
		results := database.Pool.SendBatch(atCut, batch)
		if held := database.Pool.Stat().AcquiredConns(); held != 1 {
			t.Errorf("the pool holds %d connections while the batch is open, want its one", held)
		}
		if err := results.Close(); err != nil {
			t.Fatalf("close the batch at the cut: %v", err)
		}
		if counted != 1 {
			t.Errorf("the batch's callback counts %d events at the cut, want 1", counted)
		}
		if held := database.Pool.Stat().AcquiredConns(); held != 0 {
			t.Errorf("the pool holds %d connections once the batch has closed, want none", held)
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
// statements out of the connection's cache. A batch at a cut leaves none either: pgx sends a
// batch in the connection's own mode, so the store sends each statement of a batch at a cut on
// its own, from a transaction opened at the cut and from the pool.
func TestABoundStatementLeavesNoPreparedStatement(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	atCut := asof.WithCut(ctx, mustCut(t, "2026-11-10T00:00:00Z"))
	// boundToTheCut is a part of the text of each statement below once Bind has bound it.
	const boundToTheCut = "public.events where created_at <="
	const countPrepared = "select count(*) from pg_prepared_statements where strpos(statement, $1) > 0"
	batchAtCut := func(t *testing.T, what string, send func(*pgx.Batch) pgx.BatchResults) {
		t.Helper()
		batch := &pgx.Batch{}
		batch.Queue("select count(*) from events where type <> $1", "asof.batch")
		batch.Queue("select count(*) from events where type = $1", "asof.batch")
		results := send(batch)
		for range batch.Len() {
			var events int
			if err := results.QueryRow().Scan(&events); err != nil {
				t.Fatalf("read %s at the cut: %v", what, err)
			}
		}
		if err := results.Close(); err != nil {
			t.Fatalf("close %s at the cut: %v", what, err)
		}
	}

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
	batchAtCut(t, "a transaction's batch", func(batch *pgx.Batch) pgx.BatchResults { return tx.SendBatch(ctx, batch) })
	if err := tx.QueryRow(ctx, countPrepared, boundToTheCut).Scan(&prepared); err != nil {
		t.Fatalf("read the connection's prepared statements: %v", err)
	}
	if prepared != 0 {
		t.Errorf("the connection holds %d prepared statements bound to the cut, want none", prepared)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back the transaction at the cut: %v", err)
	}

	if err := database.Pool.QueryRow(atCut, "select count(*) from events where type <> $1", "asof.pool").Scan(&events); err != nil {
		t.Fatalf("read a row of the pool at the cut: %v", err)
	}
	batchAtCut(t, "a batch of the pool", func(batch *pgx.Batch) pgx.BatchResults { return database.Pool.SendBatch(atCut, batch) })
	// The pool gives a connection back as the statement or the batch ends, so each connection
	// that ran them is idle now.
	idle := database.Pool.AcquireAllIdle(ctx)
	if len(idle) == 0 {
		t.Fatal("the pool holds no idle connection, so no connection's prepared statements are read")
	}
	prepared = 0
	for _, connection := range idle {
		var held int
		err := connection.QueryRow(ctx, countPrepared, boundToTheCut).Scan(&held)
		connection.Release()
		if err != nil {
			t.Fatalf("read a pooled connection's prepared statements: %v", err)
		}
		prepared += held
	}
	if prepared != 0 {
		t.Errorf("the pool's connections hold %d prepared statements bound to the cut, want none", prepared)
	}
}

// readOnlySQLTransaction is PostgreSQL's SQLSTATE for a write in a read-only transaction
// (read_only_sql_transaction).
const readOnlySQLTransaction = "25006"

// Bind refuses a statement that is not a read by its first word, so a read that calls a function
// that writes passes it. The database refuses it: every statement at a cut runs in a read-only
// transaction. That holds for a cursor, a row and a statement of the pool, a batch of the pool,
// and a statement and a batch of a transaction opened at the cut, even one whose options ask to
// write. The test makes the sequence with no cut, and no refused statement draws from it.
func TestAWriteThatBindPassesIsRefusedAtTheCutByTheDatabase(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, "create sequence asof_written"); err != nil {
		t.Fatalf("create the sequence: %v", err)
	}
	atCut := asof.WithCut(ctx, mustCut(t, "2026-11-10T00:00:00Z"))
	const write = "select nextval('asof_written')"
	refused := func(t *testing.T, what string, err error) {
		t.Helper()
		var postgres *pgconn.PgError
		if !errors.As(err, &postgres) || postgres.Code != readOnlySQLTransaction {
			t.Errorf("%s: %v, want PostgreSQL's refusal of a write in a read-only transaction (%s)", what, err, readOnlySQLTransaction)
		}
	}
	var value int64

	rows, err := database.Pool.Query(atCut, write)
	if err == nil {
		for rows.Next() {
		}
		rows.Close()
		err = rows.Err()
	}
	refused(t, "a cursor at the cut", err)
	refused(t, "a row at the cut", database.Pool.QueryRow(atCut, write).Scan(&value))
	_, err = database.Pool.Exec(atCut, write)
	refused(t, "a statement at the cut", err)

	batch := &pgx.Batch{}
	batch.Queue("select 1")
	batch.Queue(write)
	batch.Queue("select 2")
	results := database.Pool.SendBatch(atCut, batch)
	if err := results.QueryRow().Scan(&value); err != nil {
		t.Fatalf("read the batch's first statement at the cut: %v", err)
	}
	refused(t, "a batch's statement at the cut", results.QueryRow().Scan(&value))
	refused(t, "the batch's close at the cut", results.Close())

	tx, err := database.Pool.BeginTx(atCut, pgx.TxOptions{AccessMode: pgx.ReadWrite})
	if err != nil {
		t.Fatalf("begin at the cut: %v", err)
	}
	refused(t, "a statement of a transaction at the cut", tx.QueryRow(ctx, write).Scan(&value))
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back the transaction at the cut: %v", err)
	}
	tx, err = database.Pool.BeginTx(atCut, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin at the cut: %v", err)
	}
	defer tx.Rollback(ctx)
	refused(t, "a batch of a transaction at the cut", tx.SendBatch(ctx, batch).Close())

	var drawn bool
	if err := database.Pool.QueryRow(ctx, "select is_called from asof_written").Scan(&drawn); err != nil {
		t.Fatalf("read the sequence: %v", err)
	}
	if drawn {
		t.Error("a statement at the cut drew a value from the sequence")
	}
}

// refusedAtCut reports whether err is the store's refusal of method under a cut.
func refusedAtCut(err error, method string) bool {
	var refused *store.RefusedAtCutError
	return errors.As(err, &refused) && refused.Method == method
}
