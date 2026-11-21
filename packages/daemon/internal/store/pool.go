package store

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store/storetext"
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
//
// This guard is the envoy pool's, ported, and two of its forms were left behind: a mark a caller
// sets around a connection it holds itself (envoy's HoldsConnection, for a connection taken with
// envoy's Acquire, which this pool hands out only through acquire, and acquire sets that mark
// itself) and a per-read mark for reads of one caller that run side by side (envoy's
// ForConcurrentRead). No call site of this daemon does either, so neither form is here; a daemon
// that grows one ports back the form it needs rather than keeping an untested copy of both.
var ErrNestedAcquire = errors.New(
	"this caller already holds a connection of the pool: a second one would deadlock it",
)

// holding records whether the context's caller holds a connection of this pool: its transaction
// or an open cursor of its own. One writer sets it and one reader tests it, so a flag is the
// whole state — a transaction never opens a second transaction of its own.
type holding struct {
	connection atomic.Bool
}

// holdingKey is the context key the mark lives under. It holds a pointer, so a transaction body
// that runs on the context it was opened with is covered with no threading of its own.
type holdingKey struct{}

// WithTransactionTracking marks ctx so the pool can refuse its caller a second connection while
// one of its transactions or cursors is open, and hands back a context already marked as it is,
// so a caller inside a marked one does not get a mark that cannot see the connection the caller
// holds. It is the mark of a sequential flow, one call after another on one goroutine: every API
// route (api's trackTransactions, where the mux is built), the boot's steps, its readiness probes
// and its replay of the terminal facts no daemon applied. A goroutine that runs beside others
// takes ForConcurrentWorker instead.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(holdingKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, holdingKey{}, &holding{})
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

// heldRelease is hold's release for a caller that runs its release however it ends, so it is never
// nil: noRelease for a context the pool cannot see.
func heldRelease(ctx context.Context) func() {
	if release := hold(ctx); release != nil {
		return release
	}
	return noRelease
}

// noRelease is the release of a hold that marked nothing: one on a context the pool cannot see,
// and a savepoint's, which ends inside the transaction that holds the connection.
func noRelease() {}

// holdsConnection answers whether ctx's caller holds a connection of this pool now. A context
// nothing marked holds none as far as the pool can tell, which is the guard's own limit
// (ErrNestedAcquire).
func holdsConnection(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side
// for as long as the caller lives: ctx's deadline, cancellation and values, with a hold mark of
// its own, always a fresh one, and no commit scope. A worker that takes a connection — a
// transaction, a cursor — marks only itself, so its siblings are still free to take theirs, and
// the pool still refuses each worker a second connection of its own. A goroutine that runs beside
// others and reaches this pool takes one. A caller that takes none is one the guard cannot see,
// since a context nothing marked holds no connection as far as the pool can tell
// (holdsConnection): the sandbox launcher's frame loop is such a caller, deleting a restored
// workspace bundle on a context made from context.Background() (runtime/sandbox/launcher.go,
// restoreReported), and the pool refuses it nothing.
//
// It differs from WithTransactionTracking, this package's other mark, by never reusing ctx's:
// that one hands back the mark ctx already carries, which siblings sharing it would set and
// clear under each other. A worker's mark is its own whatever the parent carries, so the caller
// must start these goroutines outside any transaction or connection of this pool's — a
// process-lifetime context, as the daemon's loops run off — since a fresh mark cannot see a
// connection the parent holds. For the same reason it holds no commit scope: a goroutine started
// inside a TransactWith body would otherwise join that body's transaction, run statements on its
// connection beside the body, and append to its hook list from a second goroutine.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, holdingKey{}, &holding{}), commitHooksKey{}, (*commitScope)(nil))
}

// loggedSites is the calling stacks that have already logged a refusal: one line per distinct
// stack that reaches the guard. A site reached by N different paths logs at most N lines, one per
// path, and a call repeated in a loop has the same stack every time, so it logs once instead of
// flooding the log. The error itself is returned to every caller, every time. The key is the
// stack's program counters themselves (refusalDepth), a comparable array, so a stack already seen
// is found with no allocation and no walk of its frames. A function the compiler copies into each
// of its callers stands at a different counter in each copy, but each copy is on a path of its
// own, so it adds no line that path would not log anyway.
var loggedSites = struct {
	sync.Mutex
	seen map[[refusalDepth]uintptr]struct{}
}{seen: map[[refusalDepth]uintptr]struct{}{}}

// refusalDepth is how many frames of a refused call's stack, from the guard outwards, make up its
// key. The frames nearest the guard — the guarded method, pgx's wrapper, a forwarder the compiler
// keeps whole — are the same program counters on every path through them; the frames beyond them
// are the ones that tell the paths into the pool apart, so two stacks that differ only beyond this
// depth share one line.
const refusalDepth = 32

// logRefusal logs a refusal once per distinct calling stack, with that stack, which is what names
// the path to repair. The stack, read into a fixed array, is itself the key (loggedSites): a
// function that only passes a call on to the pool — TransactWith, a Store method, intake's
// ApplyFact or any function of the daemon's own — stands in the stacks of all its callers without
// making them one, so no caller's refusal silences another's. The stack is formatted only on a
// stack's first refusal; a refusal of a stack already seen costs the read of its program counters
// and a map lookup.
func logRefusal() {
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
// pool of its own wraps it here, so that it exercises what this type adds to any pool it wraps:
// the guard (ErrNestedAcquire) and the argument rule (storableArgs). The codec rule, which is what
// makes a string storable, belongs to the pgx pool instead: it runs only on a pool built from
// PoolConfig (testpg.ClonePool's is), and a pool built without it sends a NUL, an invalid byte or a
// \u0000 escape to the server as pgx's own codecs write it, and the server refuses the write.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{pool: pool}
}

// storableArgs is the argument rule: args, with each string argument that the store's text rule
// changes (storetext.Changes) given as a *string to the same text, in a copy of args. pgx sends a
// plain string in text format as it is, without asking the codec of its parameter's type, and a
// *string reaches that codec (the codec rule, registerStorableCodecs), which knows whether the
// parameter is text or jsonb and applies that type's rule. Every other argument stays as it is,
// and so does a leading pgx option such as pgx.QueryExecModeSimpleProtocol. A call whose strings
// the rule keeps gets args itself back, with no allocation.
func storableArgs(args []any) []any {
	var storable []any
	for i, arg := range args {
		s, ok := arg.(string)
		if !ok || !storetext.Changes(s) {
			continue
		}
		if storable == nil {
			storable = slices.Clone(args)
		}
		storable[i] = new(s)
	}
	if storable == nil {
		return args
	}
	return storable
}

// guard refuses a connection to a caller that already holds one, and logs the stack that asked
// (logRefusal). Every exported method that hands out a connection runs it before it asks pgx for
// anything, so a refusal touches neither the pool nor the database.
func (p *Pool) guard(ctx context.Context) error {
	if !holdsConnection(ctx) {
		return nil
	}
	logRefusal()
	return ErrNestedAcquire
}

// Query runs a query on a pooled connection. The rows hold that connection until they close, so
// the caller counts as holding one for as long as they are open: a cursor is one of this pool's
// two connection-holders — a transaction and an open cursor — and work done inside the loop over
// the rows is exactly the shape that wedged the deployment.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, sql, storableArgs(args)...)
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
	return p.pool.QueryRow(ctx, sql, storableArgs(args)...)
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := p.guard(ctx); err != nil {
		return pgconn.CommandTag{}, err
	}
	return p.pool.Exec(ctx, sql, storableArgs(args)...)
}

// acquire hands out one of the pool's connections to a caller that must run its own work on the
// connection after its transaction ends and before the connection returns to the pool. Store.apply
// is the one such caller: it resets the session of the connection that a migration ran on
// (releaseMigrationConn). A transaction from BeginTx cannot serve it, because pgx returns that
// transaction's connection to the pool as the transaction ends. acquire runs the guard first, as
// every other way this pool hands out a connection does. Then it marks the caller as holding a
// connection, as begin does. The caller runs release only once the connection is back in the pool
// or closed, so the guard sees the connection for as long as the caller holds it. release is never
// nil: for a context the pool cannot see, it does nothing.
func (p *Pool) acquire(ctx context.Context) (conn *pgxpool.Conn, release func(), err error) {
	if err = p.guard(ctx); err != nil {
		return nil, nil, err
	}
	conn, err = p.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, heldRelease(ctx), nil
}

// BeginTx opens a transaction with options and marks the caller as holding a connection. It is
// what TransactWith opens every transaction of the daemon through.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, options)
}

// begin is BeginTx's body once its guard has passed. The transaction it gives is always the
// store's own (storeTx), so every statement of it passes the argument rule. It carries the release
// of the caller's hold mark, which does nothing for a context the pool cannot see, as acquire's
// does.
func (p *Pool) begin(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.pool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &storeTx{Tx: tx, release: heldRelease(ctx)}, nil
}

// storeTx is every transaction the pool hands out, and every savepoint opened inside one. Its
// Exec, Query and QueryRow, and each query a SendBatch sends, pass their arguments through the
// argument rule (storableArgs). A savepoint its Begin opens is a storeTx too. CopyFrom needs no
// rule of its own: COPY sends every value in binary format, through the codec rule
// (registerStorableCodecs). Prepare takes no arguments. Conn gives pgx's own connection: a
// statement run on it reaches the codecs but not the argument rule, and no code of the daemon
// calls it.
//
// release clears the caller's hold mark however the transaction ends. It is never nil: it does
// nothing for a transaction on a context the pool cannot see and for a savepoint, which ends
// inside the transaction that holds the connection. Callers commit and then run a deferred
// rollback; the release absorbs that second call (hold).
type storeTx struct {
	pgx.Tx
	release func()
}

func (t *storeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	savepoint, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &storeTx{Tx: savepoint, release: noRelease}, nil
}

func (t *storeTx) Commit(ctx context.Context) error {
	defer t.release()
	return t.Tx.Commit(ctx)
}

func (t *storeTx) Rollback(ctx context.Context) error {
	defer t.release()
	return t.Tx.Rollback(ctx)
}

func (t *storeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(ctx, sql, storableArgs(args)...)
}

func (t *storeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.Tx.Query(ctx, sql, storableArgs(args)...)
}

func (t *storeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.Tx.QueryRow(ctx, sql, storableArgs(args)...)
}

func (t *storeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	for _, query := range b.QueuedQueries {
		query.Arguments = storableArgs(query.Arguments)
	}
	return t.Tx.SendBatch(ctx, b)
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
