package store

// The rule the pool cannot survive without, measured against the PostgreSQL LEGION_TEST_PG_DSN
// names: a caller that already holds one of the pool's connections is refused a second one, at
// the call that asks for it, rather than waiting for a connection only it can give back. That
// wait is INFRA-3169 — every connection of the production pool sat `idle in transaction` while
// the callers behind them waited — and a pool of one connection is that deployment in miniature.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// refusalBound is how long a connection is allowed to be asked for before the test calls the
// answer a wait. The guard answers without touching the database, so the honest answer takes
// microseconds; without it the request waits for the one connection this caller's own
// transaction or cursor holds, which no one will give back, and this bound is what makes that a
// failure here instead of a hang that runs until the test binary's own timeout kills the package.
const refusalBound = 10 * time.Second

// refusalLine is the message logRefusal writes, which is what a count of refusals in the log
// counts.
const refusalLine = "second pooled connection requested while one is held"

// A caller holding one connection of the pool is refused a second, by name and at once, whichever
// of the pool's two holders holds it: a transaction, asked for a second one the way the daemon
// opens its transactions (pgx.BeginFunc, which Store.Tx is), and an open cursor, asked for a
// query while the caller's loop over its rows has not ended. Each refusal is logged with the
// stack of the site that asked. The hold ends where the connection is given back — a transaction
// that commits, a cursor that Next drained or Close closed — and the same caller's next call is
// served. A caller that holds no connection of the pool is served: the guard refuses a second
// connection, never a first.
//
// One connection in the pool, so a connection the caller holds is the only one there is: a
// request the guard did not refuse would have nothing to be served with and would wait, and so
// would the call after a hold whose connection the pool never got back.
func TestASecondPooledConnectionUnderAHeldOneIsRefused(t *testing.T) {
	store := storeWith(t, map[string]string{"pool_max_conns": "1"})
	pool := store.Pool()
	logged := captureLog(t)

	// The negative control the incident's check names: the same requests, made by a caller that
	// holds none of this pool's connections, are served.
	served(t, pool, WithTransactionTracking(context.Background()), "a caller holding no connection")

	// The caller's context is marked as the daemon's sequential flows mark theirs: an API route
	// where the mux is built, the boot for its own steps.
	ctx := WithTransactionTracking(context.Background())

	var inner error
	var took time.Duration
	outer := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// A statement, so the transaction is open on the server and not only in the driver.
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			return err
		}
		asking, cancel := context.WithTimeout(ctx, refusalBound)
		defer cancel()
		started := time.Now()
		inner = pgx.BeginFunc(asking, pool, func(pgx.Tx) error { return nil })
		took = time.Since(started)
		return inner
	})
	if !errors.Is(inner, ErrNestedAcquire) {
		t.Fatalf("the transaction asked for inside the held one = %v after %s, want %v", inner, took.Truncate(time.Millisecond), ErrNestedAcquire)
	}
	if !errors.Is(outer, ErrNestedAcquire) {
		t.Fatalf("the holding transaction ended with %v, want the refusal its body returned", outer)
	}
	refusalsLogged(t, logged, 1)

	// The holding transaction rolled back with the refusal; one that commits gives its connection
	// back as well, and the caller's next call is served.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "select 1")
		return err
	}); err != nil {
		t.Fatalf("the transaction that commits: %v", err)
	}
	served(t, pool, ctx, "the caller whose transaction committed")

	rows, err := pool.Query(ctx, "select generate_series(1, 2)")
	if err != nil {
		t.Fatalf("open the caller's cursor: %v", err)
	}
	asking, cancel := context.WithTimeout(ctx, refusalBound)
	started := time.Now()
	second, err := pool.Query(asking, "select 1")
	took = time.Since(started)
	cancel()
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("the query asked for under the caller's open cursor = %v after %s, want %v", err, took.Truncate(time.Millisecond), ErrNestedAcquire)
	}
	refusalsLogged(t, logged, 2)

	// Drained: the Next that finds no more rows gives the cursor's connection back, with no Close.
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("drain the caller's cursor: %v", err)
	}
	served(t, pool, ctx, "the caller whose cursor was drained")
	rows.Close()

	// Closed: a Close before the rows run out gives it back too.
	rows, err = pool.Query(ctx, "select generate_series(1, 2)")
	if err != nil {
		t.Fatalf("open the cursor to close: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("the cursor to close has no first row: %v", rows.Err())
	}
	rows.Close()
	served(t, pool, ctx, "the caller whose cursor was closed")

	served(t, pool, WithTransactionTracking(context.Background()), "a caller holding no connection")
	// None of the served calls logged a refusal.
	refusalsLogged(t, logged, 2)
}

// Each call site that asks for a second connection under a held one logs a line of its own, and a
// site that asks again logs nothing more. Both sites here ask through pgx.BeginFunc, whose own
// frame stands above the pool's at every one of its callers, as intake.ApplyFact's does for every
// fact: a log remembered by the frame above the pool would let the first refusal through either
// silence every other caller's, so the site a refusal is remembered by is the caller's.
func TestEachRefusingSiteLogsItsOwnLine(t *testing.T) {
	store := storeWith(t, map[string]string{"pool_max_conns": "1"})
	pool := store.Pool()
	logged := captureLog(t)

	ctx := WithTransactionTracking(context.Background())
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			return err
		}
		asking, cancel := context.WithTimeout(ctx, refusalBound)
		defer cancel()
		firstSite := func() error { return pgx.BeginFunc(asking, pool, func(pgx.Tx) error { return nil }) }

		if err := firstSite(); !errors.Is(err, ErrNestedAcquire) {
			t.Fatalf("the first site's transaction under the held one = %v, want %v", err, ErrNestedAcquire)
		}
		refusalsLogged(t, logged, 1)
		if err := pgx.BeginFunc(asking, pool, func(pgx.Tx) error { return nil }); !errors.Is(err, ErrNestedAcquire) {
			t.Fatalf("the second site's transaction under the held one = %v, want %v", err, ErrNestedAcquire)
		}
		refusalsLogged(t, logged, 2)
		if err := firstSite(); !errors.Is(err, ErrNestedAcquire) {
			t.Fatalf("the first site's transaction asked for again = %v, want %v", err, ErrNestedAcquire)
		}
		refusalsLogged(t, logged, 2)
		return nil
	}); err != nil {
		t.Fatalf("the holding transaction: %v", err)
	}
}

// served fails the test unless the pool serves caller a read within refusalBound: a hold the pool
// never ended refuses it, and a connection it never got back leaves it, in a pool of one,
// nothing to be served with.
func served(t *testing.T, pool *Pool, caller context.Context, who string) {
	t.Helper()
	asking, cancel := context.WithTimeout(caller, refusalBound)
	defer cancel()
	var one int
	if err := pool.QueryRow(asking, "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("the read asked for by %s answered %d, %v; want it served", who, one, err)
	}
}

// refusalsLogged fails the test unless the log holds exactly want refusals, each with a stack that
// names this test, which is what names the path to repair.
func refusalsLogged(t *testing.T, logged *bytes.Buffer, want int) {
	t.Helper()
	lines := refusals(logged.String())
	if len(lines) != want {
		t.Fatalf("the log holds %d refusals, want %d:\n%s", len(lines), want, logged.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, t.Name()) {
			t.Fatalf("a refusal's line names no site this test can be found by:\n%s", line)
		}
	}
}

// captureLog sends the process's default logger into a buffer for the length of the test, which
// is where logRefusal writes, and gives the test the buffer to read. It forgets the sites logged
// before the test, so a test the binary runs again (-count) logs its own sites again.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	loggedSites.Lock()
	clear(loggedSites.seen)
	loggedSites.Unlock()
	return &logged
}

// refusals is every line of log that reports a refused second connection. The stack the line
// carries is written as one quoted value, so a refusal is one line however deep the stack is.
func refusals(log string) []string {
	var found []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, refusalLine) {
			found = append(found, line)
		}
	}
	return found
}
