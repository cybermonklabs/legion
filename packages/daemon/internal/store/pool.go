package store

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNestedAcquire is the daemon's pool refusing a second connection to a caller that already
// holds one of its connections.
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
var ErrNestedAcquire = errors.New(
	"this caller already holds a connection of the pool: a second one would deadlock it",
)

// holding records whether the context's caller holds a connection of this pool: its transaction,
// an open cursor of its own, or one it acquired directly. One writer sets it and one reader tests
// it, so a flag is the whole state — a transaction never opens a second transaction of its own.
type holding struct {
	connection atomic.Bool
}

// holdingKey is the context key the mark lives under. It holds a pointer, so a transaction body
// that runs on the context it was opened with is covered with no threading of its own.
type holdingKey struct{}

// WithTransactionTracking marks ctx so the pool can refuse a second connection while one of its
// transactions is open, and hands back a context already marked as it is, so a caller inside a
// marked one does not get a mark that cannot see the connection the caller holds. Every API
// request is marked where the mux is built; every other entry point that opens a transaction —
// the intake of a message, the outbox's lanes, the boot's steps, the supervision callbacks that
// apply facts — marks its own context.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(holdingKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// HoldsConnection marks ctx as already holding a pooled connection until the returned release
// runs, so a connection asked for under it is refused like one asked for inside a transaction.
// It is for a caller that holds a connection the pool cannot see it holding: one taken with
// Acquire, which the caller gives back itself.
func HoldsConnection(ctx context.Context) (context.Context, func()) {
	ctx = WithTransactionTracking(ctx)
	return ctx, hold(ctx)
}

// hold records that ctx's caller now holds a connection of this pool and returns the release that
// clears it, or nil for a context the pool cannot see, which has no mark to clear. The release
// runs at most once: a holder that releases twice — a transaction that commits and then runs its
// deferred rollback, a cursor drained and then closed — must not clear the mark of whatever holds
// a connection by then.
func hold(ctx context.Context) func() {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	if !ok {
		return nil
	}
	state.connection.Store(true)
	return sync.OnceFunc(func() { state.connection.Store(false) })
}

// holdsConnection answers whether ctx's caller holds a connection of this pool now. A context
// nothing marked holds none as far as the pool can tell, which is the guard's own limit
// (ErrNestedAcquire).
func holdsConnection(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// ForConcurrentRead returns a context for one of a caller's reads that run side by side: ctx's
// deadline, cancellation and values, with a hold mark of its own, so that read takes one
// connection and the pool still refuses it a second. Reads sharing one mark would refuse each
// other, since one read's open cursor marks the context they share. A caller already holding a
// connection (its transaction, a cursor, an acquired connection) is refused with
// ErrNestedAcquire, because each of its reads would be a second connection taken while it holds
// one.
func ForConcurrentRead(ctx context.Context) (context.Context, error) {
	if holdsConnection(ctx) {
		logRefusal(refusalFromMark)
		return nil, ErrNestedAcquire
	}
	return context.WithValue(ctx, holdingKey{}, &holding{}), nil
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side
// for as long as the caller lives: ctx's deadline, cancellation and values, with a hold mark of
// its own, always a fresh one. A worker that takes a connection — a transaction, a cursor — marks
// only itself, so its siblings are still free to take theirs, and the pool still refuses each
// worker a second connection of its own. The outbox's lanes, the workflow runtime's loops and the
// serving group's goroutines each run on one.
//
// It differs from the other two marks by never reusing ctx's: WithTransactionTracking hands back
// the mark ctx already carries, which siblings sharing it would set and clear under each other,
// and ForConcurrentRead refuses a caller that holds a connection. A worker's mark is its own
// whatever the parent carries, so the caller must start these goroutines outside any transaction
// or connection of this pool's — a process-lifetime context, as the daemon's loops run off — since
// a fresh mark cannot see a connection the parent holds.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// loggedSites remembers the call sites that have already logged a refusal, so a caller that trips
// the guard in a loop reports its stack once instead of flooding the log. The error itself is
// returned to every caller, every time.
var loggedSites sync.Map

// refusalFromMethod and refusalFromMark are how many frames above runtime.Callers the call site
// that asked for the connection stands, which is the frame a refusal is remembered and logged
// under: from a guarded method of the pool, runtime.Callers, logRefusal, guard and the method
// itself; from ForConcurrentRead, which refuses without the guard, one frame fewer.
const (
	refusalFromMethod = 4
	refusalFromMark   = 3
)

// logRefusal logs one refusal per call site, with the stack that asked for the second connection,
// which is what names the path to repair. skip says where that site stands (refusalFromMethod).
func logRefusal(skip int) {
	var caller [1]uintptr
	if runtime.Callers(skip, caller[:]) == 0 {
		return
	}
	if _, seen := loggedSites.LoadOrStore(caller[0], struct{}{}); seen {
		return
	}
	slog.Error("legion: second pooled connection requested while one is held",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// Pool is the daemon's connection pool. Every way it hands out a connection refuses one asked for
// while the caller already holds one; see ErrNestedAcquire. The pgx pool is private so no caller
// can reach past the refusal: Store.Pool hands out this type and nothing else.
type Pool struct {
	pool *pgxpool.Pool
}

// NewPool wraps an open pgx pool. The daemon's own pool is opened by Open; a test that builds a
// pool of its own wraps it here, so what it exercises is the pool the daemon runs on.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{pool: pool}
}

// guard refuses a connection to a caller that already holds one, and records the call site that
// asked (logRefusal). It runs in each exported method rather than here in one place below them,
// so the site a refusal names is the caller's and not one of this pool's own frames.
func (p *Pool) guard(ctx context.Context) error {
	if !holdsConnection(ctx) {
		return nil
	}
	logRefusal(refusalFromMethod)
	return ErrNestedAcquire
}

// Query runs a query on a pooled connection. The rows hold that connection until they close, so
// the caller counts as holding one for as long as they are open: a cursor is one of this pool's
// three connection-holders — a transaction, an open cursor and a connection taken with Acquire —
// and work done inside the loop over the rows is exactly the shape that wedged the deployment.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	release := hold(ctx)
	if release == nil {
		return rows, nil
	}
	return &trackedRows{Rows: rows, release: release}, nil
}

// trackedRows clears the caller's mark when pgx hands the cursor's connection back: an explicit
// Close, a Next that runs out of rows, or a Scan or Values error, which pgx treats as fatal and
// closes the rows on.
type trackedRows struct {
	pgx.Rows
	release func()
}

func (r *trackedRows) Close() {
	defer r.release()
	r.Rows.Close()
}

func (r *trackedRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.release()
	return false
}

func (r *trackedRows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	if err != nil {
		r.release()
	}
	return err
}

func (r *trackedRows) Values() ([]any, error) {
	values, err := r.Rows.Values()
	if err != nil {
		r.release()
	}
	return values, err
}

// QueryRow runs a single-row query on a pooled connection. The connection is given back when the
// row is scanned, so the caller holds none afterwards and the row carries no mark of its own; a
// refusal is carried by the row, which has no error return of its own (errRow).
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := p.guard(ctx); err != nil {
		return errRow{err: err}
	}
	return p.pool.QueryRow(ctx, sql, args...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := p.guard(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.pool.Exec(ctx, sql, args...)
}

// Acquire takes a pooled connection the caller holds until it releases it. The pool cannot see
// that release, so a caller that reads through the pool while it holds the connection marks its
// own context for the length of the hold (HoldsConnection).
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.pool.Acquire(ctx)
}

// Begin opens a transaction and marks the caller as holding a connection.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the caller as holding a connection. It is
// what pgx.BeginFunc and pgx.BeginTxFunc call through, so every call site that wraps a
// transaction in a function keeps working as it was written.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, options)
}

// begin is the guarded body both transaction entry points share: guard runs in the exported
// method, as it does in every exported method that hands out one of this pool's connections, so a
// refusal records the caller that asked for the transaction rather than one of this pool's own
// frames. A transaction on a context the pool cannot see has no mark to clear and is handed on as
// pgx opened it: a trackedTx around it would only forward its Commit and Rollback.
func (p *Pool) begin(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	release := hold(ctx)
	if release == nil {
		return tx, nil
	}
	return &trackedTx{Tx: tx, release: release}, nil
}

// trackedTx clears the caller's mark however the transaction ends. Callers commit and then run a
// deferred rollback; the release absorbs that second call (hold).
type trackedTx struct {
	pgx.Tx
	release func()
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
}

// errRow carries a refusal through the single-row surface, which has no error return of its own:
// the caller reads it where it would read a query's failure, out of Scan.
type errRow struct {
	err error
}

func (r errRow) Scan(...any) error { return r.err }

// Ping opens a connection and asks the server to answer on it, which is how Open proves the
// database is reachable before a Store is handed to the daemon.
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.guard(ctx); err != nil {
		return err
	}
	return p.pool.Ping(ctx)
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
