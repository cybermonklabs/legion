package storetest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// reopenedPool opens a second pool on pool's database, from the configuration pool was opened
// with and whatever change configure makes to a copy of it, and closes it after t ends. The
// production pool is built in store.Open and is not a seam a test may configure, so every test
// that needs a pool of its own shape opens it here: Traced, Reopened, SingleConnection and
// SecondProcess.
func reopenedPool(t testing.TB, pool *store.Pool, configure func(*pgxpool.Config)) *store.Pool {
	t.Helper()
	config := pool.Config()
	configure(config)
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a second pool on the same database: %v", err)
	}
	reopened := store.NewPool(opened)
	t.Cleanup(reopened.Close)
	return reopened
}

// Reopened is a store on database's own database whose pool reopenedPool opens with configure's
// change to its configuration.
func Reopened(t testing.TB, database *store.Store, configure func(*pgxpool.Config)) *store.Store {
	t.Helper()
	return &store.Store{Pool: reopenedPool(t, database.Pool, configure)}
}

// SingleConnection is a Reopened store of one connection, which every read on it takes, so a test
// reads one connection's own state - the statements it has prepared (pg_prepared_statements) - as
// the reads it drives leave it.
func SingleConnection(t testing.TB, database *store.Store) *store.Store {
	t.Helper()
	return Reopened(t, database, func(config *pgxpool.Config) {
		config.MaxConns = 1
		config.MinConns = 0
		config.MinIdleConns = 0
	})
}

// Traced opens a pool on pool's database whose every statement tracer sees, with params set on
// each of its connections, closed after t ends. The lease and listener connections opened from its
// configuration (store.Pool.Leases, store.Pool.Listener) are traced too. Only the code under test
// is given it, so everything tracer sees is that code's. pgx reports one query start per statement
// sent on its own and one batch start per batch, whatever that batch holds, so a tracer that also
// implements pgx.BatchTracer counts a read's round trips.
func Traced(t testing.TB, pool *store.Pool, tracer pgx.QueryTracer, params map[string]string) *store.Pool {
	t.Helper()
	return reopenedPool(t, pool, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = tracer
		for name, value := range params {
			config.ConnConfig.RuntimeParams[name] = value
		}
	})
}

// StatementCounter counts every statement started on the pool it traces (Traced).
type StatementCounter struct{ count atomic.Int64 }

func (c *StatementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.count.Add(1)
	return ctx
}

func (c *StatementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Count is how many statements c has counted.
func (c *StatementCounter) Count() int64 {
	return c.count.Load()
}

// AwaitQuiet waits until counter counts no statement over a span of quiet, which is how a loop
// that writes nothing shows it has gone idle. Each span is a testwait.Span held for why, which
// says why neither the state the code leaves, synctest.Wait nor a later action's effect shows the
// idle instead.
func AwaitQuiet(t testing.TB, counter *StatementCounter, quiet time.Duration, why string) {
	t.Helper()
	testwait.Eventually(t, fmt.Sprintf("a span of %v in which no statement starts", quiet), func() bool {
		before := counter.Count()
		testwait.Span(t, quiet, why)
		return counter.Count() == before
	})
}

// RequireNoStatements fails if counter counts any statement over a span of window, a
// testwait.Span held for why, which says why neither the state the code leaves, synctest.Wait nor
// a later action's effect proves the absence instead.
func RequireNoStatements(t testing.TB, counter *StatementCounter, window time.Duration, what, why string) {
	t.Helper()
	before := counter.Count()
	testwait.Span(t, window, why)
	if made := counter.Count() - before; made != 0 {
		t.Errorf("%s: %d statements over %v, want none", what, made, window)
	}
}

// StatementRecorder records the SQL of every statement run on the pool it traces (Traced).
type StatementRecorder struct {
	mu  sync.Mutex
	sql []string
}

func (r *StatementRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	r.sql = append(r.sql, data.SQL)
	r.mu.Unlock()
	return ctx
}

func (r *StatementRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Statements is the SQL of every statement r has recorded, in the order they started.
func (r *StatementRecorder) Statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sql...)
}
