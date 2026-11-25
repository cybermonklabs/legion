package store

import (
	"cmp"
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
// pg_stat_activity is dispatch-<name>, after the connection string's own name and a slash where it
// names one (applicationName), and the name is its entry in Stats and what Use is asked about.
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
	// TextTreesPool is where the background display tree fill reads and writes (Pool.TextTrees).
	TextTreesPool = "text-trees"
	// RegistryPool is the server process registry's one connection (Pool.OpenRegistry).
	RegistryPool = "registry"
)

// poolNames is every pool name, in the order Stats reports them. The registry's pool is in Stats
// once processes.Register has opened it (Pool.OpenRegistry).
var poolNames = [...]string{
	RequestsPool, RoomsPool, RoomAppendsPool, HealthPool, ListenPool, LeasesPool, OutboxPool,
	CompactionPool, BackgroundPool, LivePool, SettlementsPool, TextTreesPool, HorizonPool,
	RegistryPool,
}

// applicationNamePrefix begins every pool's own name in application_name: dispatch-<pool>, the one
// convention every pool of the process shares, so pg_stat_activity groups a process's connections
// by pool and the metrics' pool label is the part after the prefix (applicationName).
const applicationNamePrefix = "dispatch-"

// maxApplicationName is how many bytes of an application_name pg_stat_activity shows: Postgres
// keeps NAMEDATALEN - 1 of them and drops the rest.
const maxApplicationName = 63

// applicationName is the application_name of pool's connections: the pool's own name,
// dispatch-<pool>, and, where the connection string names an application_name of its own (client),
// that name and a slash before it, <client>/dispatch-<pool>. Each says what the other cannot. The
// pool's is what an operator reading pg_stat_activity during an incident needs, and it is the same
// in every deployment; the connection string's is how a deployment, or a test running two builds
// on one database, tells its processes' connections from another's. client is cut short so the
// whole fits maxApplicationName, so the part cut is never the pool's name. Each byte of it outside
// printable ASCII becomes '?' first, as Postgres before 16 makes it (16 and later write it as the
// four bytes \xNN), so what is cut here is the length pg_stat_activity shows.
func applicationName(client, pool string) string {
	own := applicationNamePrefix + pool
	if client == "" {
		return own
	}
	printable := []byte(client)
	for i, c := range printable {
		if c < ' ' || c > '~' {
			printable[i] = '?'
		}
	}
	room := maxApplicationName - len(own) - len("/")
	return string(printable[:min(len(printable), room)]) + "/" + own
}

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
	// leaseAcquireBound is the lease pool's. The pool has a connection for every caller that
	// reserved one and two spare (LeasePoolSize), so a wait at all means a connection is held by
	// something that is not a reserved caller; a pass refused here skips (duty.Exclusive), and the
	// next tick tries again, and a hold's renewal refused here is tried again on the next renewal.
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
	case BackgroundPool, SettlementsPool, CompactionPool, RoomAppendsPool, TextTreesPool:
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
// committed. Eight calls produce it:
//
//   - an acquire that ran out its pool's acquire bound while the caller was still waiting,
//     every connection of that pool having stayed taken for the whole bound (poolBusy);
//   - a call the database stopped answering on before the call reached it at all
//     (pgconn.SafeToRetry), which is no call at all (poolBusy, commitTimedOut);
//   - a call the database stopped answering on that commits nothing wherever it got to - an
//     acquire's ping, a pool's own ping, the BEGIN that opens a transaction (poolBusy,
//     commitsNothing);
//   - a read-only transaction's COMMIT the database never answered, which commits nothing
//     whether or not it ran (commitTimedOut);
//   - a statement run on the commit's own bound before the COMMIT was sent, whose transaction
//     rolled back (RanOutTheCommitBound);
//   - a rollback that ran out its own bound before the database answered it, which pgconn ends
//     by closing the connection under the transaction, so that transaction commits nothing
//     (RanOutTheRollbackBound);
//   - work outside admission that gave its place of the request pool's share outside admission
//     back for a wait on another server - a lapsed sign-in's refresh with the sign-in issuer, an
//     IAP assertion's read of IAP's key set - and was refused a place again for what it reads and
//     writes after (admission's Away.TakeBack), before any of it ran;
//   - a wait for a lock another transaction holds that ran out its bound (lockWait) - a
//     read-phase owner lock's, another advisory lock a write takes (LockWithinBound), or a row
//     lock a write takes (LockRowsWithinBound, WriteBatch.QueueWithinBound) - which fails the
//     caller, whose transaction then rolls back: an advisory lock's wait leaves its transaction
//     as it was before the lock first, and a row lock's has aborted it (boundedWait.refused).
//
// The caller may retry; a request answers it as busy rather than as a failure. An acquire's
// error wraps context.DeadlineExceeded too, so errors.Is finds both, and IsPoolBusy tells any
// of them from a deadline of the caller's own.
//
// Every pool answers a bound it ran out with it, the wrapped pools (Pool) and the raw ones
// (RawPool: rooms, leases, listen, registry, health) alike, so a caller tells a pool that could
// not serve it from its own deadline by IsPoolBusy rather than by guessing from a bare
// context.DeadlineExceeded - a guess that read a commit cut off at its bound (ErrCommitTimeout),
// whose outcome is unknown, as a pool that refused before anything ran.
var ErrPoolBusy = errors.New("a bound of this server's ended the call before the database answered it")

// IsPoolBusy reports whether err is a call one of this server's bounds ended before the database
// answered it and before anything of it could have committed (ErrPoolBusy), as opposed to the
// caller's own deadline or cancellation. A statement Postgres refused because its wait for a lock
// ran out a lock_timeout (lock_not_available, 55P03: ranOutItsBound) is one too, wherever that
// lock_timeout came from: every connection and transaction of the request pool waits under the
// write lock bound (lockWait) by default (configurePool, Pool.begin), so a plain statement there -
// a locking read, a write of a held row, an advisory lock - that waits that long is refused so,
// and the refusal aborts the statement and the transaction it ran in, which then rolls back. A
// bounded wait of this package's names its own refusal ErrPoolBusy (heldPastItsBound). It may be
// retried whatever it was. The two calls whose outcome a lost answer leaves unknown are told apart
// before they reach here: a commit is ErrCommitTimeout (commitTimedOut) and a statement this pool
// ran outside a transaction is ErrStatementOutcomeUnknown.
func IsPoolBusy(err error) bool { return errors.Is(err, ErrPoolBusy) || ranOutItsBound(err) }

// ErrStatementOutcomeUnknown is a statement one of these pools ran outside a transaction that
// reached the database and was never answered: a bound of this server's stopped the read of its
// answer (DatabaseStalled), or the connection ended under it (connectionLost), and pgconn does not
// mark it safe to retry, so it may have run. A statement outside a transaction commits itself, so
// its outcome is unknown exactly as an unanswered commit's is (ErrCommitTimeout), and it is not
// retryable: a client that sent it again could write twice. A write route answers it 500
// COMMIT_OUTCOME_UNKNOWN with no Retry-After and an ERROR line; a read route answers it 503
// DATABASE_BUSY, since what a read runs commits nothing and sending the GET again only reads
// (admission.WriteUnserved).
//
// Only a statement is this. A call of a pool's that commits nothing whatever the database did
// with it - an acquire, with the ping and the statement-cache deallocation pgx runs inside it,
// and the BEGIN that opens a transaction, which leaves a transaction that rolls back or no
// transaction at all - is a busy pool (commitsNothing, poolBusy).
//
// A statement inside a transaction is none of this either: its transaction rolls back, or ends
// with its connection, so nothing of it committed, and it never passes through this package's
// pools in the first place - it runs on the pgx transaction the caller holds.
var ErrStatementOutcomeUnknown = errors.New(
	"this statement reached the database, and the database stopped answering or the connection ended before its answer came; it ran outside a transaction, so its outcome is unknown",
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
// the caller's to answer, which ctx says; a connection that ended under the call is
// connectionLost's; and any other error that is no timeout - a statement Postgres refused - stays
// the error it is, so a fault the server should be alarmed over is not hidden behind a refusal.
func stalled(ctx context.Context, err error) bool {
	return err != nil && stalledWhile(ctx.Err() == nil, err)
}

// stalledWhile is stalled for a caller whose context is still live (live), where the caller has
// read that already.
func stalledWhile(live bool, err error) bool {
	return err != nil && live && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) && pgconn.Timeout(err)
}

// DatabaseStalled reports whether err is a stall (stalled) after which nothing the caller asked
// for can have committed, so its request may be refused as busy and sent again. Every stall a
// caller meets outside this package's pools is one: a statement inside a transaction, which
// rolls back. Two are not, and this reports false for both, so the caller answering a client
// tells it the outcome is unknown rather than offering it a retry that could write twice:
//
//   - a statement one of these pools ran outside a transaction, which commits itself, unless
//     pgconn marks it safe to retry; poolBusy names that one ErrStatementOutcomeUnknown;
//   - a COMMIT this server sent that the database never answered, which may have committed the
//     transaction; commitTimedOut names that one ErrCommitTimeout, whatever shape the stall
//     took - the commit's own bound passing, or the socket timing out under it.
func DatabaseStalled(ctx context.Context, err error) bool {
	return stalled(ctx, err) && !outcomeUnknown(err)
}

// connectionLost reports whether err is the connection to Postgres having ended under the call, in
// one of the two shapes only pgx makes, so a caller holding an error from anywhere can read it:
//
//   - pgconn.ErrConnClosed: pgconn had already closed the connection when the call took it and
//     refused the call at its lock, `conn closed` (pgconn's connLockError, which it marks safe to
//     retry, since nothing was sent), or closed it during this call and refused the read of its
//     answer. pgconn closes a connection on every failure of a call it cannot recover from - a
//     read cut off by a context or a bound (its DeadlineContextWatcherHandler puts a deadline on
//     the socket), a socket that broke, a rollback that failed (pgx's Conn.die) - so every call
//     on that connection after the one that met the failure meets this;
//   - a *pgconn.PgError of severity FATAL or PANIC: Postgres ended the session - an
//     administrator's pg_terminate_backend (57P01), the idle-in-transaction bound
//     (RequestIdleInTransactionTimeout, 25P03), a shutdown or a crash - and pgconn closes the
//     connection on it (its default OnPgError).
//
// The one call that met the broken socket itself is answered with the socket's own error, raw:
// io.ErrUnexpectedEOF, which is how pgproto3 reads the socket's EOF, or a *net.OpError for a reset
// or a closed socket (pgproto3 wraps one met while writing in a write error of its own). Those are
// not read here: the same errors come from every other socket a request touches - its client's
// body, an HTTP client's call to GitHub or the file store - and naming one of those the database's
// would offer a retry of a write that may have committed before it. A dial that failed never had
// a connection to lose (pgconn.ConnectError), and is not this either.
func connectionLost(err error) bool {
	var connect *pgconn.ConnectError
	if err == nil || errors.As(err, &connect) {
		return false
	}
	if errors.Is(err, pgconn.ErrConnClosed) {
		return true
	}
	var ended *pgconn.PgError
	if !errors.As(err, &ended) {
		return false
	}
	severity := cmp.Or(ended.SeverityUnlocalized, ended.Severity)
	return severity == "FATAL" || severity == "PANIC"
}

// lostAfterSent reports whether err is the connection ending under a call (connectionLost) that
// had been sent: pgconn marks a call it refused before sending anything safe to retry (its
// connLockError, `conn closed`), and that call cannot have run.
func lostAfterSent(err error) bool { return connectionLost(err) && !pgconn.SafeToRetry(err) }

// ConnectionLost reports whether err is a connection to Postgres that ended under a call
// (connectionLost) after which nothing the caller asked for can have committed, so its request may
// be refused as busy and sent again, as DatabaseStalled reports a stall. Every such call a caller
// meets outside this package's pools is one: a statement inside a transaction, which ends with its
// connection and commits nothing - Postgres rolls back the transaction of a backend whose client
// is gone, and the idle-in-transaction bound ends one whose socket it never hears close. Two are
// not, and this reports false for both, so the caller answering a client tells it the outcome is
// unknown rather than offering it a retry that could write twice:
//
//   - a statement one of these pools ran outside a transaction, which commits itself, after it
//     reached the database; poolBusy names that one ErrStatementOutcomeUnknown;
//   - a COMMIT this server sent before the connection ended under it; commitTimedOut names that
//     one ErrCommitTimeout.
func ConnectionLost(err error) bool {
	return connectionLost(err) && !outcomeUnknown(err)
}

// outcomeUnknown reports whether err is a call that reached the database and may have committed
// although its answer never came: a statement one of these pools ran outside a transaction
// (ErrStatementOutcomeUnknown) or a COMMIT (ErrCommitTimeout). DatabaseStalled and ConnectionLost
// answer false for both, so neither is offered as a retry.
func outcomeUnknown(err error) bool {
	return IsStatementOutcomeUnknown(err) || IsCommitTimeout(err)
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
// DatabaseStalled, ConnectionLost and IsStatementOutcomeUnknown instead (admission.WriteUnserved,
// api.writeHandlerError): a client may repeat a request, where a pass only runs again. A
// connection that ended under a pass's call is not this, even where poolBusy names it
// ErrStatementOutcomeUnknown (connectionLost): it stays a failure of the pass, as it always was. A
// caller of a read, which a lost connection leaves with nothing done, asks UnservedRead instead.
func Unserved(ctx context.Context, err error) bool {
	return IsPoolBusy(err) || stalled(ctx, err)
}

// UnservedRead reports whether err is a read the database did not serve: any call Unserved
// reports, and a connection that ended under the read (connectionLost), before it was sent or
// after it reached the database, where poolBusy names it ErrStatementOutcomeUnknown. A read commits
// nothing, so a statement of it whose answer never came leaves no outcome unknown: it is only a
// read not made, as one a busy pool refused is, and the next connection makes it.
//
// It is for a caller whose answer turns on what a read found, and which must not answer a read it
// could not make as one that found nothing: the read of a sign-in's session, which a 401 would
// answer as a person signed out (docs.Service.ServeHTTP) and a heartbeat's check as a socket to
// close at once (api's transientCheckFailure).
func UnservedRead(ctx context.Context, err error) bool {
	return Unserved(ctx, err) || connectionLost(err)
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
// database and commits itself may have run, and that one is ErrStatementOutcomeUnknown - as is
// such a statement whose connection ended under it (connectionLost). A connection that ended
// under any other call stays the error it is, which ConnectionLost reads: that call commits
// nothing, or never reached the database. Each wrap names what the pool met, so the line a
// refusal is logged on says which bound passed and what it left behind. A caller whose own
// deadline came first gets its own error back.
func poolBusy(ctx context.Context, pool string, err error, commits outcome) error {
	if err == nil {
		return nil
	}
	return poolBusyUntil(ctx.Done(), pool, err, commits)
}

// poolBusyUntil is poolBusy for a caller whose context ends when ends closes (its Done): a row or
// a batch's results map their deferred error once they are read, and keep only that channel of
// the caller's context, which says whether the context has ended as its Err does.
func poolBusyUntil(ends <-chan struct{}, pool string, err error, commits outcome) error {
	switch {
	case err == nil || errors.Is(err, ErrPoolBusy) || errors.Is(err, ErrStatementOutcomeUnknown):
		return err
	case stalledWhile(!closed(ends), err):
		switch {
		case pgconn.SafeToRetry(err):
			return fmt.Errorf("%s pool: %w: the database stopped answering before the call reached it: %w", pool, ErrPoolBusy, err)
		case commits == commitsNothing:
			return fmt.Errorf("%s pool: %w: the database stopped answering on the connection, and this call commits nothing: %w", pool, ErrPoolBusy, err)
		}
		return fmt.Errorf("%s pool: %w: %w", pool, ErrStatementOutcomeUnknown, err)
	case commits == commitsItself && lostAfterSent(err):
		return fmt.Errorf("%s pool: %w: the connection ended under it: %w", pool, ErrStatementOutcomeUnknown, err)
	case closed(ends) || !errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return fmt.Errorf("%s pool: %w: no connection came free within the pool's acquire bound: %w", pool, ErrPoolBusy, err)
}

// closed reports whether ends, a context's Done, is closed: whether that context has ended. A
// nil ends, the Done of a context that never ends, is never closed.
func closed(ends <-chan struct{}) bool {
	select {
	case <-ends:
		return true
	default:
		return false
	}
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
	// name and client make the application_name (applicationName): client is the connection
	// string's own, kept here because a pool opened from this pool's configuration finds this
	// pool's name in that configuration's application_name by then. inner is the tracer the
	// configuration had before this one was composed with it: kept so a pool opened from this
	// pool's configuration composes its own tracer with that one rather than with this.
	name   string
	client string
	bound  time.Duration
	inner  pgx.QueryTracer

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

// idleInTransactionParam is the session setting Postgres ends an idle transaction by, which the
// connections of the request, background, outbox, room-appends and settlements pools carry
// (configurePool), and the transactions of a compaction's write, the compaction sweep's removal of
// a document from its list and the room-sync catch-up's confirmation of a replaced history set for
// themselves (BoundIdleInTransaction), all at one value (RequestIdleInTransactionTimeout).
const idleInTransactionParam = "idle_in_transaction_session_timeout"

// lockTimeoutParam is the session setting Postgres refuses a statement's wait for a lock by once
// the wait has lasted that long (lock_not_available, 55P03), which the request pool's connections
// carry (configurePool) and each of its transactions sets for itself (Pool.begin), at the write
// lock bound (lockWaitDefault), and every other pool's connections carry at none
// (lockTimeoutNone).
const lockTimeoutParam = "lock_timeout"

// lockTimeoutNone is lockTimeoutParam's value that bounds no lock wait, which every pool but the
// request pool asks for as a startup parameter (configurePool).
const lockTimeoutNone = "0"

// idleInTransactionSetting is the idle bound (requestIdleInTransaction) as idleInTransactionParam's
// value: a count of milliseconds, the setting's own unit.
func idleInTransactionSetting() string {
	return strconv.FormatInt(requestIdleInTransaction.Milliseconds(), 10)
}

// configurePool names config's connections after the pool in application_name, after the
// connection string's own name where it names one (applicationName), bounds an idle transaction on
// them where every transaction of the pool can take that bound (idleInTransactionParam; the
// comment below names those pools), bounds every lock wait of the request pool's connections at
// the write lock bound and asks for no bound on every other pool's (lockTimeoutParam), and sets the
// pool's acquire tracer, bounded at bound (zero is none) and composed with any tracer config
// already carries. A configuration copied from another pool's is re-configured from that pool's
// original tracer and the connection string's own application_name, which that pool's tracer
// kept, not stacked on its acquire tracer or its name.
func configurePool(config *pgxpool.Config, name string, bound time.Duration) *acquireTracer {
	inner := config.ConnConfig.Tracer
	client := config.ConnConfig.RuntimeParams["application_name"]
	if configured := acquireTracerOf(inner); configured != nil {
		inner, client = configured.inner, configured.client
	}
	tracer := &acquireTracer{name: name, client: client, bound: bound, inner: inner}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationName(client, name)
	// An idle transaction is bounded wherever it can hold a lock a request's write waits on - an
	// issue's owner lock, a document's advisory lock - since a request's write waits for such a
	// lock up to lockWait and is then refused, as is every request queued behind it: a
	// transaction orphaned there, its connection abandoned and its close never reaching Postgres,
	// holds those locks until Postgres ends it, and this bound is what ends it. Every separate
	// pool's configuration is a copy of the request pool's (Pool.newSeparate), so each pool sets
	// the setting here or deletes it.
	//
	// The request, background, outbox, room-appends and settlements pools carry it on every
	// connection, since none of their transactions sits idle longer by design. A request's longest
	// wait is one cold read (RequestIdleInTransactionTimeout). A background writer does its slow
	// work - a GitHub or listener call, a document read - before it begins its transaction
	// (task progress, block-index and reference-excerpt reconciliation, the edit-notice sweep, a
	// pull request's heads, the delivery resume's claims and settles), and the two that wait
	// inside one wait within a bound of their own below this one: the issue ask repair's document
	// writes wait out at most one cold read at a time, as a request's do, and the fleet build
	// announcer's publishes are bounded at ten seconds each and thirty a pass. The outbox and a
	// room's appends hold a batch's few round trips. The settlements pool's two transactions both
	// take a document's lock - a settlement's pass and the block-id backfill's store of a stamp - and
	// each waits inside it for no longer than the settlement's own deadline, or its finish bound
	// once it has written into its room; nothing else on that pool opens a transaction (a lease, its
	// give-back, the removal check, the mark sweep's read and the display trees' store are statements
	// of their own), and a settlement's warm-up loads its room on the rooms pool. The compaction pool
	// carries none, though its writes take those locks: it also runs a fold's repeatable-read read,
	// which may sit idle in its transaction for longer while it folds a large stored history, so a
	// compaction's write and the compaction sweep's removal of a document from its list each bound
	// their own transaction instead (BoundIdleInTransaction). The rooms pool carries none for the
	// same reason - a load's read-only transaction there waits for a fold slot and folds a large
	// history inside it - so its one taker of a document's lock, the room-sync catch-up's
	// confirmation of a replaced history (historyReplacedUnderLock), bounds its own transaction too.
	// The live, leases, horizon, text-trees, registry, listen and health pools take nothing a
	// request's write waits on, so none of them carries it. A connection string that sets the
	// parameter itself is replaced either way.
	switch name {
	case RequestsPool, BackgroundPool, OutboxPool, RoomAppendsPool, SettlementsPool:
		config.ConnConfig.RuntimeParams[idleInTransactionParam] = idleInTransactionSetting()
	default:
		delete(config.ConnConfig.RuntimeParams, idleInTransactionParam)
	}
	// The request pool's connections wait for a lock no longer than a write's lock bound (lockWait),
	// a statement outside a transaction included: Postgres refuses a wait that runs it out with
	// lock_not_available (55P03), which a request answers 503 DATABASE_BUSY with Retry-After
	// (IsPoolBusy). An admitted request has no deadline, so a statement no bounded wait of its own
	// covered waited for as long as the lock's holder held it. Each request-pool transaction sets
	// the bound for itself too (Pool.begin), which holds whichever server connection a pooler hands
	// it, and every bounded wait keeps it and gives it back (boundedWaitKeep, boundedWaitRestore). It
	// is a startup parameter, so it is the session's reset value, which RESET ALL and DISCARD ALL
	// give back (pgmigrate.ResetSession); Postgres refuses a connection whose value it cannot take,
	// and requireLockBound refuses one whose session does not carry it, such as a session a pooler
	// opened without the parameter. Every other pool's connections ask for no bound,
	// lock_timeout = 0 (lockTimeoutNone), rather than leave the parameter out: the bound is a
	// request's, and their work keeps the waits it had. A startup parameter outranks a setting of
	// the database role or of the database (ALTER ROLE ... SET lock_timeout, ALTER DATABASE ... SET
	// lock_timeout), which a deployment behind a pooler that drops startup parameters sets for the
	// request pool's sake (cmd/dispatch/README.md), and every pool connects as the one login
	// DATABASE_URL names, so a connection that asked for nothing would wait under that setting. A
	// pooler that drops the parameter drops this one too, and the setting then reaches every pool
	// (the README says what each pool's work does when Postgres refuses its wait). A connection
	// string that sets the parameter itself is replaced either way, as the idle bound is.
	switch name {
	case RequestsPool:
		config.ConnConfig.RuntimeParams[lockTimeoutParam] = lockWaitDefault
	default:
		config.ConnConfig.RuntimeParams[lockTimeoutParam] = lockTimeoutNone
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

// lockTimeoutRead reads a connection's lock_timeout in milliseconds, the unit pg_settings gives it.
const lockTimeoutRead = `select setting from pg_settings where name = 'lock_timeout'`

// requireLockBound refuses conn, a new connection of a pool whose configuration asks for the write
// lock bound (lockWaitDefault, configurePool: the request pool's; every other pool asks for none,
// lockTimeoutNone, and is not checked), unless its session waits for a lock under a bound no
// longer than lockWait. Postgres applies a startup parameter or refuses the connection, but a
// pooler between this process and Postgres can drop the parameter and hand over a session that
// waits without limit, and its statements outside a transaction would then wait for as long as a
// lock's holder held it with nothing to say so. Such a connection is refused here, at its connect,
// so every acquire that would have taken it fails with this error, unless the database role or the
// database carries a bound of its own at or below lockWait (ALTER ROLE ... SET lock_timeout,
// cmd/dispatch/README.md). It reads on pgconn, below pgx's query tracer, so a test counting a
// request's statements never counts it, and it costs a round trip per connection, not per request.
func requireLockBound(ctx context.Context, conn *pgx.Conn) error {
	if conn.Config().RuntimeParams[lockTimeoutParam] != lockWaitDefault {
		return nil
	}
	results, err := conn.PgConn().Exec(ctx, lockTimeoutRead).ReadAll()
	if err != nil {
		return fmt.Errorf("read this connection's %s: %w", lockTimeoutParam, err)
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return fmt.Errorf("read this connection's %s: the read did not answer one value", lockTimeoutParam)
	}
	millis, err := strconv.ParseInt(string(results[0].Rows[0][0]), 10, 64)
	if err != nil {
		return fmt.Errorf("read this connection's %s: %w", lockTimeoutParam, err)
	}
	if millis <= 0 || millis > lockWait.Milliseconds() {
		return fmt.Errorf("this connection's session has %s = %dms (0 is no bound), where the pool asks for %s: "+
			"the startup parameter did not reach Postgres, and nothing would bound a lock wait outside a transaction; "+
			"let the parameter through, or set %s on the database role at or below %s",
			lockTimeoutParam, millis, lockWaitDefault, lockTimeoutParam, lockWaitDefault)
	}
	return nil
}
