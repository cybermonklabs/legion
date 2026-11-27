package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// ErrNestedAcquire is a pool this package guards (Pool: the request pool and the pools it wraps)
// refusing a connection to a caller that already holds a connection of one of them.
//
// One caller, one connection, is the rule the pool cannot survive without. A transaction
// holds its connection until it commits; a caller that asks for a second one while it holds a
// row or advisory lock waits for a connection only the callers queued behind that lock can
// release, and they are waiting for the lock. Two anchored writes and two document
// settlements on one issue closed that cycle on a four-connection pool, and nothing but
// pg_terminate_backend recovered it. Rationing connections cannot fix it, and neither can
// SharedPoolSize's size: the queue behind one writer's issue lock is unbounded - a
// settlement per document, every issue-owned event append, the architecture importer - so a
// bigger pool only moves the number of callers it takes.
//
// So work that genuinely needs a connection of its own while a transaction is open takes it
// from a pool of its own - the rooms pool (Pool.Rooms) for a document load, the health pool
// (Pool.Healthy) for the load balancer's probe - not from here. Everything else reads through
// the transaction it is already inside.
//
// This error makes the rule enforce itself. Every entry point that opens one of this pool's
// transactions marks its context (api.Register marks every route; settlement, the architecture
// importer, the outbox publisher, the CLI backfills, the startup seeds and the migrations mark
// their own goroutines), and an acquisition that arrives under a held connection fails here,
// loudly, instead of wedging production.
var ErrNestedAcquire = errors.New(
	"this caller already holds a connection of the pool: a second one would deadlock it",
)

// holding records whether the context's caller holds a connection of this pool: its
// transaction, its open cursor, or one it acquired directly. One writer sets it and one reader
// tests it, so a flag is the whole state - a transaction never opens a second transaction of its
// own. A pinned request (PinConnection) also carries its pinned connection here, so a context
// made with a mark of its own (ForConcurrentRead) pins nothing. It also carries the request's
// refusal (RefuseRequest), which a mark ForConcurrentRead made reads from the request's own.
type holding struct {
	connection atomic.Bool
	pin        *pinnedConnection
	// request is the mark of the request a ForConcurrentRead mark was made from, nil on the
	// request's own mark.
	request *holding
	// refusal is the request's refusal (RefuseRequest), nil while it may run its statements. It is
	// read on the request's own mark (owner).
	refusal atomic.Pointer[error]
}

// owner is the request's own mark: h, or the mark a ForConcurrentRead mark was made from.
func (h *holding) owner() *holding {
	if h.request != nil {
		return h.request
	}
	return h
}

// refused is the request's refusal (RefuseRequest), nil while it may run its statements.
func (h *holding) refused() error {
	if refusal := h.owner().refusal.Load(); refusal != nil {
		return *refusal
	}
	return nil
}

// RefuseRequest refuses every later statement of ctx's request with refusal: each call it makes
// on any pool (refuseOrPin), a read ForConcurrentRead split off it included, and the commit of a
// transaction it opened (CommitRefusal). admission.Away.TakeBack puts it on a request refused the
// admission units it gave back for a wait on the network. The API answers such a request itself,
// 429 OVERLOADED, at its entry point before any layer can go on with it (api's admitted); this is
// the database's own guard beside that answer, so no statement or commit of the request runs
// whatever reaches the store: it fails with the refusal, which the API answers as admission's own
// (admission.WriteUnserved). It does nothing for a context the pool cannot see
// (WithTransactionTracking), and the first refusal stands.
func RefuseRequest(ctx context.Context, refusal error) {
	if state, ok := ctx.Value(holdingKey{}).(*holding); ok {
		state.owner().refusal.CompareAndSwap(nil, &refusal)
	}
}

type holdingKey struct{}

// WithTransactionTracking marks ctx so the pool can refuse a second connection while one of its
// transactions is open. API requests are marked by middleware; every background entry point
// that opens a transaction - settlement, the architecture importer, the outbox publisher, the
// CLI backfills, startup seeding and the migrations - marks its own context.
func WithTransactionTracking(ctx context.Context) context.Context {
	if ctx.Value(holdingKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// HoldsConnection marks ctx as already holding a pooled connection until the returned release
// runs, so an acquisition under it is refused like one inside a transaction. The durable
// document appends hold their connection directly, outside any transaction of this pool's.
func HoldsConnection(ctx context.Context) (context.Context, func()) {
	ctx = WithTransactionTracking(ctx)
	return ctx, hold(ctx)
}

// hold records that ctx's caller now holds a connection of this pool and returns the release
// that clears it, or nil for a context the pool cannot see, which has no mark to clear. The
// release runs at most once: a holder that releases twice - a transaction that commits and
// then runs its deferred rollback, a cursor drained and then closed - must not clear the mark
// of whatever holds a connection by then.
func hold(ctx context.Context) func() {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	if !ok {
		return nil
	}
	state.connection.Store(true)
	return sync.OnceFunc(func() { state.connection.Store(false) })
}

// ConnectionHeld reports whether ctx's caller holds a pooled connection: its transaction, a
// cursor, a connection it acquired or marked (HoldsConnection), or its request's pinned connection
// (PinConnection), idle between statements or not. Whatever a connection holder must never wait
// for refuses the callers it reports - a document's writer gate, whose holder may be waiting for a
// connection itself. The pools refuse a second acquire to every one of them (ErrNestedAcquire);
// only the pool that pinned the connection serves a call under a pin, and it runs that call on
// the pinned connection instead of taking a second.
func ConnectionHeld(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && (state.connection.Load() || state.pin.held())
}

// HoldsPooledConnection reports whether ctx's caller holds a connection a second acquire would
// be refused beside: its transaction, an open cursor, or a connection it acquired or marked
// (HoldsConnection). Work that may be skipped asks before it takes one of its own - storing and
// reading a display tree beside another caller's work (texttrees) is the one such caller -
// because a second connection taken while one is held is the deadlock ErrNestedAcquire refuses,
// and for optional work a quiet skip is better than that refusal, whose stack names a call site
// that is working as intended.
//
// A request's pinned connection (PinConnection) is deliberately not one of them, which is what
// makes this a different question from ConnectionHeld's. The request pool serves a call under a
// pin on the connection the request already holds rather than taking a second, so a pinned read
// that asks the table for its trees takes no connection beside its pin and cannot deadlock on
// one: were a pin reported here, every pinned read - which is every ordinary GET (routeHandler) -
// would skip the table and read each of its texts for display again, and the stored trees would
// serve only the routes that are not pinned. ConnectionHeld answers the other question, whether
// the caller may wait (a document's writer gate), where a pin counts because the request holds
// it while it waits. TestAPinnedRequestDoesNotHoldAPooledConnection holds this apart.
func HoldsPooledConnection(ctx context.Context) bool {
	state, ok := ctx.Value(holdingKey{}).(*holding)
	return ok && state.connection.Load()
}

// ForConcurrentRead returns a context for one of a caller's reads that run side by side: ctx's
// deadline, cancellation and values, with a hold mark of its own and no pinned connection, so that
// read takes one connection and the pool still refuses it a second. Reads sharing one mark would
// refuse each other, since one read's open cursor marks the context they share, and so would reads
// sharing one pinned connection, which runs one call at a time. A caller already holding a
// connection (its transaction, a cursor, an acquired connection, its pinned connection) is refused
// with ErrNestedAcquire, because each of its reads would be a second connection taken while it
// holds one.
func ForConcurrentRead(ctx context.Context) (context.Context, error) {
	if ConnectionHeld(ctx) {
		logRefusal()
		return nil, ErrNestedAcquire
	}
	split := &holding{}
	if state, ok := ctx.Value(holdingKey{}).(*holding); ok {
		split.request = state.owner()
	}
	return context.WithValue(ctx, holdingKey{}, split), nil
}

// ReadSideBySide runs reads at once and answers the first error. Each runs on a hold mark of its
// own (ForConcurrentRead), so it takes at most one connection of the pool and one read's open
// cursor refuses no other; a read holds no lock past its own statements and waits on no other
// read, so none waits on another: a read opens no transaction, or reads in one that only reads,
// which its caller opened before the group on a hold mark of its own and the read commits once
// its statements end (legion/metrics' read opens it, and readSampled reads in it and commits it).
// A caller that holds a connection is refused (ErrNestedAcquire) before any read starts.
//
// Where ctx carries a read limit (WithReadLimit), at most that many of the reads run at once across
// every group the call tree opens: a read waits for a place before it starts, and a read that
// itself opens a group lets go of its place while it waits for that group's reads, so a group
// nested in a group takes the places its parent left, and no read waits for a place a read that is
// waiting for it holds.
func ReadSideBySide(ctx context.Context, reads ...func(ctx context.Context) error) error {
	limit := readLimitOf(ctx)
	var waiting *readPlace
	if limit != nil {
		// The caller may be a read of an outer group, holding one of the places.
		if waiting, _ = ctx.Value(readPlaceKey{}).(*readPlace); waiting != nil && !waiting.let() {
			waiting = nil
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for _, read := range reads {
		readCtx, err := ForConcurrentRead(groupCtx)
		if err != nil {
			_ = group.Wait()
			_ = waiting.take(ctx)
			return err
		}
		if limit == nil {
			group.Go(func() error { return read(readCtx) })
			continue
		}
		place := &readPlace{limit: limit}
		readCtx = context.WithValue(readCtx, readPlaceKey{}, place)
		group.Go(func() error {
			if err := place.take(readCtx); err != nil {
				return err
			}
			defer place.let()
			return read(readCtx)
		})
	}
	err := group.Wait()
	if takeErr := waiting.take(ctx); err == nil {
		err = takeErr
	}
	return err
}

type readLimitKey struct{}

type readPlaceKey struct{}

// readLimit is the places of a request's side-by-side reads (WithReadLimit).
type readLimit chan struct{}

// WithReadLimit returns ctx with a limit of n on the reads ReadSideBySide runs at once under it,
// which is how many request-pool connections one request's reads hold at once: the weight
// admission counts its route at. A context that carries a limit already keeps it, so a read that
// is itself a limited group's is not given places of its own, and n below one is one.
func WithReadLimit(ctx context.Context, n int) context.Context {
	if readLimitOf(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, readLimitKey{}, readLimit(make(chan struct{}, max(n, 1))))
}

func readLimitOf(ctx context.Context) readLimit {
	limit, _ := ctx.Value(readLimitKey{}).(readLimit)
	return limit
}

// readPlace is one read's hold on a place of its limit: held while the read runs, and let go of
// while it waits for the reads of a group it opened (ReadSideBySide).
type readPlace struct {
	limit readLimit
	held  atomic.Bool
}

// take waits for a place of p's limit and answers ctx's cause if its end came first. A place taken
// is marked held; a nil p, a caller that held none, takes none.
func (p *readPlace) take(ctx context.Context) error {
	if p == nil {
		return nil
	}
	select {
	case p.limit <- struct{}{}:
		p.held.Store(true)
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// let gives p's place back and answers whether it held one.
func (p *readPlace) let() bool {
	if p == nil || !p.held.CompareAndSwap(true, false) {
		return false
	}
	<-p.limit
	return true
}

// ForConcurrentWorker returns a context for one of a caller's goroutines that run side by side
// for as long as the caller lives: ctx's deadline, cancellation and values, with a hold mark of
// its own, always a fresh one. A worker that takes a connection - a transaction, a cursor - marks
// only itself, so its siblings are still free to take theirs, and the pool still refuses each
// worker a second connection of its own.
//
// It differs from the other two marks by never reusing ctx's: WithTransactionTracking hands back
// the mark ctx already carries, which siblings sharing it would set and clear under each other,
// and ForConcurrentRead refuses a caller that holds a connection. A worker's mark is its own
// whatever the parent carries, so the caller must start these goroutines outside any transaction
// or connection of this pool's: a process-lifetime context, as room sync's lanes run off
// (docs.Service.RegisterRoomSync), or a request's once its write has committed, as a broadcast's
// detached delivery runs on (api.server.deliverBroadcast). A fresh mark cannot see a connection
// the parent holds.
func ForConcurrentWorker(ctx context.Context) context.Context {
	return context.WithValue(ctx, holdingKey{}, &holding{})
}

// loggedSites remembers the call sites that have already logged a refusal, so a caller that
// trips the guard in a loop reports its stack once instead of flooding the log. The error
// itself is returned to every caller, every time.
var loggedSites sync.Map

func logRefusal() {
	var caller [1]uintptr
	// Skip runtime.Callers, logRefusal, guard, and the pool method that called it.
	if runtime.Callers(4, caller[:]) == 0 {
		return
	}
	if _, seen := loggedSites.LoadOrStore(caller[0], struct{}{}); seen {
		return
	}
	slog.Error("dispatch: second pooled connection requested while one is held",
		"error", ErrNestedAcquire, "stack", string(debug.Stack()))
}

// Pool is a Dispatch connection pool: the shared request pool store.Open opens, or one of the
// pools of its own that wraps (Outbox, Background, Live, Settlements). Every way it hands out a
// connection refuses one asked for while the caller already holds one; see ErrNestedAcquire. The
// pgx pool is private so no caller can reach past the refusal.
type Pool struct {
	pool *pgxpool.Pool
	// name is the pool's name (RequestsPool for the shared pool), and acquire the tracer that
	// bounds and counts its acquires, nil for a pool opened without one (configurePool).
	name    string
	acquire *acquireTracer
	// mu guards each separate pool's creation against this pool's close: one lock, so a
	// caller that loses the race is told the pool is closed rather than opening one nothing
	// will ever close.
	mu        sync.Mutex
	separates map[string]openedPool
	// leaseCallers is the registry Leases hands out, holding the reservations of the lease
	// pool: one registry per pool, so every caller's reservation is counted together.
	leaseCallers *LeaseCallers
	// closed refuses every separate pool but the health probe's from the moment Close begins
	// (open); healthClosed refuses that one too, once Close has closed every other pool
	// (healthPool).
	closed       bool
	healthClosed bool
	// acquireBounds replaces defaultAcquireBound for the separate pools it names, read as each
	// one opens: a test shortens a bound with it, so a full pool refuses in milliseconds.
	acquireBounds map[string]time.Duration
	// openFailures is the error each separate pool it names fails to open with, in place of
	// opening (SetOpenFailureForTest): a test sees what its caller does when a pool cannot open.
	openFailures map[string]error
	// lockTimeoutDropped names the separate pools that open without the lock_timeout startup
	// parameter (DropLockTimeoutParameterForTest): a test sees what a pool's work does behind a
	// pooler that drops startup parameters.
	lockTimeoutDropped map[string]bool
	// wrapper marks a separate pool wrapped for its callers (Pool.wrapped), which opens no pool of
	// its own (ErrNotRootPool).
	wrapper bool
}

// NewPool wraps an open pgx pool. Its name and acquire tracer are the ones configurePool set on
// the pool's configuration; a pool opened without them is the request pool, unbounded.
func NewPool(pool *pgxpool.Pool) *Pool {
	acquire := acquireTracerOf(pool.Config().ConnConfig.Tracer)
	name := RequestsPool
	if acquire != nil {
		name = acquire.name
	}
	return &Pool{pool: pool, name: name, acquire: acquire}
}

// Config returns a copy of the pool's configuration.
func (p *Pool) Config() *pgxpool.Config { return p.pool.Config() }

// Stat reports the pool's current connection counts.
func (p *Pool) Stat() *pgxpool.Stat { return p.pool.Stat() }

// busy maps a call of this pool's that a bound of the server's ended to ErrPoolBusy, or to
// ErrStatementOutcomeUnknown where the call is a statement that commits itself (poolBusy):
// commits is what this call can have left behind at the database.
func (p *Pool) busy(ctx context.Context, err error, commits outcome) error {
	return poolBusy(ctx, p.name, err, commits)
}

// refuseOrPin is the refusal every way this pool hands out a connection makes first, and the
// pinned connection to run the call on where ctx pins one on this pool (nil where it pins none).
// A caller already holding a connection of these pools is refused with ErrNestedAcquire: its
// transaction, its open cursor or one it acquired (holding.connection), and its request's pinned
// connection on every pool but the one that pinned it. A pinned read holds its request-pool
// connection between its statements, so a background, live or settlements connection handed to it
// would be two of these pools' connections held by one caller, which is the cycle the guard
// exists to prevent; the pool that pinned it serves it on that very connection instead
// (pinnedConnection.take). Its callers log the refusal themselves (logRefusal), which keys it to
// the call site that asked for the connection by how deep the stack is where it is logged.
func (p *Pool) refuseOrPin(ctx context.Context) (*pinnedConnection, error) {
	state, _ := ctx.Value(holdingKey{}).(*holding)
	if state == nil {
		return nil, nil
	}
	if refusal := state.refused(); refusal != nil {
		return nil, refusal
	}
	if state.connection.Load() || (p.name != RequestsPool && state.pin.held()) {
		return nil, ErrNestedAcquire
	}
	return p.pinned(state), nil
}

// guard is the refusal alone (refuseOrPin), for a method that hands out no connection to run a
// call on.
func (p *Pool) guard(ctx context.Context) error {
	_, err := p.refuseOrPin(ctx)
	if errors.Is(err, ErrNestedAcquire) {
		logRefusal()
	}
	return err
}

// connection is what one call runs its statement on: the pgx pool, which takes a connection for
// the call, or the request's pinned connection (PinConnection). pgxpool's Pool and Conn share
// these methods.
type connection interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error)
	BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error)
}

// target is what one call runs on - ctx's pinned connection where it pins one on this pool, else
// the pgx pool - and done, which gives the pinned connection back once the call is over, nil for
// a call on the pgx pool. It refuses a caller already holding a connection (refuseOrPin), and a
// call while another runs on the pinned connection, with ErrNestedAcquire. Every exported method
// that hands out a connection calls it directly, as guard is called, so a refusal records that
// method's caller.
func (p *Pool) target(ctx context.Context) (connection, func(), error) {
	pin, err := p.refuseOrPin(ctx)
	if err != nil {
		if errors.Is(err, ErrNestedAcquire) {
			logRefusal()
		}
		return nil, nil, err
	}
	if pin == nil {
		return p.pool, nil, nil
	}
	conn, done, err := pin.take(ctx, p)
	if err != nil {
		if errors.Is(err, ErrNestedAcquire) {
			logRefusal()
		}
		return nil, nil, err
	}
	if conn == nil {
		return p.pool, nil, nil
	}
	return conn, done, nil
}

// finished gives a call's pinned connection back (target); a call on the pgx pool has nothing to.
func finished(done func()) {
	if done != nil {
		done()
	}
}

// joined is release, then done where the call ran on a pinned connection.
func joined(release, done func()) func() {
	if done == nil {
		return release
	}
	return func() {
		release()
		done()
	}
}

// Query runs a query on a pooled connection. The rows hold that connection until they close,
// so the caller counts as holding one for as long as they are open. A cursor is one of this
// pool's three connection-holders - a transaction, an open cursor, and a connection taken
// with Acquire (the durable append's withRoomLock, marked with HoldsConnection) - and work
// done inside the loop - a publish, a follower lookup, a write per row - is exactly the shape
// that wedges the pool. On a pinned connection an open cursor is a call still running on it, so a
// statement inside the loop is refused there too.
//
// Every method below that takes a connection returns ErrPoolBusy for an acquire that ran out the
// pool's bound (acquireTracer), however it reports its errors, and runs on the request's pinned
// connection where its context pins one (PinConnection).
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := on.Query(ctx, sql, args...)
	if err != nil {
		finished(done)
		return nil, p.busy(ctx, err, commitsItself)
	}
	// A context without a mark pins nothing, so done is nil whenever release is.
	release := hold(ctx)
	if release == nil {
		return rows, nil
	}
	return &trackedRows{Rows: rows, release: joined(release, done)}, nil
}

// trackedRows clears the caller's mark when pgx hands the cursor's connection back: an
// explicit Close, a Next that runs out of rows, or a Scan or Values error, which pgx treats as
// fatal and closes the rows on.
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

// QueryRow runs a single-row query on a pooled connection. pgxpool defers an acquire's error to
// the row's Scan, so the row maps it there.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	on, done, err := p.target(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return busyRow{Row: on.QueryRow(ctx, sql, args...), ends: ctx.Done(), pool: p.name, done: done}
}

// Exec runs a statement on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := on.Exec(ctx, sql, args...)
	finished(done)
	return tag, p.busy(ctx, err, commitsItself)
}

// Acquire takes a pooled connection the caller holds until it releases it. Under a pin it hands
// over the request's pinned connection, if it holds one, rather than taking a second; the request's
// next call takes another.
func (p *Pool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := p.handOver(ctx)
	if conn != nil || err != nil {
		return conn, err
	}
	conn, err = p.pool.Acquire(ctx)
	return conn, p.busy(ctx, err, commitsNothing)
}

// handOver is Acquire's refusal and its pinned connection: ErrNestedAcquire for a caller holding a
// connection (refuseOrPin) or a pinned connection a call runs on, the pinned connection where ctx
// pins an idle one on this pool, and nil otherwise.
func (p *Pool) handOver(ctx context.Context) (*pgxpool.Conn, error) {
	pin, err := p.refuseOrPin(ctx)
	if err != nil {
		if errors.Is(err, ErrNestedAcquire) {
			logRefusal()
		}
		return nil, err
	}
	if pin == nil {
		return nil, nil
	}
	conn, err := pin.handOver(p)
	if err != nil {
		logRefusal()
	}
	return conn, err
}

// AcquireFunc runs fn with a pooled connection - the pinned one under a pin. Only the acquire's
// error is mapped: fn's own is returned as fn returned it.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	on, done, err := p.target(ctx)
	if err != nil {
		return err
	}
	if pinned, ok := on.(*pgxpool.Conn); ok {
		defer done()
		return fn(pinned)
	}
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return p.busy(ctx, err, commitsNothing)
	}
	defer conn.Release()
	return fn(conn)
}

// AcquireAllIdle takes every idle connection the pool holds.
func (p *Pool) AcquireAllIdle(ctx context.Context) []*pgxpool.Conn {
	if err := p.guard(ctx); err != nil {
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
	on, done, err := p.target(ctx)
	if err != nil {
		return 0, err
	}
	copied, err := on.CopyFrom(ctx, table, columns, source)
	finished(done)
	return copied, p.busy(ctx, err, commitsItself)
}

// SendBatch runs a batch on a pooled connection. pgxpool defers an acquire's error to the
// results, so the results map it there.
func (p *Pool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	on, done, err := p.target(ctx)
	if err != nil {
		return errBatchResults{err: err}
	}
	return busyBatchResults{BatchResults: on.SendBatch(ctx, batch), ends: ctx.Done(), pool: p.name, done: done}
}

// QueueExec queues sql with args into b; its error, wrapped in what as a statement sent alone
// would wrap it, is what b's Close returns when the statement is the first of b's to fail. One
// helper for every caller that pipelines statements, so a batch's statements report their errors
// alike.
func QueueExec(b *pgx.Batch, what, sql string, args ...any) {
	b.Queue(sql, args...).Fn = func(results pgx.BatchResults) error {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
}

// Begin opens a transaction and marks the request as holding one.
func (p *Pool) Begin(ctx context.Context) (pgx.Tx, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	return p.begin(ctx, on, done, pgx.TxOptions{})
}

// BeginTx opens a transaction with options and marks the request as holding one.
func (p *Pool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	on, done, err := p.target(ctx)
	if err != nil {
		return nil, err
	}
	return p.begin(ctx, on, done, options)
}

// begin is the body both transaction entry points share, on what target gave them: target runs in
// the exported method, as it does in every exported method that hands out one of this pool's
// connections, so a refusal records the caller that asked for the transaction rather than one of
// this pool's own frames. A transaction on a pinned connection gives it back when it ends. A
// request-pool transaction opens with its own lock bound (requestBegin), keeping options'
// isolation level, access mode and deferrable mode.
func (p *Pool) begin(ctx context.Context, on connection, done func(), options pgx.TxOptions) (pgx.Tx, error) {
	if p.name == RequestsPool {
		options.BeginQuery = requestBegin(ctx, options)
	}
	tx, err := on.BeginTx(ctx, options)
	if err != nil {
		finished(done)
		return nil, p.busy(ctx, err, commitsNothing)
	}
	release := hold(ctx)
	if release == nil {
		release = func() {}
	}
	beforeCommit, _ := ctx.Value(beforeCommitKey{}).(BeforeCommit)
	return &trackedTx{
		Tx:           tx,
		release:      joined(release, done),
		beforeCommit: beforeCommit,
		readOnly:     options.AccessMode == pgx.ReadOnly,
	}, nil
}

// requestBegin is the statement a request-pool transaction opens with (Pool.begin): the BEGIN pgx
// would send for options, then SET LOCAL lock_timeout at the write lock bound (lockWait), or at the
// bound a test put on ctx (lockWaitSetting), in the one simple-protocol message pgx sends an
// argument-less statement as (pgx.Conn.BeginTx runs it with Exec), so the transaction holds the
// bound at no round trip more than its BEGIN. The connection carries the same bound
// (configurePool), but a session setting holds only where the connection is the session's: behind
// a pooler that hands each transaction whichever server connection is free, only the
// transaction's own setting is sure to be the one its statements wait under. It is local to the
// transaction and ends with it, and a bounded wait inside it keeps it and gives it back
// (boundedWaitKeep, boundedWaitRestore). The production bound's statements are built once
// (requestBegins), so a request's begin builds no string.
func requestBegin(ctx context.Context, options pgx.TxOptions) string {
	wait := lockWaitSetting(ctx)
	if begin, ok := requestBegins[options]; ok && wait == lockWaitDefault {
		return begin
	}
	return renderBegin(options, wait)
}

// requestBegins is requestBegin's statement at the production bound for every combination of
// pgx's standard transaction options.
var requestBegins = func() map[pgx.TxOptions]string {
	begins := map[pgx.TxOptions]string{}
	for _, isoLevel := range []pgx.TxIsoLevel{"", pgx.Serializable, pgx.RepeatableRead, pgx.ReadCommitted, pgx.ReadUncommitted} {
		for _, accessMode := range []pgx.TxAccessMode{"", pgx.ReadWrite, pgx.ReadOnly} {
			for _, deferrableMode := range []pgx.TxDeferrableMode{"", pgx.Deferrable, pgx.NotDeferrable} {
				options := pgx.TxOptions{IsoLevel: isoLevel, AccessMode: accessMode, DeferrableMode: deferrableMode}
				begins[options] = renderBegin(options, lockWaitDefault)
			}
		}
	}
	return begins
}()

// renderBegin is the BEGIN of a transaction with options that sets its own lock_timeout at wait:
// the caller's own BEGIN where options names one, else the one pgx renders from options (begin,
// then the isolation level, the access mode and the deferrable mode options sets, in that order),
// then SET LOCAL lock_timeout. It renders requestBegin's statement, and the BEGIN of a transaction
// that waits for its locks without a bound (waitingForLocksBegin, readOnlyWaitingForLocksBegin).
// wait is lockTimeoutSetting's whole milliseconds and their unit, or lockTimeoutNone, so it is
// written into the text as a literal: the simple protocol takes no parameters.
func renderBegin(options pgx.TxOptions, wait string) string {
	begin := options.BeginQuery
	if begin == "" {
		begin = "begin"
		if options.IsoLevel != "" {
			begin += " isolation level " + string(options.IsoLevel)
		}
		if options.AccessMode != "" {
			begin += " " + string(options.AccessMode)
		}
		if options.DeferrableMode != "" {
			begin += " " + string(options.DeferrableMode)
		}
	}
	return begin + "; set local " + lockTimeoutParam + " = '" + wait + "'"
}

// RequestBeginStatement is the statement the request pool opens a transaction with options with,
// at production's bound (requestBegin): a test that stalls a request's BEGIN on the wire matches
// that message (storetest.SimpleQuery).
func RequestBeginStatement(options pgx.TxOptions) string {
	return requestBegin(context.Background(), options)
}

// WaitForLocksUntilDeadline opts tx out of the request pool's write lock bound (requestBegin): it
// sets tx's lock_timeout, for tx alone, to the time ctx's deadline leaves, so a lock tx waits for
// is refused (lock_not_available) at that deadline rather than at lockWait. It is for the one
// request-pool transaction that waits for a lock longer than a commit by design, and under a
// deadline of its own: the boot's dashboard publish and prune (dashboard.Publish, dashboard.Prune),
// which wait for another process's publish of the same build or asset across that publish's whole
// upload, a statement per file, within the boot's publish deadline. A ctx with no deadline leaves
// the write lock bound as it is. It costs the transaction a round trip.
func WaitForLocksUntilDeadline(ctx context.Context, tx pgx.Tx) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	if _, err := tx.Exec(ctx, boundedWaitBound, lockTimeoutSetting(time.Until(deadline))); err != nil {
		return fmt.Errorf("set the transaction's lock_timeout to its deadline: %w", err)
	}
	return nil
}

// BeforeCommit is a step every transaction a request opens takes just before its COMMIT, inside
// that transaction, so what it writes commits with the request's own write or not at all. An
// error from it rolls the transaction back, and Commit returns that error.
//
// Every transaction but a read-only one: the step writes, and Postgres refuses a write inside a
// READ ONLY transaction, which has nothing to mark in any case (Commit).
type BeforeCommit func(ctx context.Context, tx pgx.Tx) error

type beforeCommitKey struct{}

// WithBeforeCommit returns ctx carrying step, which every transaction this pool opens under ctx,
// or a context derived from it, takes before it commits (BeforeCommit). A keyed write marks its
// Idempotency-Key committed this way, in the transaction of the write itself (api/request_keys.go).
// A transaction already failed when its caller commits it skips the step: pgx answers that commit
// with its own rollback error, as it would without one.
func WithBeforeCommit(ctx context.Context, step BeforeCommit) context.Context {
	return context.WithValue(ctx, beforeCommitKey{}, step)
}

// WithoutBeforeCommit returns ctx with the step taken off, for a transaction of a request's that
// writes none of the request's own work: the document rooms' conditional-edit queue takes its
// bookkeeping transaction this way (docs.Service.admitConditionalEdit), since it commits before
// the request's write and a crash between the two must leave the key for the repeat to run rather
// than answer it KEY_COMMITTED for a write nobody made, and a cold read's fold takes its load
// this way (docs.Service.foldContext), since the fold is the document's work rather than the
// request's and outlives the request that waits for it.
func WithoutBeforeCommit(ctx context.Context) context.Context {
	return context.WithValue(ctx, beforeCommitKey{}, BeforeCommit(nil))
}

// trackedTx commits apart from its caller's cancellation (CommitContext) and rolls back apart
// from it too (RollbackContext), clears the caller's mark however the transaction ends, and
// takes the caller's BeforeCommit step before it commits.
// Callers commit and then run a deferred rollback; the release absorbs that second call. Either
// part may be absent: a context the pool does not track has no mark, and one with no step commits
// as it is.
type trackedTx struct {
	pgx.Tx
	release      func()
	beforeCommit BeforeCommit
	// readOnly is set for a transaction opened read-only (pgx.ReadOnly). Postgres refuses every
	// write inside such a transaction, so it takes no BeforeCommit step (Commit), its COMMIT
	// commits nothing whatever becomes of it, and an answer it never gave leaves nothing in
	// doubt (commitTimedOut).
	readOnly bool
}

func (t *trackedTx) Commit(ctx context.Context) error {
	defer t.end()
	ctx, cancel := CommitContext(ctx)
	defer cancel()
	// A read-only transaction takes no step: Postgres refuses every write inside one, and the
	// step is a write (api.markRequestKeyCommitted is a SELECT with an update CTE, which a READ
	// ONLY transaction refuses whether or not the update matches a row). Such a transaction
	// wrote nothing, so it has nothing to mark.
	if t.beforeCommit != nil && !t.readOnly && t.Tx.Conn().PgConn().TxStatus() == 'T' {
		if err := t.beforeCommit(ctx, t.Tx); err != nil {
			// The COMMIT was never sent, so this transaction rolled back and its outcome is
			// known, whatever ended the step: the error is the step's own, named a busy
			// database when the commit's own bound is what ended it (RanOutTheCommitBound).
			// The rollback runs on a context of its own, as Rollback's does, so a step whose
			// bound passed before it sent anything still sends its ROLLBACK and gets its
			// connection back idle. A bound that cuts a statement in flight is pgconn's to
			// end: it closes the connection under the step, so the ROLLBACK here fails and
			// pgx drops that connection. The outcome is the same either way -- no COMMIT was
			// sent, and the backend's statement and its transaction end with it -- and what
			// the in-flight case costs is the connection alone.
			err = RanOutTheCommitBound(ctx, err)
			rollbackCtx, cancelRollback := RollbackContext(ctx)
			defer cancelRollback()
			_ = t.Tx.Rollback(rollbackCtx)
			return err
		}
	}
	return commitTimedOut(ctx, t.readOnly, t.Tx.Commit(ctx))
}

// Rollback ends the transaction on a context of its own (RollbackContext), so a caller whose
// context is already cancelled - every write handler's deferred rollback after its client went
// away - still sends the ROLLBACK and gets its connection back idle.
func (t *trackedTx) Rollback(ctx context.Context) error {
	defer t.end()
	ctx, cancel := RollbackContext(ctx)
	defer cancel()
	return t.Tx.Rollback(ctx)
}

// commitTimeout bounds a commit CommitContext has detached from its caller.
const commitTimeout = 10 * time.Second

// ErrCommitTimeout is a commit of a transaction that may have written which the database did not
// answer: the bound of its own context cut it off (CommitContext, commitTimeout), the socket to
// Postgres timed out under it, which is a bound of this server's that passed on that connection,
// or the connection ended under it once it was sent (connectionLost: Postgres ending the session,
// or pgconn closing the connection while it read the answer). Its outcome is unknown - the
// transaction may have committed, and nothing the server holds says whether it did - so it is
// neither a pool that could not serve the call (ErrPoolBusy) nor a failure that rolled back: a
// request answers it 500, naming the unknown outcome, and a client must read what it wrote before
// sending it again (admission.WriteUnserved, cmd/dispatch/README.md "Load errors"). A commit its
// own bound cut off wraps context.DeadlineExceeded too, so errors.Is finds both.
//
// Two unanswered COMMITs are not this and are ErrPoolBusy (commitTimedOut): a read-only
// transaction's, and one of a stall pgconn marks safe to retry, which the COMMIT was never sent
// through. Nothing can have committed in either, so the request is refused and may be sent
// again. The same two COMMITs whose connection ended are not this either, and stay the error
// they are, which ConnectionLost answers as busy: pgconn refuses a COMMIT on a connection it
// closed earlier at its lock (`conn closed`), before anything is sent.
var ErrCommitTimeout = errors.New("the commit was not answered; its outcome is unknown")

// IsCommitTimeout reports whether err is a commit the database did not answer (ErrCommitTimeout).
func IsCommitTimeout(err error) bool { return errors.Is(err, ErrCommitTimeout) }

// commitTimedOut names err for a COMMIT the database did not answer: ctx, the commit's own
// context, is the bound that ended it - pgx answers a statement its context cut off with that
// context's error and not with its cause, so the cause is read here instead - or the socket
// timed out while that context was still live, which is a bound of this server's that passed on
// the connection (stalled), or the connection ended under the COMMIT (connectionLost). Each is
// told here rather than left to poolBusy, which never sees a commit, because the outcome of a
// COMMIT whose answer was lost is unknown where every other lost answer leaves the transaction
// rolled back.
//
// Unknown, that is, unless the COMMIT never reached the database or the transaction wrote
// nothing, and both are a busy database like any other the server's bounds ended (ErrPoolBusy) -
// a refusal the caller may make again, answered 503 DATABASE_BUSY rather than with an unknown
// outcome a client would have to read back:
//
//   - a stall pgconn marks safe to retry is one the COMMIT was never sent through, so nothing
//     of the transaction can have committed, exactly as poolBusy reads such a stall for every
//     other call (acquire.go);
//   - a read-only transaction's COMMIT commits nothing whether or not it ran, since Postgres
//     let it write nothing.
//
// A connection that ended under either of those two is returned as it is, for ConnectionLost to
// answer busy in the same way. A commit that failed for any other reason, and one that did not
// fail at all, is returned as it is.
func commitTimedOut(ctx context.Context, readOnly bool, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(context.Cause(ctx), ErrCommitTimeout) || stalled(ctx, err):
		switch {
		case pgconn.SafeToRetry(err):
			return fmt.Errorf("%w: the database stopped answering before the COMMIT reached it: %w", ErrPoolBusy, err)
		case readOnly:
			return fmt.Errorf("%w: the database did not answer this read-only transaction's COMMIT, which commits nothing: %w", ErrPoolBusy, err)
		}
		return fmt.Errorf("%w: %w", ErrCommitTimeout, err)
	case !readOnly && lostAfterSent(err):
		return fmt.Errorf("%w: the connection ended under the COMMIT: %w", ErrCommitTimeout, err)
	}
	return err
}

// RanOutTheCommitBound names err a database too busy to serve the call (ErrPoolBusy) when ctx -
// the bound CommitContext puts on the work a commit does - ran out while that work was still
// before the COMMIT itself. Nothing of the transaction committed: these statements run inside it
// and it is rolled back, so the request is refused 503 DATABASE_BUSY and the client may send the
// whole write again (admission.WriteUnserved). Unnamed, the bare context deadline pgx answers
// such a statement with is a failure no rule knows, which answers the client 500 INTERNAL
// (INFRA-592: a ten-times load with the database proxy frozen answered POST
// /api/v1/artifacts/{id}/edits that way, on the announcement).
//
// Two callers run statements on that bound: the document ledger's settlement credit and
// announcement (docs.Ledger.commit), and a request's Idempotency-Key step (trackedTx.Commit,
// BeforeCommit). A COMMIT the server did send and the database never answered is
// ErrCommitTimeout, whose outcome nobody knows, and this never covers it: it names only the
// statements run before the COMMIT is reached.
//
// A failure of any other kind, and one met while the bound still stood - a stall of the socket,
// which DatabaseStalled names and which rolls the transaction back too - is returned as it is.
func RanOutTheCommitBound(ctx context.Context, err error) error {
	if err == nil || !errors.Is(context.Cause(ctx), ErrCommitTimeout) {
		return err
	}
	return fmt.Errorf("%w: the commit's bound passed before the COMMIT was sent, and the transaction rolled back: %w", ErrPoolBusy, err)
}

// commitBoundKey carries the bound a test puts on the commits made under a context
// (WithCommitBoundForTest).
type commitBoundKey struct{}

// WithCommitBoundForTest returns ctx with bound as the bound of every commit made under it, in
// place of commitTimeout. It is for tests outside this package alone, which make a commit run out
// its bound in milliseconds rather than in production's ten seconds; nothing in production calls
// it, and a context carrying none commits under commitTimeout.
func WithCommitBoundForTest(ctx context.Context, bound time.Duration) context.Context {
	return context.WithValue(ctx, commitBoundKey{}, bound)
}

// CommitContext is the context a transaction commits on: ctx's values without its cancellation
// or deadline, bounded at commitTimeout with ErrCommitTimeout as the cause. A commit cut short by
// its caller - a client that went away, a caller's deadline that passed - has an unknown outcome,
// since the server may have committed it, and the document service fails every room such a
// transaction wrote, which then reload the durable document (docs.Ledger.Commit); a request that
// ended mid-commit would take its rooms down with it. So a commit in flight runs to its end, and
// only its own bound, ten seconds, keeps one the database never answers from holding its
// connection for good. Every transaction this pool opens commits on it.
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, ok := ctx.Value(commitBoundKey{}).(time.Duration)
	if !ok {
		bound = commitTimeout
	}
	return context.WithTimeoutCause(context.WithoutCancel(ctx), bound, ErrCommitTimeout)
}

// RollbackTimeout bounds a rollback RollbackContext has detached from its caller. A ROLLBACK
// takes no lock and waits for nothing, so the only rollback that reaches this bound is one on a
// connection the database has stopped answering on, which the bound then stops holding. It is
// exported because the request pool's idle-in-transaction bound is computed from it
// (RequestIdleInTransactionTimeout).
const RollbackTimeout = 5 * time.Second

// ColdReadBound is the bound on one cold read's fold of a document's stored history: the document
// service's own coldReadTimeout is this constant (docs/read_cache.go), and
// TestTheRequestPoolsIdleBoundOutlastsAColdRead holds the two together. It is declared here
// rather than in docs because the request pool's idle-in-transaction bound is computed from it
// and docs imports this package, never the other way round.
//
// It is the longest a request-pool transaction sits idle by design. A write has no deadline of
// its own (api's defaultReadDeadline says why), so a write that reads a document it holds no
// fork for waits out the fold with its transaction open and no statement running: recording a
// check result does exactly that, under the two issue row locks it holds (api/results.go's
// refreshResolvedCheck, docs/read_cache.go's coldRead).
const ColdReadBound = 60 * time.Second

// requestIdleMargin is what RequestIdleInTransactionTimeout leaves over that longest idle wait
// and the rollback that follows it, so the bound never ends a transaction the server was itself
// about to end.
const requestIdleMargin = 5 * time.Second

// RequestIdleInTransactionTimeout is the idle_in_transaction_session_timeout every request-pool
// connection carries: the longest a request transaction may sit idle by design, plus the bound on
// the rollback that ends it, plus a margin - computed from those three, so a raised cold-read
// bound raises this with it. The background, outbox, room-appends and settlements pools'
// connections carry the same bound, and a compaction's write, the compaction sweep's removal of a
// document from its list and the room-sync catch-up's confirmation of a replaced history set it for
// their own transaction (configurePool says why those and no others, BoundIdleInTransaction): each
// of those transactions does its slow work before it begins, or within a bound below this one, and
// then runs its statements one after another, so the one bound serves them all and no second one
// is kept.
//
// Postgres is the only thing that can end an orphaned request transaction: one whose pgx
// connection was abandoned, and whose close reached Postgres late or not at all, holds the rows
// it locked and the event horizon until Postgres notices the socket is gone, which a stall run
// measured at over two seconds past every request that could have owned it and which a starved
// proxy can stretch much further. This bound ends it instead.
const RequestIdleInTransactionTimeout = ColdReadBound + RollbackTimeout + requestIdleMargin

// requestIdleInTransaction is the bound configurePool writes onto the bounded pools' connections
// and BoundIdleInTransaction sets for one transaction. Tests in this package shorten it so a
// transaction outlasts it in milliseconds rather than in the seventy seconds production runs;
// nothing else moves it.
var requestIdleInTransaction = RequestIdleInTransactionTimeout

// BoundIdleInTransaction bounds tx's own idle time at the bound a request-pool connection carries
// (RequestIdleInTransactionTimeout), for tx alone: set_config's third argument makes it local to
// the transaction, so the connection carries nothing once tx ends. It is the first statement of a
// transaction that takes a lock a request's write waits on, on a pool whose connections carry no
// bound because another of its transactions - a fold's read - legitimately sits idle for longer: a
// compaction's write and the compaction sweep's removal of a document from its list (docs'
// forgetCompactionCandidate) on the compaction pool, and the room-sync catch-up's confirmation of a
// replaced history (docs' historyReplacedUnderLock) on the rooms pool.
func BoundIdleInTransaction(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select set_config($1, $2, true)`, idleInTransactionParam, idleInTransactionSetting()); err != nil {
		return fmt.Errorf("bound the transaction's idle time: %w", err)
	}
	return nil
}

// RollbackContext is the context a transaction rolls back on: ctx's values without its
// cancellation or deadline, bounded at RollbackTimeout. pgconn refuses a statement on a context
// that is already done before it writes anything to the socket, and pgx answers a rollback that
// failed by killing the connection (pgx.dbTx.Rollback -> Conn.die), so a rollback on the
// cancelled context of a request whose client went away sent no ROLLBACK at all: the transaction
// ended by the pooled connection being destroyed and dialled again, exactly when the server is
// busiest. On this context the ROLLBACK is sent, the transaction ends at once instead of when
// the connection dies, and the connection goes back to the pool idle.
func RollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), RollbackTimeout)
}

// RanOutTheRollbackBound names err a database too busy to serve the call (ErrPoolBusy) when ctx,
// a RollbackContext, ran out its bound before the database answered the rollback. pgx answers a
// statement its context cut off with that context's error (`timeout: context deadline
// exceeded`), which no rule reads as the database's, and closes the connection under it
// (connectionLost): the transaction the rollback was part of has ended without committing, and
// whatever runs on that connection next is refused `conn closed`. So a write whose own rollback
// of a savepoint inside its transaction met that is refused 503 DATABASE_BUSY, and the client may
// send it again (admission.WriteUnserved). api's endAdviceSavepoint is such a rollback, and says
// how INFRA-592's ten-times freeze met it.
//
// A failure of any other kind, and one met while the bound still stood - a stall of the socket,
// which DatabaseStalled names, or a connection that ended, which ConnectionLost names - is returned
// as it is.
func RanOutTheRollbackBound(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	return fmt.Errorf("%w: the rollback's bound passed before the database answered it, and the transaction ended with its connection: %w", ErrPoolBusy, err)
}

// ErrRequestEnded is a commit never started, because the request that would have made it had
// already ended - its client went away, or its own bound passed - and nothing it wrote is worth
// the ten seconds CommitContext would give the commit, nor the locks and the event horizon that
// commit holds while it runs. Nothing was committed, which a caller may rely on; the handler's
// deferred rollback ends the transaction and api.writeHandlerError answers it as it answers
// every request the server could not serve (admission.WriteUnserved): nothing written and a 499
// line for a client that went away.
var ErrRequestEnded = errors.New("the request ended before its commit began; nothing was committed")

// CommitRefusal is the error a commit on ctx is refused with when ctx has already ended, or its
// request was refused (RefuseRequest, which it answers as it was given), and nil while it is live.
// It is read where a commit begins and before CommitContext detaches it - the transaction an API
// handler opened (api.server.begin) and the document ledger's commit (docs.Ledger.Commit) - so a
// request already gone or refused starts no commit, rather than holding its rows, its documents'
// rooms and the event horizon for a commit nobody will read.
func CommitRefusal(ctx context.Context) error {
	if state, ok := ctx.Value(holdingKey{}).(*holding); ok {
		if refusal := state.refused(); refusal != nil {
			return refusal
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRequestEnded, err)
	}
	return nil
}

// end clears the caller's mark, when the pool tracks one.
func (t *trackedTx) end() {
	if t.release != nil {
		t.release()
	}
}

type errRow struct {
	err error
}

func (r errRow) Scan(...any) error { return r.err }

// errBatchResults carries a refusal through the batch surface, which has no error return of
// its own: every result the caller reads is the refusal, and the refused batch begins no
// transaction.
type errBatchResults struct {
	err error
}

func (b errBatchResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatchResults) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatchResults) QueryRow() pgx.Row                { return errRow{err: b.err} }
func (b errBatchResults) Close() error                     { return b.err }
func (b errBatchResults) Tx() pgx.Tx                       { return nil }

// busyRow maps a deferred acquire error to ErrPoolBusy when the row is scanned, and gives back the
// pinned connection the row was read on, if any, once it is. The call it carries the error of is
// the statement itself, which runs outside any transaction (commitsItself).
type busyRow struct {
	pgx.Row
	ends <-chan struct{}
	pool string
	done func()
}

func (r busyRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	finished(r.done)
	return poolBusyUntil(r.ends, r.pool, err, commitsItself)
}

// busyBatchResults maps a deferred acquire error to ErrPoolBusy in every result the caller reads,
// and gives back the pinned connection the batch ran on, if any, once it closes. Its statements
// run outside any transaction, as a single statement's do (commitsItself).
type busyBatchResults struct {
	pgx.BatchResults
	ends <-chan struct{}
	pool string
	done func()
}

func (b busyBatchResults) Exec() (pgconn.CommandTag, error) {
	tag, err := b.BatchResults.Exec()
	return tag, poolBusyUntil(b.ends, b.pool, err, commitsItself)
}

func (b busyBatchResults) Query() (pgx.Rows, error) {
	rows, err := b.BatchResults.Query()
	return rows, poolBusyUntil(b.ends, b.pool, err, commitsItself)
}

func (b busyBatchResults) QueryRow() pgx.Row {
	return busyRow{Row: b.BatchResults.QueryRow(), ends: b.ends, pool: b.pool}
}

func (b busyBatchResults) Close() error {
	err := b.BatchResults.Close()
	finished(b.done)
	return poolBusyUntil(b.ends, b.pool, err, commitsItself)
}
