package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/pgguard"
)

// ErrNestedAcquire is the daemon's pool refusing a second connection to a caller that already
// holds one of its connections. It is pgguard's ErrNestedAcquire, the one value Dispatch's pool
// refuses with too, so errors.Is matches either name.
//
// One caller, one connection, is the rule this pool cannot survive without. A transaction holds
// its connection until it commits, and a caller that asks for a second one while it holds the
// first waits for a connection only the callers queued behind it can give back. INFRA-3169 is
// that cycle measured: three call sites asked the pool for a connection while holding one of
// their own — the boot's report of stranded claims, the mapping refresh and the state route —
// and one of them held the global fact serializer (intake.ApplyFact's advisory lock) while it
// waited, which is the lock every other fact of every tree queues behind. On the production
// deployment all 32 connections sat `idle in transaction` with `active` at 1 to 2, Postgres
// reported no deadlock — it sees connections waiting for a client, not for each other — and
// every claim write and state read timed out.
//
// A bigger pool does not fix it and rationing connections does not either: what queues behind
// the fact serializer is unbounded — every Dispatch event, every supervision terminal, every
// outbox row's fact — so a larger pool only moves the number of callers it takes to close the
// cycle. Work that genuinely needs a connection of its own while it holds one has to read
// through the transaction it is already inside, which is how all three sites were repaired.
//
// This error makes the rule enforce itself: every entry point that opens one of this pool's
// transactions marks its context (api.NewServer marks every route; the intake of a message, the
// outbox's lanes, the boot's own steps and the supervision callbacks that apply facts mark their
// own), and a connection asked for under a held one fails here, loudly, at the call that asked
// for it, rather than wedging the deployment.
//
// Its limit is the mark. A context nothing marked is not tracked, so a caller holding a
// connection under such a context is refused nothing and can still close the cycle; what the
// guard proves is only as wide as the entry points that mark. That is why a new long-lived
// goroutine, a new route or a new fact source marks its context where it starts, and why a
// refusal is a defect at the call it names rather than a mark to take off.
//
// The guard itself is packages/envoy/pgguard, the one implementation this pool and Dispatch's
// share. Dispatch's store builds two more marks on it that no call site of this daemon needs: a
// mark a caller sets around a connection it holds itself (its HoldsConnection; this pool hands a
// connection out only through acquire, which sets that mark itself) and a per-read mark for reads
// of one caller that run side by side (its ForConcurrentRead). Each is two calls of pgguard, so a
// daemon that needs one writes those two calls here.
var ErrNestedAcquire = pgguard.ErrNestedAcquire

// guardProgram is the name this pool's refusals are logged under (pgguard.Options).
const guardProgram = "legion"

// WithTransactionTracking marks ctx so the pool can refuse its caller a second connection while
// one of its transactions or cursors is open (pgguard.WithTransactionTracking, which hands back
// the mark ctx already carries). It is the mark of a sequential flow, one call after another on
// one goroutine: every API route (api's trackTransactions, where the mux is built), the boot's
// steps, its readiness probes and its replay of the terminal facts no daemon applied. A goroutine
// that runs beside others takes ForConcurrentWorker instead.
func WithTransactionTracking(ctx context.Context) context.Context {
	return pgguard.WithTransactionTracking(ctx)
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side
// for as long as the caller lives, with a hold mark of its own, always a fresh one
// (pgguard.ForConcurrentWorker says how it differs from WithTransactionTracking's). A goroutine
// that runs beside others and reaches this pool takes one, outside any transaction or connection
// of this pool's — a process-lifetime context, as the daemon's loops run off — since a fresh mark
// cannot see a connection the parent holds. A caller that takes none is one the guard cannot see:
// the sandbox launcher's frame loop is such a caller, deleting a restored workspace bundle on a
// context made from context.Background() (runtime/sandbox/launcher.go, restoreReported), and the
// pool refuses it nothing.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return pgguard.ForConcurrentWorker(ctx)
}

// Pool is the daemon's connection pool. Every way it hands out a connection refuses one asked for
// while the caller already holds one; see ErrNestedAcquire. It hands out only what the guarded
// pool under it (pgguard.Pool) does, and the pgx pool is private so no caller can reach past the
// refusal: Store.Pool hands out this type and nothing else.
type Pool struct {
	guarded *pgguard.Pool
	pool    *pgxpool.Pool
}

// NewPool wraps an open pgx pool. The daemon's own pool is opened by Open; a test that builds a
// pool of its own wraps it here, so what it exercises is the pool the daemon runs on.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{guarded: pgguard.New(pool, pgguard.Options{Program: guardProgram}), pool: pool}
}

// Query runs a query on a pooled connection. The rows hold that connection until they close, so
// the caller counts as holding one for as long as they are open: a cursor is one of this pool's
// two connection-holders — a transaction and an open cursor — and work done inside the loop over
// the rows is exactly the shape that wedged the deployment.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return p.guarded.Query(ctx, sql, args...)
}

// QueryRow runs a single-row query on a pooled connection. The connection is given back when the
// row is scanned, so the caller holds none afterwards; a refusal is carried by the row, out of
// Scan.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.guarded.QueryRow(ctx, sql, args...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.guarded.Exec(ctx, sql, args...)
}

// acquire hands out one of the pool's connections to a caller that must run its own work on the
// connection after its transaction ends and before the connection returns to the pool. Store.apply
// is the one such caller: it resets the session of the connection that a migration ran on
// (releaseMigrationConn). A transaction from Begin cannot serve it, because pgx returns that
// transaction's connection to the pool as the transaction ends. acquire runs the guard first, as
// every other way this pool hands out a connection does. Then it marks the caller as holding a
// connection, as Begin does. The caller runs release only once the connection is back in the pool
// or closed, so the guard sees the connection for as long as the caller holds it. release is never
// nil: for a context the pool cannot see, it does nothing.
func (p *Pool) acquire(ctx context.Context) (conn *pgxpool.Conn, release func(), err error) {
	conn, err = p.guarded.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release = pgguard.Hold(ctx)
	if release == nil {
		release = func() {}
	}
	return conn, release, nil
}

// Begin opens a transaction and marks the caller as holding a connection. It is what
// pgx.BeginFunc opens its transaction through, and with it Store.Tx, so every call site that
// wraps a transaction in a function keeps working as it was written.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	return p.guarded.Begin(ctx)
}

// BeginTx opens a transaction with options and marks the caller as holding a connection. It is
// what pgx.BeginTxFunc opens its transaction through, and what intake.ApplyFact opens each
// fact's with.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	return p.guarded.BeginTx(ctx, options)
}

// Ping opens a connection and asks the server to answer on it, which is how Open proves the
// database is reachable before a Store is handed to the daemon.
func (p *Pool) Ping(ctx context.Context) error {
	return p.guarded.Ping(ctx)
}

// Close releases every connection the pool holds and returns once every connection in use has
// come back.
func (p *Pool) Close() { p.pool.Close() }

// Reset closes every connection the pool holds idle and takes back every connection in use as it
// is given back, so the work after it runs on connections opened since: the daemon's stop resets
// the pool before it stamps the boot's end (daemon.go).
func (p *Pool) Reset() { p.pool.Reset() }

// Config returns a copy of the pool's configuration, which is how a caller reads the DSN the pool
// was opened on.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }
