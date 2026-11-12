package store

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
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
//
// This guard is the envoy pool's, ported, and two of its forms were left behind: a mark a caller
// sets around a connection it holds itself (envoy's HoldsConnection, for a connection taken with
// envoy's Acquire, which this pool does not hand out) and a per-read mark for reads of one caller
// that run side by side (envoy's ForConcurrentRead). No call site of this daemon does either, so
// neither form is here; a daemon that grows one ports back the form it needs rather than keeping
// an untested copy of both.
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

// holdsConnection answers whether ctx's caller holds a connection of this pool now. A context
// nothing marked holds none as far as the pool can tell, which is the guard's own limit
// (ErrNestedAcquire).
func holdsConnection(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side
// for as long as the caller lives: ctx's deadline, cancellation and values, with a hold mark of
// its own, always a fresh one. A worker that takes a connection — a transaction, a cursor — marks
// only itself, so its siblings are still free to take theirs, and the pool still refuses each
// worker a second connection of its own. Every goroutine of the daemon that runs beside others
// takes one: the outbox's lanes and each row's lease renewal, the workflow runtime's loops, the
// serving group's goroutines, the supervision sweeps and relaunches, each machine's feeding
// goroutine, timers and sends, each hello the worker stream resolves, and each write a
// supervision callback hands a goroutine of its own — a live terminal fact, a session fact, a
// task's confirmation, a provider-errors notice, a pod resume's stamp and a stall's notice.
//
// It differs from WithTransactionTracking, this package's other mark, by never reusing ctx's:
// that one hands back the mark ctx already carries, which siblings sharing it would set and
// clear under each other. A worker's mark is its own whatever the parent carries, so the caller
// must start these goroutines outside any transaction or connection of this pool's — a
// process-lifetime context, as the daemon's loops run off — since a fresh mark cannot see a
// connection the parent holds.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// loggedSites is the call sites that have already logged a refusal (refusalSite), so a site that
// trips the guard in a loop reports its stack once instead of flooding the log, while a refusal
// at any other site still logs a line of its own. The error itself is returned to every caller,
// every time. A plain map under a mutex keeps the key unboxed, and a site's file name is the
// runtime's own string, so remembering a site costs no allocation beyond the walk that finds it.
var loggedSites = struct {
	sync.Mutex
	seen map[callSite]struct{}
}{seen: map[callSite]struct{}{}}

// callSite is a call that asked for a connection, by the line of source it is written on. A line
// and not a program counter, because the compiler copies a small function into each of its
// callers: one call written once then stands at a program counter per copy, and keyed on those it
// would log once per copy rather than once.
type callSite struct {
	file string
	line int
}

// refusalDepth bounds how many frames above logRefusal a refusal reads while it looks for its
// call site. The frames passed over on the way (passedOver) are a handful — the guard, the
// guarded method, a Store method, pgx's BeginFunc, intake.ApplyFact — so the bound is never
// reached by a stack the daemon builds; one that did would be remembered under its last frame.
const refusalDepth = 32

// storeFunctions, applyFact and applied are how passedOver knows a frame of this package or of
// intake's two functions, by the name the runtime gives its function (runtime.Frame.Function: an
// import path, then the symbol). This package's path is read off its own type, and intake's
// beside it under internal/, so a rename of the module renames nothing here; intake imports this
// package, so its functions can only be named.
var (
	storeFunctions = reflect.TypeFor[Pool]().PkgPath() + "."
	applyFact      = path.Dir(reflect.TypeFor[Pool]().PkgPath()) + "/intake.ApplyFact"
	applied        = path.Dir(reflect.TypeFor[Pool]().PkgPath()) + "/intake.Applied"
)

// pgxFunctions is how every pgx function's name starts, the driver's own package and its
// subpackages alike, which is how passedOver knows a frame of pgx.BeginFunc or pgx.BeginTxFunc.
const pgxFunctions = "github.com/jackc/pgx/"

// logRefusal logs one refusal per call site, with the stack that asked for the second connection,
// which is what names the path to repair. The site is the caller's own call (refusalSite), not
// the frame just above the guarded method: that frame is pgx's for every transaction opened
// through pgx.BeginFunc, Store.Tx among them, and intake's for every fact, so keyed on it one
// refusal would silence every other caller's defect. The frames are read into a fixed array, and
// the stack is only formatted for a site's first refusal.
func logRefusal() {
	var callers [refusalDepth]uintptr
	n := runtime.Callers(2, callers[:])
	if n == 0 {
		return
	}
	site := refusalSite(callers[:n])
	loggedSites.Lock()
	_, seen := loggedSites.seen[site]
	if !seen {
		loggedSites.seen[site] = struct{}{}
	}
	loggedSites.Unlock()
	if seen {
		return
	}
	slog.Error("legion: second pooled connection requested while one is held",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// refusalSite is the call that asked for a refused connection: the first of callers' frames that
// passedOver does not pass over, or the last frame when it passes over every one. Two calls of
// one function on two lines are two sites, and a call made again in a loop is the same one.
func refusalSite(callers []uintptr) callSite {
	frames := runtime.CallersFrames(callers)
	for {
		frame, more := frames.Next()
		if !more || !passedOver(frame) {
			return callSite{file: frame.File, line: frame.Line}
		}
	}
}

// passedOver answers whether frame only carries a call on to the pool, so that a refusal is not
// remembered under it: this package's code (the guard, the guarded method, and every Store
// method that asks the pool on its caller's behalf, Store.Tx among them), pgx's (BeginFunc and
// BeginTxFunc, which open the transaction they wrap through Begin and BeginTx), and
// intake.ApplyFact and intake.Applied, which every fact and every replay asks through. Each of
// them stands above the pool for every caller it serves. This package's tests are callers like
// any other, so a frame of a test file is a site.
func passedOver(frame runtime.Frame) bool {
	switch {
	case frame.Function == applyFact, frame.Function == applied:
		return true
	case strings.HasPrefix(frame.Function, pgxFunctions):
		return true
	case strings.HasPrefix(frame.Function, storeFunctions):
		return !strings.HasSuffix(frame.File, "_test.go")
	}
	return false
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
// asked (logRefusal). Every exported method that hands out a connection runs it before it asks
// pgx for anything, so a refusal touches neither the pool nor the database.
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

// Begin opens a transaction and marks the caller as holding a connection. It is what
// pgx.BeginFunc opens its transaction through, and with it Store.Tx, so every call site that
// wraps a transaction in a function keeps working as it was written.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the caller as holding a connection. It is
// what pgx.BeginTxFunc opens its transaction through, and what intake.ApplyFact opens each
// fact's with.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if err := p.guard(ctx); err != nil {
		return nil, err
	}
	return p.begin(ctx, options)
}

// begin is the body both transaction entry points share once their guard has passed. A
// transaction on a context the pool cannot see has no mark to clear and is handed on as pgx
// opened it: a trackedTx around it would only forward its Commit and Rollback.
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
