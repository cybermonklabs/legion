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

// refusalBound is how long the second connection is allowed to be asked for before the test calls
// the answer a wait. The guard answers without touching the database, so the honest answer takes
// microseconds; without it the acquire waits for the one connection this caller's own transaction
// holds, which no one will give back, and this bound is what makes that a failure here instead of
// a hang that runs until the test binary's own timeout kills the package.
const refusalBound = 10 * time.Second

// refusalLine is the message logRefusal writes, which is what a count of refusals in the log
// counts.
const refusalLine = "second pooled connection requested while one is held"

// A caller holding one connection of the pool is refused a second, by name and at once, and the
// refusal is logged once with the stack of the site that asked. The same request from a caller
// that holds no connection of that pool is served: the guard refuses a second connection, never a
// first.
//
// One connection in the pool, so the connection the held transaction holds is the only one there
// is: an acquire the guard did not refuse would have nothing to be served with and would wait.
func TestASecondPooledConnectionUnderAHeldOneIsRefused(t *testing.T) {
	store := storeWith(t, map[string]string{"pool_max_conns": "1"})
	pool := store.Pool()
	logged := captureLog(t)

	// The caller's context is marked as an entry point of the daemon's marks it: the API's routes
	// where the mux is built, the intake for each message, the boot for its own steps.
	ctx := WithTransactionTracking(context.Background())
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the pool's one connection: %v", err)
	}
	// A statement, so the transaction is open on the server and not only in the driver.
	if _, err := tx.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("the first statement of the holding transaction: %v", err)
	}

	asking, cancel := context.WithTimeout(ctx, refusalBound)
	defer cancel()
	started := time.Now()
	conn, err := pool.Acquire(asking)
	took := time.Since(started)
	if conn != nil {
		conn.Release()
	}
	if !errors.Is(err, ErrNestedAcquire) {
		t.Fatalf("the second connection asked for under the held one = %v after %s, want %v", err, took.Truncate(time.Millisecond), ErrNestedAcquire)
	}

	// One line, with the stack of the caller that asked, which is what names the path to repair.
	lines := refusals(logged.String())
	if len(lines) != 1 {
		t.Fatalf("the refusal logged %d lines, want exactly one:\n%s", len(lines), logged.String())
	}
	if !strings.Contains(lines[0], "TestASecondPooledConnectionUnderAHeldOneIsRefused") {
		t.Fatalf("the refusal's line names no site this test can be found by:\n%s", lines[0])
	}

	// The transaction is what held the connection, so its end is what frees the caller's mark.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back the holding transaction: %v", err)
	}

	// The negative control the incident's check names: the same request, made by a caller that
	// holds none of this pool's connections, is served.
	serving, cancelServing := context.WithTimeout(WithTransactionTracking(context.Background()), refusalBound)
	defer cancelServing()
	free, err := pool.Acquire(serving)
	if err != nil {
		t.Fatalf("the connection asked for by a caller holding none = %v, want it served", err)
	}
	var one int
	if err := free.QueryRow(serving, "select 1").Scan(&one); err != nil {
		t.Fatalf("read on the connection the pool served: %v", err)
	}
	free.Release()
	if one != 1 {
		t.Fatalf("the served connection answered %d, want 1", one)
	}
	if lines := refusals(logged.String()); len(lines) != 1 {
		t.Fatalf("the served request logged a refusal; the log holds %d:\n%s", len(lines), logged.String())
	}
	// A transaction opened the same way is served too, so what the guard refuses is the second
	// connection and not this pool's transactions.
	after, err := pool.BeginTx(WithTransactionTracking(context.Background()), pgx.TxOptions{})
	if err != nil {
		t.Fatalf("the transaction opened after the hold ended = %v, want it served", err)
	}
	if err := after.Rollback(context.Background()); err != nil {
		t.Fatalf("roll back the transaction opened after the hold ended: %v", err)
	}
}

// captureLog sends the process's default logger into a buffer for the length of the test, which
// is where logRefusal writes, and gives the test the buffer to read.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
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
