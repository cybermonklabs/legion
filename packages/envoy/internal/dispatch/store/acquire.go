package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pools a Pool opens, by the name each one carries: its connections' application_name in
// pg_stat_activity is dispatch-<name> (configurePool), and the name is its entry in Stats and what
// Use is asked about.
const (
	// RequestsPool is the shared pool store.Open opens: what every API request reads and writes
	// through (SharedPoolSize).
	RequestsPool = "requests"
	// RoomsPool is where a document load takes its connection (Pool.Rooms).
	RoomsPool = "rooms"
	// RoomAppendsPool is where a document room's browser updates are stored (Pool.RoomAppends).
	RoomAppendsPool = "room-appends"
	// HealthPool is the health probe's one connection (Pool.Healthy).
	HealthPool = "health"
	// ListenPool is what the LISTEN connections are dialled through (Pool.Listener).
	ListenPool = "listen"
	// LeasesPool is where a leased maintenance pass holds its lease (Pool.Leases).
	LeasesPool = "leases"
	// OutboxPool is the events outbox's two connections, its scan's and its hold renewal's (Pool.Outbox).
	OutboxPool = "outbox"
	// CompactionPool is where a document's compaction reads and writes (Pool.Compaction).
	CompactionPool = "compaction"
	// BackgroundPool is the background loops' pool (Pool.Background).
	BackgroundPool = "background"
	// LivePool is the live-update readers' pool (Pool.Live).
	LivePool = "live"
	// SettlementsPool is the document settlements' pool (Pool.Settlements).
	SettlementsPool = "settlements"
	// HorizonPool is the event log's safe horizon's pool (Pool.Horizon).
	HorizonPool = "horizon"
	// RegistryPool is the server process registry's one connection (Pool.OpenRegistry).
	RegistryPool = "registry"
)

// poolNames is every pool name, in the order Stats reports them. The registry's pool is in Stats
// once processes.Register has opened it (Pool.OpenRegistry).
var poolNames = [...]string{
	RequestsPool, RoomsPool, RoomAppendsPool, HealthPool, ListenPool, LeasesPool, OutboxPool,
	CompactionPool, BackgroundPool, LivePool, SettlementsPool, HorizonPool, RegistryPool,
}

// applicationNamePrefix begins every pool's application_name: dispatch-<pool>, the one convention
// every pool of the process shares, so pg_stat_activity groups a process's connections by pool and
// the metrics' pool label is the part after the prefix. The connection string's own
// application_name, if it names one, is replaced: a pool's name is what an operator reading
// pg_stat_activity during an incident needs, and it is the same in every deployment.
const applicationNamePrefix = "dispatch-"

// The acquire bounds: how long one acquire on each pool waits for a connection before it gives up
// with ErrPoolBusy. Before them every wait was unbounded, so past capacity callers queued until
// their clients gave up, and the queue only grew. A bound refuses the excess instead, and the
// caller's own deadline wins whenever it is sooner (acquireTracer). Each bound is per acquire: a
// write makes an acquire per statement and transaction, one after another (a read pins one
// connection and makes one, PinConnection), so the request's own deadline, not these, bounds
// their sum.
const (
	// requestAcquireBound is the request pool's, and the room loads', the live readers' and the
	// event horizon's too: a request a client is waiting on that has waited three seconds for a
	// connection - a page's whoami waits on the horizon's - is better answered busy, so the
	// client retries, than held for a connection that may never come free.
	requestAcquireBound = 3 * time.Second
	// backgroundAcquireBound is the background loops', the settlements', the compactions' and
	// the rooms' appends': no client's answer is waiting on them, so they may wait far longer
	// than a request, and a refused pass only runs at its next tick - but not without end, so a
	// pool that never frees a connection shows as an error.
	backgroundAcquireBound = 30 * time.Second
	// leaseAcquireBound is the lease pool's. The pool has a connection for every lease name and
	// spare (LeasePoolSize), so a wait at all means a connection is held by something that is not
	// a listed lease; a pass refused here skips (duty.Exclusive), and the next tick tries again.
	leaseAcquireBound = 2 * time.Second
	// listenAcquireBound is the listener pool's. Its one connection is hijacked as soon as it is
	// dialled (listenerPoolSize), so an acquire is a wait for the dials ahead of it - four
	// listeners at most - and then a dial of its own: milliseconds on a database that answers,
	// and far longer than ten seconds only on one that does not, where a listener that gives up
	// redials on its own backoff (pglisten.Listener) instead of waiting out the two-minute
	// connect timeout pgxpool floors a dial at.
	listenAcquireBound = 10 * time.Second
	// coldDialBound is the bound of the acquires that must dial where every other acquire of
	// the request pool waits on a connection already dialled: the boot's, its reachability ping
	// (Open) and the migration runner's after it (Store.migrate), both under forColdDial. The
	// runner's are not one dial but several, because a concurrent index's migration hijacks the
	// connection it runs on and closes it (Store.applyNonTransactionalMigration), so the
	// migration after it dials again. A client waits on none of them, since the process serves
	// nothing yet, and requestAcquireBound would end the boot at a database whose dial takes
	// longer than three seconds - before the probe that is bounded for exactly that ever runs
	// (Pool.HealthyWithin, cmd/dispatch's bootProbeTimeout). Thirty seconds is that probe's
	// bound, for the same reason it is: it leaves a boot that cannot reach its database time to
	// say so before the deploy clock rolls the image back.
	coldDialBound = 30 * time.Second
)

// defaultAcquireBound is pool name's acquire bound; zero is none. The health probe bounds its own
// wait, dial and query together (healthProbeTimeout), so its acquire needs nothing more; the
// registry is its pool's only caller and the outbox's pool has one connection for each of its two
// callers (its scan and its hold renewal), each running one statement or transaction at a time
// and holding at most one connection, so their acquires never wait behind another caller.
func defaultAcquireBound(name string) time.Duration {
	switch name {
	case RequestsPool, RoomsPool, LivePool, HorizonPool:
		return requestAcquireBound
	case BackgroundPool, SettlementsPool, CompactionPool, RoomAppendsPool:
		return backgroundAcquireBound
	case LeasesPool:
		return leaseAcquireBound
	case ListenPool:
		return listenAcquireBound
	}
	return 0
}

// ErrPoolBusy is a call a bound of this server's own, and not the caller's context, ended
// before the database answered it, and after which nothing the caller asked for can have
// committed: an acquire that ran out its pool's acquire bound while the caller was still
// waiting, every connection of that pool having stayed taken for the whole bound, or a call the
// database stopped answering on before the call reached it at all (pgconn.SafeToRetry). The
// caller may retry; a request answers it as busy rather than as a failure. An acquire's error
// wraps context.DeadlineExceeded too, so errors.Is finds both, and IsPoolBusy tells either from
// a deadline of the caller's own.
//
// Every pool answers a bound it ran out with it, the wrapped pools (Pool) and the raw ones
// (RawPool: rooms, leases, listen, registry, health) alike, so a caller tells a pool that could
// not serve it from its own deadline by IsPoolBusy rather than by guessing from a bare
// context.DeadlineExceeded - a guess that read a commit cut off at its bound (ErrCommitTimeout),
// whose outcome is unknown, as a pool that refused before anything ran.
var ErrPoolBusy = errors.New("a bound of this server's ended the call before the database answered it")

// IsPoolBusy reports whether err is a call one of this server's bounds ended before the database
// answered it and before anything of it could have committed (ErrPoolBusy), as opposed to the
// caller's own deadline or cancellation. It may be retried whatever it was. The two calls whose
// outcome a lost answer leaves unknown are told apart before they reach here: a commit is
// ErrCommitTimeout (commitTimedOut) and a statement this pool ran outside a transaction is
// ErrStatementOutcomeUnknown.
func IsPoolBusy(err error) bool { return errors.Is(err, ErrPoolBusy) }

// ErrStatementOutcomeUnknown is a statement one of these pools ran outside a transaction that
// reached the database and was never answered: a bound of this server's stopped the read of its
// answer (DatabaseStalled) and pgconn does not mark it safe to retry, so it may have run. A
// statement outside a transaction commits itself, so its outcome is unknown exactly as an
// unanswered commit's is (ErrCommitTimeout), and it is not retryable: a client that sent it
// again could write twice. A write route answers it 500 COMMIT_OUTCOME_UNKNOWN with no
// Retry-After and an ERROR line; a read route answers it 503 DATABASE_BUSY, since what a read
// runs commits nothing and sending the GET again only reads (admission.WriteUnserved).
//
// Only a statement is this. A call of a pool's that commits nothing whatever the database did
// with it - an acquire, with the ping and the statement-cache deallocation pgx runs inside it,
// and the BEGIN that opens a transaction, which leaves a transaction that rolls back or no
// transaction at all - is a busy pool (commitsNothing, poolBusy).
//
// A statement inside a transaction is none of this either: its transaction rolls back, so
// nothing of it committed, and it never passes through this package's pools in the first place -
// it runs on the pgx transaction the caller holds.
var ErrStatementOutcomeUnknown = errors.New(
	"the database stopped answering on the connection after this statement reached it; it ran outside a transaction, so its outcome is unknown",
)

// IsStatementOutcomeUnknown reports whether err is a statement run outside a transaction whose
// answer never came and which may have committed (ErrStatementOutcomeUnknown).
func IsStatementOutcomeUnknown(err error) bool { return errors.Is(err, ErrStatementOutcomeUnknown) }

// stalled reports whether err is a call pgx answered with a timeout of the socket to Postgres
// (pgconn.Timeout) while ctx, the caller's own context, was still live.
//
// pgx puts a deadline on that socket for one reason: a context it is watching was cancelled, and
// the deadline is how it stops the read in flight (pgconn's DeadlineContextWatcherHandler). So a
// socket timeout raised while the caller's context is live belongs to a bound of this server's -
// the acquire bound this package puts on the call (acquireTracer), which pgx also runs an idle
// connection's ping and that ping's statement-cache deallocation under (pgxpool's Acquire,
// pgx.Conn.Exec), or a bound that passed on an earlier call of the same pooled connection, whose
// deadline pgx had not lifted by the time this call read (the race pgconn's
// exitPotentialWriteReadDeadlock names). Before this such a call was answered 500 naming no
// cause (INFRA-592).
//
// It is the socket's timeout alone. A context's deadline pgx reports as
// context.DeadlineExceeded is poolBusy's own case; the caller's own deadline or cancellation is
// the caller's to answer, which ctx says; and an error that is no timeout - a statement Postgres
// refused, a connection that broke - stays the error it is, so a fault the server should be
// alarmed over is not hidden behind a refusal.
func stalled(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) && pgconn.Timeout(err)
}

// DatabaseStalled reports whether err is a stall (stalled) after which nothing the caller asked
// for can have committed, so its request may be refused as busy and sent again. Every stall a
// caller meets outside this package's pools is one: a statement inside a transaction, which
// rolls back. A statement one of these pools ran outside a transaction is not, unless pgconn
// marks it safe to retry; poolBusy names that one ErrStatementOutcomeUnknown, and this reports
// false for it so its caller answers the unknown outcome rather than a retry.
func DatabaseStalled(ctx context.Context, err error) bool {
	return stalled(ctx, err) && !errors.Is(err, ErrStatementOutcomeUnknown)
}

// Unserved reports whether err is any call a bound of this server's ended before the database
// answered it: a pool with no connection free (ErrPoolBusy), a stall after which nothing can
// have committed (DatabaseStalled), and a statement outside a transaction the database may
// still have run (ErrStatementOutcomeUnknown) alike.
//
// It is for a caller whose work it may run again whatever that statement did, because the work
// reads what it needs and writes what it reads: a settlement pass, which is dropped and armed
// again (docs.Service.settleRoomWithin), a duty pass, which the next tick makes, a sweep. Such a
// caller must not count the database's overload as a failure of its own - under a stall of the
// whole pod that would fail every room whose settlement met it, which is the collapse these
// bounds exist to prevent - and must not tell the three shapes apart, because for it they mean
// one thing. A caller answering a client does tell them apart, and uses IsPoolBusy,
// DatabaseStalled and IsStatementOutcomeUnknown instead (admission.WriteUnserved,
// api.writeHandlerError): a client may repeat a request, where a pass only runs again.
func Unserved(ctx context.Context, err error) bool {
	return IsPoolBusy(err) || stalled(ctx, err)
}

// outcome is what the call poolBusy maps an error for can have left behind at the database if it
// reached it, which only the caller of the pool knows: a statement it runs outside a transaction
// commits itself, and everything else a pool does commits nothing.
type outcome bool

const (
	// commitsNothing is a call that leaves nothing behind whatever the database did with it: an
	// acquire (with the ping and the statement-cache deallocation pgx runs inside it), the
	// pool's own ping, and the BEGIN that opens a transaction - a BEGIN the database never
	// answered opened no transaction the caller can commit, and one it did answer is rolled
	// back by the caller's deferred rollback or ended by the connection being destroyed.
	commitsNothing outcome = false
	// commitsItself is a statement run outside any transaction, which commits itself: the
	// pool's Query, QueryRow, Exec, CopyFrom and SendBatch, whose statements may write.
	commitsItself outcome = true
)

// poolBusy names err for what a bound of this server's did to the call: a deadline exceeded
// while the caller's own context is still live can only be the bound acquireTracer put on the
// acquire, since the statement itself runs on the caller's context, and a socket that timed out
// under a live caller is one of those bounds too (stalled). A stall pgconn marks safe to retry
// never reached the database, and one of a call that commits nothing left nothing behind even
// though it did: both are a busy pool like any other. Only a statement that reached the
// database and commits itself may have run, and that one is ErrStatementOutcomeUnknown. Each
// wrap names what the pool met, so the line a refusal is logged on says which bound passed and
// what it left behind. A caller whose own deadline came first gets its own error back.
func poolBusy(ctx context.Context, pool string, err error, commits outcome) error {
	switch {
	case err == nil || errors.Is(err, ErrPoolBusy) || errors.Is(err, ErrStatementOutcomeUnknown):
		return err
	case stalled(ctx, err):
		switch {
		case pgconn.SafeToRetry(err):
			return fmt.Errorf("%s pool: %w: the database stopped answering before the call reached it: %w", pool, ErrPoolBusy, err)
		case commits == commitsNothing:
			return fmt.Errorf("%s pool: %w: the database stopped answering on the connection, and this call commits nothing: %w", pool, ErrPoolBusy, err)
		}
		return fmt.Errorf("%s pool: %w: %w", pool, ErrStatementOutcomeUnknown, err)
	case ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("%s pool: %w: no connection came free within the pool's acquire bound: %w", pool, ErrPoolBusy, err)
}

// acquireBuckets are the upper bounds of the acquire-wait histogram's buckets, from a
// millisecond, an idle pool's hand-over, to the longest acquire bound.
var acquireBuckets = [...]time.Duration{
	time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 50 * time.Millisecond,
	100 * time.Millisecond, 500 * time.Millisecond, time.Second, 2500 * time.Millisecond,
	5 * time.Second, 10 * time.Second, 30 * time.Second,
}

// AcquireBucketCount is how many buckets the acquire-wait histogram has.
const AcquireBucketCount = len(acquireBuckets)

// AcquireBucketBounds are the upper bounds, in seconds, of the acquire-wait histogram's buckets:
// AcquireStats.Buckets[i] counts the acquires that waited at most AcquireBucketBounds()[i].
func AcquireBucketBounds() [AcquireBucketCount]float64 {
	var bounds [AcquireBucketCount]float64
	for i, bound := range acquireBuckets {
		bounds[i] = bound.Seconds()
	}
	return bounds
}

// AcquireStats is what one pool's acquire tracer has counted since the pool opened.
type AcquireStats struct {
	// Waiting is the acquires in progress now: callers waiting for a connection of the pool.
	Waiting int64
	// Timeouts is the acquires that ran out the pool's acquire bound (ErrPoolBusy).
	Timeouts uint64
	// Count is the acquires that finished, with a connection or without one, and Sum their
	// waits added together, in seconds.
	Count uint64
	Sum   float64
	// Buckets[i] is how many of those acquires waited at most AcquireBucketBounds()[i]:
	// cumulative, as a Prometheus histogram's buckets are, so Count is the bucket past the last.
	Buckets [AcquireBucketCount]uint64
}

// PoolStats is one open pool's connection counts and acquire waits.
type PoolStats struct {
	Name    string
	Stat    *pgxpool.Stat
	Acquire AcquireStats
}

// acquireTracer bounds and measures every acquire of one pool. pgxpool's Acquire calls
// TraceAcquireStart first and waits for the connection, the ping of an idle one included, on the
// context it returns, and every way the pool hands out a connection - Query, QueryRow, Exec,
// BeginTx, SendBatch, CopyFrom, AcquireFunc - goes through Acquire. So the bound covers every
// caller of the pool, the store.Pool wrappers and the raw pgxpool users alike, while the statement
// still runs on the caller's own context. It counts the connections handed back open too
// (TraceRelease), which pgxpool reports to a configuration's Tracer the same way.
type acquireTracer struct {
	// name makes the application_name, and inner is the tracer the configuration had before this
	// one was composed with it: kept so a pool opened from this pool's configuration composes its
	// own tracer with that one rather than with this.
	name  string
	bound time.Duration
	inner pgx.QueryTracer

	waiting  atomic.Int64
	timeouts atomic.Uint64
	count    atomic.Uint64
	waited   atomic.Int64 // nanoseconds
	buckets  [AcquireBucketCount]atomic.Uint64
	// finished counts the connections handed back open and idle (TraceRelease).
	finished atomic.Uint64
}

// errAcquireBound is the cause of an acquire context the bound ended, which is how
// TraceAcquireEnd tells a bound run out from the caller's own deadline or cancellation.
var errAcquireBound = errors.New("acquire bound reached")

type acquireKey struct{}

// coldDialKey marks the context of the acquire that dials where the pool holds nothing yet.
type coldDialKey struct{}

// forColdDial is ctx for the boot's acquires: the reachability ping (Open) and the migration
// runner's (Store.migrate), made before the process serves anything, each of which may pay a
// fresh dial. They wait under coldDialBound rather than the pool's own, and nothing else uses
// it - every later acquire of that pool is a request's, and a request waits under
// requestAcquireBound.
func forColdDial(ctx context.Context) context.Context {
	return context.WithValue(ctx, coldDialKey{}, struct{}{})
}

// boundFor is the bound this acquire waits under: coldDialBound for the boot's dials, and the
// pool's own for every other caller.
func (t *acquireTracer) boundFor(ctx context.Context) time.Duration {
	if ctx.Value(coldDialKey{}) != nil {
		return coldDialBound
	}
	return t.bound
}

// acquireSpan is one acquire's state from start to end.
type acquireSpan struct {
	started time.Time
	cancel  context.CancelFunc
}

func (t *acquireTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	t.waiting.Add(1)
	span := &acquireSpan{started: time.Now()}
	bound := t.boundFor(ctx)
	if bound > 0 {
		// A caller whose own deadline comes first keeps it, so its acquire ends with its own
		// error and never with ErrPoolBusy.
		if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(span.started) > bound {
			ctx, span.cancel = context.WithTimeoutCause(ctx, bound, errAcquireBound)
		}
	}
	return context.WithValue(ctx, acquireKey{}, span)
}

func (t *acquireTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	t.waiting.Add(-1)
	span, ok := ctx.Value(acquireKey{}).(*acquireSpan)
	if !ok {
		return
	}
	waited := time.Since(span.started)
	if span.cancel != nil {
		if data.Err != nil && errors.Is(context.Cause(ctx), errAcquireBound) {
			t.timeouts.Add(1)
		}
		span.cancel()
	}
	t.count.Add(1)
	t.waited.Add(int64(waited))
	for i, bound := range acquireBuckets {
		if waited <= bound {
			t.buckets[i].Add(1)
			break
		}
	}
}

// TraceRelease counts a connection its caller hands back open and idle, which pgxpool keeps for its
// next caller: the work on it has finished. One pgxpool discards instead - closed by a statement
// whose context ended, or handed back mid-statement or inside a transaction - finished nothing, and
// neither did one pgxpool discards inside Acquire because its ping went unanswered, which no caller
// ever held, so it is never released.
func (t *acquireTracer) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	if data.Conn.IsClosed() || data.Conn.PgConn().IsBusy() || data.Conn.PgConn().TxStatus() != 'I' {
		return
	}
	t.finished.Add(1)
}

// TraceQueryStart and TraceQueryEnd do nothing: a connection configuration's Tracer must be a
// pgx.QueryTracer, and this one traces acquires and releases only.
func (*acquireTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*acquireTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// stats reads what the tracer has counted. The counters are read one at a time, so a scrape that
// races an acquire may see it in one counter and not yet in another.
func (t *acquireTracer) stats() AcquireStats {
	stats := AcquireStats{
		Waiting:  t.waiting.Load(),
		Timeouts: t.timeouts.Load(),
		Count:    t.count.Load(),
		Sum:      time.Duration(t.waited.Load()).Seconds(),
	}
	var cumulative uint64
	for i := range t.buckets {
		cumulative += t.buckets[i].Load()
		stats.Buckets[i] = cumulative
	}
	return stats
}

// composedTracer is an acquireTracer composed with a tracer the configuration already had: every
// call that tracer takes still reaches it, and the acquire tracer runs first, so the other one
// sees the bounded context.
type composedTracer struct {
	*multitracer.Tracer
	acquire *acquireTracer
}

// acquireTracerOf is the acquire tracer configurePool set on a configuration, or nil.
func acquireTracerOf(tracer pgx.QueryTracer) *acquireTracer {
	switch tracer := tracer.(type) {
	case *acquireTracer:
		return tracer
	case *composedTracer:
		return tracer.acquire
	}
	return nil
}

// idleInTransactionParam is the session setting Postgres ends an idle transaction by, which only
// the request pool's connections carry (configurePool, RequestIdleInTransactionTimeout).
const idleInTransactionParam = "idle_in_transaction_session_timeout"

// configurePool names config's connections dispatch-<name> in application_name
// (applicationNamePrefix), bounds an idle transaction on them where they are the request pool's
// (idleInTransactionParam), and sets the pool's acquire tracer, bounded at bound (zero is none)
// and composed with any tracer config already carries. A configuration copied from another pool's
// is re-configured from that pool's original tracer, not stacked on its acquire tracer.
func configurePool(config *pgxpool.Config, name string, bound time.Duration) *acquireTracer {
	inner := config.ConnConfig.Tracer
	if configured := acquireTracerOf(inner); configured != nil {
		inner = configured.inner
	}
	tracer := &acquireTracer{name: name, bound: bound, inner: inner}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationNamePrefix + name
	// Only the request pool's connections are bounded. Every separate pool's configuration is a
	// copy of the request pool's (Pool.newSeparate), so each of them deletes the setting here,
	// and a background loop, a settlement, a live reader, a room load or append, a compaction,
	// the event horizon, a lease, the outbox, the registry and the listener may each sit in a
	// transaction for as long as its own work takes. A connection string that sets the
	// parameter itself is replaced either way, as its application_name is.
	if name == RequestsPool {
		config.ConnConfig.RuntimeParams[idleInTransactionParam] = strconv.FormatInt(requestIdleInTransaction.Milliseconds(), 10)
	} else {
		delete(config.ConnConfig.RuntimeParams, idleInTransactionParam)
	}
	if inner == nil {
		config.ConnConfig.Tracer = tracer
		return tracer
	}
	composed := multitracer.New(inner)
	composed.PoolAcquireTracers = append([]pgxpool.AcquireTracer{tracer}, composed.PoolAcquireTracers...)
	composed.PoolReleaseTracers = append([]pgxpool.ReleaseTracer{tracer}, composed.PoolReleaseTracers...)
	config.ConnConfig.Tracer = &composedTracer{Tracer: composed, acquire: tracer}
	return tracer
}

// BoundAcquires re-configures config, a copy of an open pool's configuration (Pool.Config), so a
// pool opened from it bounds its acquires at bound, under the same pool name. Tests outside this
// package shorten a full pool's wait with it, so the pool answers ErrPoolBusy in milliseconds
// rather than in production's seconds.
func BoundAcquires(config *pgxpool.Config, bound time.Duration) {
	name := RequestsPool
	if configured := acquireTracerOf(config.ConnConfig.Tracer); configured != nil {
		name = configured.name
	}
	configurePool(config, name, bound)
}
