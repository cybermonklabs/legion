// Package pgguard is the nested-acquire guard of a pgx connection pool: a caller that already
// holds one of a guarded pool's connections is refused a second one, at the call that asks for it,
// rather than left waiting for a connection that only it can give back (ErrNestedAcquire).
//
// Two stores wrap their pools in it: Dispatch's (internal/dispatch/store) and the Legion daemon's
// (packages/daemon/internal/store). The daemon is a Go module of its own, and Go's internal rule
// stops each of the two modules from importing the other's internal packages, so the guard is a
// public package of this module: one implementation, which a fix reaches in both pools at once.
// Each store keeps its own exported marks and its own ErrNestedAcquire, which are this package's,
// and says there why its pool cannot survive without the rule.
//
// The guard reads a mark on the caller's context. WithTransactionTracking marks a sequential flow,
// ForConcurrentWorker gives each goroutine of a group a mark of its own, and a context nothing
// marked is not tracked: a caller that holds a connection under such a context is refused nothing.
// What the guard proves is only as wide as the entry points that mark their contexts.
package pgguard

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

// ErrNestedAcquire is a guarded pool refusing a second connection to a caller that already holds
// one of its connections.
//
// One caller, one connection, is the rule a pool cannot survive without. A transaction holds its
// connection until it commits, and a caller that asks for a second one while it holds the first
// waits for a connection that only the callers queued behind it can give back. When the caller
// also holds a lock those callers wait on, nothing gives one back: Postgres sees connections that
// wait for a client, not for each other, so it reports no deadlock. A bigger pool only moves the
// number of callers it takes to close the cycle. Work that needs a connection of its own while it
// holds one reads through the transaction it is already inside, or takes its connection from a
// pool of its own.
var ErrNestedAcquire = errors.New(
	"this caller already holds a connection of the pool: a second one would deadlock it",
)

// holding records whether the context's caller holds a connection of a guarded pool: its
// transaction, an open cursor, or a connection it holds itself (Hold). One writer sets it and one
// reader tests it, so a flag is the whole state: a transaction never opens a second transaction of
// its own.
type holding struct {
	connection atomic.Bool
}

// holdingKey is the context key the mark lives under. It holds a pointer, so a transaction body
// that runs on the context it was opened with is covered with no threading of its own.
type holdingKey struct{}

// WithTransactionTracking marks ctx so a guarded pool can refuse its caller a second connection
// while one of its transactions, cursors or held connections is open. It hands back a context
// already marked as it is, so a caller inside a marked flow does not get a mark that cannot see the
// connection the caller holds. It is the mark of a sequential flow, one call after another on one
// goroutine. A goroutine that runs beside others takes ForConcurrentWorker instead.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(holdingKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side: ctx's
// deadline, cancellation and values, with a hold mark of its own, always a fresh one. A worker that
// takes a connection marks only itself, so its siblings are still free to take theirs, and the pool
// still refuses each worker a second connection of its own.
//
// It differs from WithTransactionTracking by never reusing ctx's mark: that one hands back the mark
// ctx already carries, which siblings sharing it would set and clear under each other. A worker's
// mark is its own whatever the parent carries, so the caller starts these goroutines outside any
// transaction or connection of a guarded pool: a fresh mark cannot see a connection the parent
// holds.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// Hold records that ctx's caller now holds a connection of a guarded pool, and returns the release
// that clears the mark, or nil for a context nothing marked, which has no mark to clear. The
// release runs at most once: a holder that releases twice - a transaction that commits and then
// runs its deferred rollback, a cursor drained and then closed - must not clear the mark of
// whatever holds a connection by then. The pool's transactions and cursors hold through it; a
// caller that takes a connection of its own (pgxpool's Acquire) holds through it too, for as long as
// it keeps that connection.
func Hold(ctx context.Context) func() {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	if !ok {
		return nil
	}
	state.connection.Store(true)
	return sync.OnceFunc(func() { state.connection.Store(false) })
}

// Holds answers whether ctx's caller holds a connection of a guarded pool now. A context nothing
// marked holds none as far as the guard can tell, which is the guard's own limit.
func Holds(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// Guard refuses a connection to a caller that already holds one: it returns ErrNestedAcquire, and
// logs the stack that asked (logRefusal) under program, the name of the process the pool serves.
// A caller that holds none passes. Every way a guarded pool hands out a connection runs it before
// it asks pgx for anything, so a refusal touches neither the pool nor the database.
func Guard(ctx context.Context, program string) error {
	if !Holds(ctx) {
		return nil
	}
	logRefusal(program)
	return ErrNestedAcquire
}

// loggedSites is the calling stacks that have already logged a refusal: one line per distinct stack
// that reaches the guard. A site reached by N different paths logs at most N lines, one per path,
// and a call repeated in a loop has the same stack every time, so it logs once instead of flooding
// the log. The error itself is returned to every caller, every time. The key is the stack's program
// counters themselves (refusalDepth), a comparable array, so a stack already seen is found with no
// allocation and no walk of its frames. A function the compiler copies into each of its callers
// stands at a different counter in each copy, but each copy is on a path of its own, so it adds no
// line that path would not log anyway.
var loggedSites = struct {
	sync.Mutex
	seen map[[refusalDepth]uintptr]struct{}
}{seen: map[[refusalDepth]uintptr]struct{}{}}

// refusalDepth is how many frames of a refused call's stack, from the guard outwards, make up its
// key. The frames nearest the guard - Guard, the pool's method, pgx's wrapper, a store's forwarder
// the compiler keeps whole - are the same program counters on every path through them; the frames
// beyond them are the ones that tell the paths into the pool apart, so two stacks that differ only
// beyond this depth share one line.
const refusalDepth = 32

// logRefusal logs a refusal once per distinct calling stack, with that stack, which is what names
// the path to repair. The stack, read into a fixed array, is itself the key (loggedSites): a
// function that only passes a call on to the pool - pgx.BeginFunc, a store's method, any function
// of the caller's own - stands in the stacks of all its callers without making them one, so no
// caller's refusal silences another's. The stack is formatted only on a stack's first refusal; a
// refusal of a stack already seen costs the read of its program counters and a map lookup.
func logRefusal(program string) {
	var stack [refusalDepth]uintptr
	runtime.Callers(2, stack[:])
	loggedSites.Lock()
	_, seen := loggedSites.seen[stack]
	if !seen {
		loggedSites.seen[stack] = struct{}{}
	}
	loggedSites.Unlock()
	if seen {
		return
	}
	slog.Error(program+": second pooled connection requested while one is held",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// Pool is a pgx pool behind the guard. Every way it hands out a connection refuses one asked for
// while the caller already holds one (Guard). The pgx pool is private so no caller can reach past
// the refusal. Each store forwards the methods its own pool hands out, and only those.
type Pool struct {
	pool         *pgxpool.Pool
	program      string
	beforeCommit func(ctx context.Context) func(ctx context.Context, tx pgx.Tx) error
}

// Options is what a store tells the guard about its pool.
type Options struct {
	// Program names the process the pool serves at the head of each refusal's log line: "legion"
	// for the daemon, "dispatch" for Dispatch's server.
	Program string
	// BeforeCommit, when set, is read once as each transaction opens: the step that transaction
	// takes just before its COMMIT, inside the transaction, or nil for none. An error from the step
	// rolls the transaction back, and Commit returns that error. A transaction already failed when
	// its caller commits it skips the step: pgx answers that commit with its own rollback error, as
	// it would without one. Dispatch's keyed writes mark their Idempotency-Key committed this way
	// (its store's WithBeforeCommit); the daemon sets none.
	BeforeCommit func(ctx context.Context) func(ctx context.Context, tx pgx.Tx) error
}

// New wraps an open pgx pool in the guard.
func New(pool *pgxpool.Pool, options Options) *Pool {
	return &Pool{pool: pool, program: options.Program, beforeCommit: options.BeforeCommit}
}

// Query runs a query on a pooled connection. The rows hold that connection until they close, so the
// caller counts as holding one for as long as they are open: work done inside the loop over the
// rows - a write per row, a lookup per row - is exactly the shape that wedges a pool.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := Guard(ctx, p.program); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	release := Hold(ctx)
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
	if err := Guard(ctx, p.program); err != nil {
		return errRow{err: err}
	}
	return p.pool.QueryRow(ctx, sql, args...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := Guard(ctx, p.program); err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.pool.Exec(ctx, sql, args...)
}

// Acquire takes a pooled connection the caller holds until it releases it. It marks nothing: a
// caller that keeps the connection while it asks the pool for more marks its context with Hold for
// as long as it keeps it, so the guard sees that connection too.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if err := Guard(ctx, p.program); err != nil {
		return nil, err
	}
	return p.pool.Acquire(ctx)
}

// AcquireFunc runs fn with a pooled connection.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	if err := Guard(ctx, p.program); err != nil {
		return err
	}
	return p.pool.AcquireFunc(ctx, fn)
}

// AcquireAllIdle takes every idle connection the pool holds, or none for a caller the guard
// refuses.
func (p *Pool) AcquireAllIdle(ctx context.Context) []*pgxpool.Conn {
	if err := Guard(ctx, p.program); err != nil {
		return nil
	}
	return p.pool.AcquireAllIdle(ctx)
}

// CopyFrom streams rows into a table on a pooled connection.
func (p *Pool) CopyFrom(
	ctx context.Context,
	table pgx.Identifier,
	columns []string,
	source pgx.CopyFromSource,
) (int64, error) {
	if err := Guard(ctx, p.program); err != nil {
		return 0, err
	}
	return p.pool.CopyFrom(ctx, table, columns, source)
}

// SendBatch runs a batch on a pooled connection. A refusal is carried by the results, which have
// no error return of their own (errBatchResults).
func (p *Pool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if err := Guard(ctx, p.program); err != nil {
		return errBatchResults{err: err}
	}
	return p.pool.SendBatch(ctx, batch)
}

// Ping opens a connection and asks the server to answer on it.
func (p *Pool) Ping(ctx context.Context) error {
	if err := Guard(ctx, p.program); err != nil {
		return err
	}
	return p.pool.Ping(ctx)
}

// Begin opens a transaction and marks the caller as holding a connection. It is what
// pgx.BeginFunc opens its transaction through.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := Guard(ctx, p.program); err != nil {
		return nil, err
	}
	return p.begin(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the caller as holding a connection. It is
// what pgx.BeginTxFunc opens its transaction through.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := Guard(ctx, p.program); err != nil {
		return nil, err
	}
	return p.begin(ctx, options)
}

// begin is the body both transaction entry points share once their guard has passed. A
// transaction with nothing to do at either end - no hold mark to clear and no BeforeCommit step
// to take - is handed on as pgx opened it: a trackedTx around it would only forward its Commit and
// Rollback.
func (p *Pool) begin(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	release := Hold(ctx)
	var beforeCommit func(context.Context, pgx.Tx) error
	if p.beforeCommit != nil {
		beforeCommit = p.beforeCommit(ctx)
	}
	if release == nil && beforeCommit == nil {
		return tx, nil
	}
	return &trackedTx{Tx: tx, release: release, beforeCommit: beforeCommit}, nil
}

// trackedTx clears the caller's mark however the transaction ends, and takes the caller's
// BeforeCommit step before it commits. Callers commit and then run a deferred rollback; the
// release absorbs that second call (Hold). Either part may be absent: a context the guard does not
// track has no mark, and one with no step commits as it is.
type trackedTx struct {
	pgx.Tx
	release      func()
	beforeCommit func(ctx context.Context, tx pgx.Tx) error
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.end()
	if t.beforeCommit != nil && t.Tx.Conn().PgConn().TxStatus() == 'T' {
		if err := t.beforeCommit(ctx, t.Tx); err != nil {
			_ = t.Tx.Rollback(ctx)
			return err
		}
	}
	return t.Tx.Commit(ctx)
}

func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.end()
	return t.Tx.Rollback(ctx)
}

// end clears the caller's mark, when the guard tracks one.
func (t *trackedTx) end() {
	if t.release != nil {
		t.release()
	}
}

// errRow carries a refusal through the single-row surface, which has no error return of its own:
// the caller reads it where it would read a query's failure, out of Scan.
type errRow struct {
	err error
}

func (r errRow) Scan(...any) error { return r.err }

// errBatchResults carries a refusal through the batch surface, which has no error return of its
// own: every result the caller reads is the refusal.
type errBatchResults struct {
	err error
}

func (b errBatchResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatchResults) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatchResults) QueryRow() pgx.Row                { return errRow{err: b.err} }
func (b errBatchResults) Close() error                     { return b.err }
