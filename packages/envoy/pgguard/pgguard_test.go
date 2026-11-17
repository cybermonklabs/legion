package pgguard_test

// The guard both stores wrap their pools in, measured against a database of the test's own
// (testpg.URL: the server DISPATCH_TEST_DATABASE_URL names, or a container). Each store pins its
// own pool's use of it with a consumer test (TestTheStoresAcquireIsRefusedThroughTheSharedGuard,
// in packages/daemon/internal/store and in internal/dispatch/store).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testpg"
	"github.com/sjawhar/envoy/pgguard"
)

// refusalBound is how long a connection is allowed to be asked for before the test calls the
// answer a wait. The guard answers without touching the database, so the honest answer takes
// microseconds; a request it does not refuse waits, in a pool of one connection, for the
// connection the caller holds, and this bound turns that wait into a failure that names its line.
const refusalBound = 10 * time.Second

// program is the name the test's pools log their refusals under.
const program = "pgguard-test"

type stepKey struct{}

// A caller that holds one connection of a guarded pool is refused a second, at once and by
// ErrNestedAcquire, by every way the pool hands one out, whichever holder holds it: a transaction,
// an open cursor, or a connection the caller took and marked itself (Hold). The hold ends where
// the connection goes back, once, and a context nothing marked is refused nothing. Each refusal is
// logged once per calling stack, under the pool's program, and a transaction takes the step
// Options.BeforeCommit names inside it, just before its commit.
//
// Control: make Guard return nil whatever its caller holds, and every subtest but the unmarked
// context's and the commit step's fails at its first refusal, which the pool then serves or waits
// out refusalBound for.
func TestAGuardedPoolRefusesASecondConnectionToACallerThatHoldsOne(t *testing.T) {
	url := testpg.URL(t)
	// Every refusal logs a stack; only the subtest that counts them reads the log.
	logtest.SetDefault(t, slog.NewTextHandler(io.Discard, nil))

	t.Run("a transaction holds its connection until it ends", func(t *testing.T) {
		pool := guardedPool(t, url, 1, nil)
		ctx := pgguard.WithTransactionTracking(context.Background())
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			t.Fatalf("a statement through the transaction: %v", err)
		}
		refusedEveryWay(t, pool, ctx, "the caller inside its transaction")
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		served(t, pool, ctx, "the caller whose transaction committed")

		// A transaction that commits and then runs its deferred rollback releases once: the
		// second end must not clear the mark of the cursor that holds a connection by then.
		committed, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction to commit twice: %v", err)
		}
		if err := committed.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rows, err := pool.Query(ctx, "select generate_series(1, 2)")
		if err != nil {
			t.Fatalf("open the cursor after the commit: %v", err)
		}
		defer rows.Close()
		_ = committed.Rollback(ctx)
		refused(t, "a read under the cursor once the committed transaction rolled back too",
			pool.QueryRow(bounded(t, ctx), "select 1").Scan(new(int)))
		rows.Close()
		served(t, pool, ctx, "the caller whose cursor closed")

		// A transaction that rolls back gives its connection back too.
		rolled, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatalf("begin the transaction to roll back: %v", err)
		}
		refused(t, "a read inside the read-only transaction", pool.QueryRow(bounded(t, ctx), "select 1").Scan(new(int)))
		if err := rolled.Rollback(ctx); err != nil {
			t.Fatalf("roll back: %v", err)
		}
		served(t, pool, ctx, "the caller whose transaction rolled back")
	})

	t.Run("a cursor holds its connection until it gives it back", func(t *testing.T) {
		pool := guardedPool(t, url, 1, nil)
		ctx := pgguard.WithTransactionTracking(context.Background())

		rows, err := pool.Query(ctx, "select generate_series(1, 2)")
		if err != nil {
			t.Fatalf("open the cursor: %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("the cursor's first row: %v", rows.Err())
		}
		refusedEveryWay(t, pool, ctx, "the caller inside its open cursor")
		// Drained: the Next that finds no more rows gives the connection back, with no Close.
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("drain the cursor: %v", err)
		}
		served(t, pool, ctx, "the caller whose cursor was drained")

		closing, err := pool.Query(ctx, "select generate_series(1, 2)")
		if err != nil {
			t.Fatalf("open the cursor to close: %v", err)
		}
		defer closing.Close()
		closing.Close()
		served(t, pool, ctx, "the caller whose cursor was closed")

		// pgx treats a scan failure as fatal: it closes the rows and hands the connection back,
		// so the mark goes back with it. A null array element is a value []string cannot take.
		failing, err := pool.Query(ctx, "select array['a', null]::text[]")
		if err != nil {
			t.Fatalf("open the cursor whose scan fails: %v", err)
		}
		defer failing.Close()
		if !failing.Next() {
			t.Fatalf("the failing cursor's first row: %v", failing.Err())
		}
		var elements []string
		if err := failing.Scan(&elements); err == nil {
			t.Fatal("scanned a null array element into []string, want a scan failure")
		}
		served(t, pool, ctx, "the caller whose cursor's scan failed")
	})

	t.Run("a connection the caller keeps is held while its mark is", func(t *testing.T) {
		pool := guardedPool(t, url, 1, nil)
		ctx := pgguard.WithTransactionTracking(context.Background())
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		release := pgguard.Hold(ctx)
		if !pgguard.Holds(ctx) {
			t.Fatal("Holds after Hold = false, want true")
		}
		refusedEveryWay(t, pool, ctx, "the caller keeping an acquired connection")
		release()
		release()
		conn.Release()
		if pgguard.Holds(ctx) {
			t.Fatal("Holds after the release = true, want false")
		}
		served(t, pool, ctx, "the caller whose acquired connection is back")
		if release := pgguard.Hold(context.Background()); release != nil {
			t.Fatal("Hold on a context nothing marked returned a release, want nil: it has no mark to clear")
		}
	})

	t.Run("a context nothing marked is refused nothing", func(t *testing.T) {
		// Two connections: the guard cannot see this caller's first, so its second is served
		// from the pool rather than refused.
		pool := guardedPool(t, url, 2, nil)
		ctx := context.Background()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		served(t, pool, ctx, "an unmarked caller inside its transaction")
	})

	t.Run("a concurrent worker keeps a mark of its own", func(t *testing.T) {
		pool := guardedPool(t, url, 2, nil)
		starter := pgguard.WithTransactionTracking(context.Background())
		if again := pgguard.WithTransactionTracking(starter); again != starter {
			t.Fatal("WithTransactionTracking on a marked context made a new mark, want the one it carries")
		}
		worker := pgguard.ForConcurrentWorker(starter)
		tx, err := pool.Begin(starter)
		if err != nil {
			t.Fatalf("the starter's transaction: %v", err)
		}
		defer tx.Rollback(context.Background())
		served(t, pool, worker, "the worker beside its starter's transaction")
		workerTx, err := pool.Begin(worker)
		if err != nil {
			t.Fatalf("the worker's own transaction: %v", err)
		}
		defer workerTx.Rollback(context.Background())
		refused(t, "a second connection under the worker's own transaction",
			pool.QueryRow(bounded(t, worker), "select 1").Scan(new(int)))
	})

	t.Run("a refusal is logged once per calling stack under its program", func(t *testing.T) {
		pool := guardedPool(t, url, 1, nil)
		pgguard.ForgetLoggedRefusalsForTest()
		var logs logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))
		ctx := pgguard.WithTransactionTracking(context.Background())
		release := pgguard.Hold(ctx)
		defer release()

		for range 3 {
			refused(t, "a transaction from the first site", beginFromOneSite(ctx, pool))
		}
		refused(t, "a transaction from the second site", beginFromAnotherSite(ctx, pool))
		lines := strings.Split(strings.TrimSpace(logs.Lines(program+": second pooled connection requested while one is held")), "\n")
		if len(lines) != 2 {
			t.Fatalf("the log holds %d refusal lines, want one per calling stack, 2:\n%s", len(lines), logs.String())
		}
		for i, site := range []string{"beginFromOneSite", "beginFromAnotherSite"} {
			if !strings.Contains(lines[i], site) {
				t.Errorf("refusal line %d names no %s:\n%s", i+1, site, lines[i])
			}
		}
	})

	t.Run("a transaction takes its commit step inside it", func(t *testing.T) {
		step := func(ctx context.Context) func(context.Context, pgx.Tx) error {
			step, _ := ctx.Value(stepKey{}).(func(context.Context, pgx.Tx) error)
			return step
		}
		pool := guardedPool(t, url, 1, step)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, "create table steps (what text not null)"); err != nil {
			t.Fatalf("create the steps table: %v", err)
		}
		insert := func(what string) func(context.Context, pgx.Tx) error {
			return func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, "insert into steps (what) values ($1)", what)
				return err
			}
		}
		// On a context nothing marked: the step alone makes the transaction one the pool wraps.
		stepping := context.WithValue(ctx, stepKey{}, insert("committed"))
		if err := pgx.BeginFunc(stepping, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(stepping, "insert into steps (what) values ('the write')")
			return err
		}); err != nil {
			t.Fatalf("the transaction with a step: %v", err)
		}

		refusal := errors.New("the step refuses")
		failing := context.WithValue(ctx, stepKey{}, func(context.Context, pgx.Tx) error { return refusal })
		err := pgx.BeginFunc(failing, pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(failing, "insert into steps (what) values ('the write the step refuses')")
			return err
		})
		if !errors.Is(err, refusal) {
			t.Fatalf("the transaction whose step fails = %v, want the step's error", err)
		}

		// A transaction already failed skips the step: pgx answers its commit with its own
		// rollback error.
		skipped := context.WithValue(pgguard.WithTransactionTracking(ctx), stepKey{}, insert("a failed transaction's step"))
		tx, err := pool.Begin(skipped)
		if err != nil {
			t.Fatalf("begin the transaction that fails: %v", err)
		}
		if _, err := tx.Exec(skipped, "select 1/0"); err == nil {
			t.Fatal("divided by zero, want the statement to fail")
		}
		if err := tx.Commit(skipped); !errors.Is(err, pgx.ErrTxCommitRollback) {
			t.Fatalf("commit of the failed transaction = %v, want pgx's %v", err, pgx.ErrTxCommitRollback)
		}
		served(t, pool, skipped, "the caller whose failed transaction ended")

		rows, err := pool.Query(ctx, "select what from steps order by what")
		if err != nil {
			t.Fatalf("read the steps: %v", err)
		}
		written, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("collect the steps: %v", err)
		}
		if want := []string{"committed", "the write"}; strings.Join(written, "|") != strings.Join(want, "|") {
			t.Fatalf("the steps table holds %q, want %q: the step commits with its transaction's write, and a refused or skipped step leaves nothing", written, want)
		}
	})
}

// guardedPool opens a guarded pool of size connections on the database url names, closed when the
// test ends, whose transactions take the commit step beforeCommit reads.
func guardedPool(t *testing.T, url string, size int32, beforeCommit func(context.Context) func(context.Context, pgx.Tx) error) *pgguard.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse the database URL: %v", err)
	}
	config.MaxConns = size
	raw, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the pool: %v", err)
	}
	t.Cleanup(raw.Close)
	return pgguard.New(raw, pgguard.Options{Program: program, BeforeCommit: beforeCommit})
}

// bounded is caller with refusalBound's deadline.
func bounded(t *testing.T, caller context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(caller, refusalBound)
	t.Cleanup(cancel)
	return ctx
}

// refused fails the test unless err is the guard's refusal.
func refused(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, pgguard.ErrNestedAcquire) {
		t.Fatalf("%s = %v, want %v", what, err, pgguard.ErrNestedAcquire)
	}
}

// refusedEveryWay fails the test unless every way pool hands out a connection refuses caller.
func refusedEveryWay(t *testing.T, pool *pgguard.Pool, caller context.Context, who string) {
	t.Helper()
	ctx := bounded(t, caller)
	rows, err := pool.Query(ctx, "select 1")
	if rows != nil {
		rows.Close()
	}
	refused(t, who+"'s query", err)
	refused(t, who+"'s row read", pool.QueryRow(ctx, "select 1").Scan(new(int)))
	_, err = pool.Exec(ctx, "select 1")
	refused(t, who+"'s statement", err)
	tx, err := pool.Begin(ctx)
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	refused(t, who+"'s second transaction", err)
	tx, err = pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	refused(t, who+"'s second transaction with options", err)
	conn, err := pool.Acquire(ctx)
	if conn != nil {
		conn.Release()
	}
	refused(t, who+"'s acquired connection", err)
	refused(t, who+"'s connection for a function", pool.AcquireFunc(ctx, func(*pgxpool.Conn) error { return nil }))
	if idle := pool.AcquireAllIdle(ctx); idle != nil {
		for _, conn := range idle {
			conn.Release()
		}
		t.Fatalf("%s took %d idle connections, want none", who, len(idle))
	}
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"steps"}, []string{"what"}, pgx.CopyFromRows(nil))
	refused(t, who+"'s copy", err)
	batch := &pgx.Batch{}
	batch.Queue("select 1")
	refused(t, who+"'s batch", pool.SendBatch(ctx, batch).Close())
	refused(t, who+"'s ping", pool.Ping(ctx))
}

// served fails the test unless pool serves caller a read within refusalBound.
func served(t *testing.T, pool *pgguard.Pool, caller context.Context, who string) {
	t.Helper()
	var one int
	if err := pool.QueryRow(bounded(t, caller), "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("the read asked for by %s answered %d, %v; want it served", who, one, err)
	}
}

// beginFromOneSite and beginFromAnotherSite are two call sites of one pool method, whose refusals
// differ only in the frames beyond the guard's.
func beginFromOneSite(ctx context.Context, pool *pgguard.Pool) error {
	tx, err := pool.Begin(ctx)
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	return err
}

func beginFromAnotherSite(ctx context.Context, pool *pgguard.Pool) error {
	tx, err := pool.Begin(ctx)
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	return err
}
