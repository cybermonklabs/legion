package storetest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
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
	testcleanup.Add(t, reopened.Close)
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

// AwaitQuiet waits until counter has counted no statement for quiet, failing once within passes.
func AwaitQuiet(t testing.TB, counter *StatementCounter, quiet, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last, since := counter.Count(), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("statements did not stop for %v within %v", quiet, within)
		}
		time.Sleep(20 * time.Millisecond)
		if n := counter.Count(); n != last {
			last, since = n, time.Now()
		}
	}
}

// RequireNoStatements fails if counter counts any statement over window.
func RequireNoStatements(t testing.TB, counter *StatementCounter, window time.Duration, what string) {
	t.Helper()
	before := counter.Count()
	time.Sleep(window)
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

// CommitBetweenReads runs the commit it was armed with, once, the moment the first statement
// naming one of its tables ends on the pool it traces (Traced), and holds every other statement
// naming one of them from starting until that commit has returned: a commit landing between a
// reader's statements on those tables, whether they run in turn or side by side, made without
// timing. The commit runs on a pool it does not trace, since a statement of its own naming one of
// the tables would wait for it.
type CommitBetweenReads struct {
	tables   []string
	mu       sync.Mutex
	commit   func()
	inFlight bool
	done     chan struct{}
	fired    bool
}

// NewCommitBetweenReads is a CommitBetweenReads on the statements whose SQL names one of tables.
func NewCommitBetweenReads(tables ...string) *CommitBetweenReads {
	return &CommitBetweenReads{tables: tables}
}

// commitsAtEnd marks the context of the statement whose end runs the armed commit.
type commitsAtEnd struct{}

func (c *CommitBetweenReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !slices.ContainsFunc(c.tables, func(table string) bool { return strings.Contains(data.SQL, table) }) {
		return ctx
	}
	c.mu.Lock()
	if c.commit == nil {
		c.mu.Unlock()
		return ctx
	}
	if c.inFlight {
		done := c.done
		c.mu.Unlock()
		<-done
		return ctx
	}
	c.inFlight = true
	c.mu.Unlock()
	return context.WithValue(ctx, commitsAtEnd{}, true)
}

func (c *CommitBetweenReads) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(commitsAtEnd{}) == nil {
		return
	}
	c.mu.Lock()
	commit, done := c.commit, c.done
	c.mu.Unlock()
	commit()
	c.mu.Lock()
	c.commit, c.inFlight, c.fired = nil, false, true
	c.mu.Unlock()
	close(done)
}

// Arm sets the commit the next statement naming one of the tables ends with.
func (c *CommitBetweenReads) Arm(commit func()) {
	c.mu.Lock()
	c.commit, c.done, c.fired = commit, make(chan struct{}), false
	c.mu.Unlock()
}

// Fired is whether the armed commit ran.
func (c *CommitBetweenReads) Fired() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}
