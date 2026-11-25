package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This file holds the pools of its own a Pool opens beside the one it wraps - their sizes and the
// budget those sizes partition (ConnectionsPerProcess), the accessor each caller reaches its pool
// through, the opening itself and what the metrics read off every pool that is open. The guard,
// the pin and the statement wrappers are in pool.go.

// openedPool is one pgx pool a Pool has open, under its name, with its acquire tracer and the raw
// pool its callers reach it through where it is handed out whole (RawPool).
type openedPool struct {
	name    string
	pool    *pgxpool.Pool
	acquire *acquireTracer
	raw     *RawPool
}

// RawPool is a separate pool handed out whole, as the pgx pool it is: the rooms pool a document
// load opens its own transaction on, the leases pool a maintenance pass holds its lease on for a
// whole pass, the listen pool whose connection is hijacked, the horizon pool the event log's safe
// horizon batches its observations on, the registry's and the health probe's.
// Each needs what only a pgx pool gives, which is why these are not wrapped (Pool.wrapped) and why
// their callers stand outside the nested-acquire guard: a separate pool cannot close the cycle the
// guard prevents.
//
// Every way it hands out a connection answers an acquire that ran out the pool's bound with
// ErrPoolBusy, as a wrapped pool does (Pool.busy), so its callers tell a pool with nothing free
// from a deadline of their own by IsPoolBusy. Its other methods - Close, Config, Stat - are the
// pgx pool's own and take no connection.
type RawPool struct {
	*pgxpool.Pool
	name string
}

// busy maps a call of this pool's that a bound of the server's ended to ErrPoolBusy, or to
// ErrStatementOutcomeUnknown where the call is a statement that commits itself (poolBusy).
func (r *RawPool) busy(ctx context.Context, err error, commits outcome) error {
	return poolBusy(ctx, r.name, err, commits)
}

// Acquire takes a connection the caller holds until it releases it: the lease a maintenance pass
// holds for its whole pass, and the connection Listener hijacks.
func (r *RawPool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := r.Pool.Acquire(ctx)
	return conn, r.busy(ctx, err, commitsNothing)
}

// AcquireFunc runs fn with a connection of the pool. Only the acquire's error is mapped: fn's own
// is returned as fn returned it.
func (r *RawPool) AcquireFunc(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	conn, err := r.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return fn(conn)
}

// Query runs a query on a connection of the pool, which the rows hold until they close.
func (r *RawPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := r.Pool.Query(ctx, sql, args...)
	return rows, r.busy(ctx, err, commitsItself)
}

// QueryRow runs a single-row query on a connection of the pool. pgxpool defers an acquire's error
// to the row's Scan, so the row maps it there.
func (r *RawPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return busyRow{Row: r.Pool.QueryRow(ctx, sql, args...), ends: ctx.Done(), pool: r.name}
}

// Exec runs a statement on a connection of the pool.
func (r *RawPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	tag, err := r.Pool.Exec(ctx, sql, args...)
	return tag, r.busy(ctx, err, commitsItself)
}

// SendBatch runs a batch on a connection of the pool. pgxpool defers an acquire's error to the
// results, so the results map it there.
func (r *RawPool) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	return busyBatchResults{BatchResults: r.Pool.SendBatch(ctx, batch), ends: ctx.Done(), pool: r.name}
}

// CopyFrom streams rows into a table on a connection of the pool.
func (r *RawPool) CopyFrom(
	ctx context.Context,
	table pgx.Identifier,
	columns []string,
	source pgx.CopyFromSource,
) (int64, error) {
	copied, err := r.Pool.CopyFrom(ctx, table, columns, source)
	return copied, r.busy(ctx, err, commitsItself)
}

// Begin opens a transaction of its own on a connection of the pool. Unlike a wrapped pool's it is
// the pgx transaction itself: it commits on its caller's context, not apart from it
// (CommitContext), since these pools' transactions are the loads and registrations that belong to
// no request.
func (r *RawPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := r.Pool.Begin(ctx)
	return tx, r.busy(ctx, err, commitsNothing)
}

// BeginTx opens a transaction with options, as Begin opens one.
func (r *RawPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := r.Pool.BeginTx(ctx, options)
	return tx, r.busy(ctx, err, commitsNothing)
}

// waitingForLocksBegin is the statement a transaction that waits for its locks without a bound
// opens with (BeginWaitingForLocks): BEGIN, then SET LOCAL lock_timeout at none (lockTimeoutNone),
// as renderBegin renders a request-pool transaction's at its bound. readOnlyWaitingForLocksBegin
// is the same for a read-only transaction: the boot's probe of the database (Pool.HealthyWithin).
var (
	waitingForLocksBegin         = renderBegin(pgx.TxOptions{}, lockTimeoutNone)
	readOnlyWaitingForLocksBegin = renderBegin(pgx.TxOptions{AccessMode: pgx.ReadOnly}, lockTimeoutNone)
)

// BeginWaitingForLocks opens a transaction as Begin does, which waits for every lock it takes
// without a bound, whatever lock_timeout its session carries: its BEGIN sets lock_timeout to none
// for the transaction alone (SET LOCAL), in the one simple-protocol message pgx sends an
// argument-less statement as, so it costs no round trip more than the BEGIN, as a request-pool
// transaction's bound costs none (requestBegin). The pool's connections ask for no bound already
// (configurePool), but a pooler that drops startup parameters drops that request too, and the
// database role's lock_timeout, which cmd/dispatch/README.md has such a deployment set for the
// request pool's sake, then reaches the session; a transaction's own setting holds on whatever
// server connection the pooler hands it. It is for a transaction whose refused wait would end the
// process: the boot's registration (processes.Register). The boot's probe of the database opens
// its read-only transaction the same way (Pool.HealthyWithin).
func (r *RawPool) BeginWaitingForLocks(ctx context.Context) (pgx.Tx, error) {
	return r.BeginTx(ctx, pgx.TxOptions{BeginQuery: waitingForLocksBegin})
}

// Ping takes a connection and asks the database to answer on it.
func (r *RawPool) Ping(ctx context.Context) error {
	return r.busy(ctx, r.Pool.Ping(ctx), commitsNothing)
}

// SharedPoolSize bounds the shared pool - the request pool - deliberately and in code. pgx's
// default, max(4, NumCPU), is four on production's one Fargate task at cpu="512", and a
// connection per blocked writer is all it takes to starve everything else: with four writers
// parked on one issue's row lock, a read of an unrelated issue waited the whole seventeen seconds
// the lock was held, where sixteen connections served 3,853 such reads at a four-millisecond
// median. The ceiling is picked here rather than derived from the task's CPU allotment or read
// out of the DSN another repository's URL builder writes.
//
// It is 48 of one connection budget of 118 (ConnectionsPerProcess) that every pool of a process
// shares, partitioned rather than grown: requests 48, background 6 (Background), live 4 (Live),
// settlements 4 (Settlements), rooms 4, the rooms' appends 8 (RoomAppends), health 1, the five
// listener connections (the listener pool's one dial is one of them redialling), leases
// LeasePoolSize (29), outbox 2, compaction 2 (Compaction), the display tree fill 2 (TextTrees),
// the event horizon 2 (Horizon) and the
// server process registry's own 1 (registryPoolSize). The partition
// itself added nothing: it is the 87 the process opened before, the request pool having given up
// 16, which background, live, settlements and the leases' two spare connections took; the event
// horizon's 2, the background loops' shared wake listener, the outbox's second connection, the
// outbox's own listener, the compaction's 2, the display tree fill's 2, the eight lease names
// added since (the CI reconcile pass's, the CI verify loop's, the settlement resumption's, the
// embeddings project_key fill's, the display tree fill's, the Legion sampler's, the parent
// status fix's and the reference forms drain's), the four
// callers of the delivery
// intakes' holds (duty.Hold, leaseCallerConnections) and the keyword fill's two, its own
// goroutine's and its hold's renewal's (textversions.ReserveLeases), are the
// only connections added since, and the
// horizon's came with the pool itself. The rooms' appends are the one pool added over the budget
// rather than out of it: the 8 they take are the connections a room's browser updates no longer
// take one request connection at a time, which is why the budget is 118 and not 110. A write
// takes a connection per statement one after another, and a read pins one for its whole length
// (PinConnection); an issue read, which once took six statements, two more when the issue had
// documents, two more when it had open asks, and one per artifact, now takes two batches. So
// under load the API holds every connection of this pool; the background
// loops, the live-update readers and the settlements wait on pools of their own, so neither side
// can starve the other, and an acquire here waits at most requestAcquireBound before it is
// refused with ErrPoolBusy rather than queueing without end. Admission derives the units it may
// hand out at once from this number, the connections left to the pool's uncounted users taken off
// (admission.MaxAdmittedUnits), so a change here moves that cap with it; the connections left stay
// as many as admission names (admission.OutsideLimit), and three kinds of those users, the DPoP
// decisions made before admission, the websockets' heartbeat checks and the document sockets'
// admission reads, hold at most that many between them.
//
// The Legion sampler's lease (legion.RunSampler) and the keyword fill's two callers
// (textversions.ReserveLeases), both counted in leaseCallerConnections, were the last connections
// added before the CI verify loop's, and that is what they cost the instance: a serving process
// opened at most 115, where it opened 112 without them, and the default tier's 500 connections
// less PostgreSQL's 3 reserved
// slots still admit four app pods holding connections at once (460 of 497) and still refuse five
// (575), exactly as they did at 112 (448 and 560), so neither moves a replica boundary. Neither
// takes a share of the partition: the sampler's pass reads and writes on the background pool,
// whose 6 it shares with every other loop, and only its lease is a connection of its own; the
// keyword fill takes its hold, renews it and writes its batches on its two connections of the
// lease pool and on no other pool. The CI verify loop's lease (cireconcile.VerifyLease) is
// one more: 116, four pods 464 of 497 and five refused at 580, so it
// moves none either, and it takes no share of the partition: a verify pass answers its requests
// from GitHub, and only its lease is a connection of its own. R25's boot fix of parents' statuses
// (api.RunParentStatusFixAtBoot, lease parent-status-fix) is one more: 117, four pods 468 of 497
// and five refused at 585, so it moves none either, and it takes no share of the partition: its
// read of every issue and the transaction of each step run on the background pool, and only its
// lease is a connection of its own. The reference forms drain's lease (duty.RefsFormsLease,
// docs.Service.RunRefsFormsReconciliation, beside the block index on a goroutine of its own) is
// the last connection added: 118, four pods 472 of 497 and five refused at 590, so it moves none
// either, and it takes no share of the partition: its reads and each drain's transaction run on
// the background pool, as the block index's do, and only its lease is a connection of its own.
//
// The database bounds the budget from above. Dispatch's database is the gcp:Dispatch component's
// Cloud SQL Postgres instance, whose default max_connections follows its tier's memory
// (https://cloud.google.com/sql/docs/postgres/flags, max_connections), and every server pod opens
// up to ConnectionsPerProcess, the budget. During a roll, every pod that holds connections (the
// replicas, the surge pod and pods still terminating, for up to their grace period) times that
// sum must fit in max_connections less PostgreSQL's 3 superuser-reserved slots, so raising a pool
// or the pod count is checked against it: read SHOW max_connections on the instance before
// raising either. A new pool, or a new permanent Listener caller, goes into
// ConnectionsPerProcess with it.
const SharedPoolSize = 48

// RoomsPoolSize bounds the connections document loads use, and the room-sync catch-up's
// confirmation of a replaced history under the document's lock (Rooms). Loading takes no lock and
// always finishes. The confirmation waits for a document's lock, but the catch-up lane runs one at
// a time, so it holds at most one connection, and one more is enough for the pool to be
// deadlock-free; a few let cold rooms load concurrently. Measured on the real server, eight cold
// rooms loading at once are no faster at eight connections than at one, so this is small on
// purpose.
const RoomsPoolSize = 4

// roomAppendsPoolSize bounds the connections the document rooms' browser updates are stored on
// (RoomAppends). Two kinds of work take one. The browser batch path: a room's queued browser
// updates are stored together, in one transaction, so a room holds a connection for its batch's
// few round trips and starts at most one batch per linger window, and a handful of connections
// serve many rooms writing at once. And a close for a closed issue, which writes its room's latest
// edit source to the document's pending-settlement row once per close (persistLastActor in
// internal/dispatch/docs). Eight is twice the rooms pool and a sixth of the request pool: every
// connection here is one more in the process's budget against the database's limit, so it is
// small on purpose, and a room that finds all eight held waits for another batch to commit.
//
// It cannot deadlock. A batch holds its connection and the document's advisory lock and takes no
// other connection (HoldsConnection stays on its context, so a read of the request pool under it
// is refused), a close's write takes its connection before the lock and nothing after it, and no
// holder of a document lock ever waits for a connection of this pool: settlements, live writes
// and the other withRoomLock callers take theirs where they always have, so every holder of a
// lock a batch or a close's write can wait on finishes without one of these connections.
const roomAppendsPoolSize = 8

// healthPoolSize bounds the connections the health probe uses. The probe answers one question -
// is Postgres reachable, and at which schema - and asks it on a connection nothing else can take,
// so one is not a ration but the whole design.
const healthPoolSize = 1

// healthProbeTimeout bounds the health probe's own wait, dial and query together, and owns the
// argument for why the probe is bounded at all. Every prober treats silence as a dead process:
// the ALB fails a poll at five seconds and three consecutive failures replace the task,
// cancelling every in-flight request of every other client, and the compose healthcheck and
// the deploy script are tighter still at three (deploy/compose/dispatch.compose.yml,
// deploy/scripts/autodeploy.sh). Two seconds fits inside the tightest of them, so a database
// that has stopped answering comes back as a 503 they can read.
//
// Nothing else bounds it usefully. A cold dial is floored at two minutes, not unbounded -
// pgxpool sets ConnectTimeout to two minutes whenever the config leaves it at zero, under
// pgx's own "ensure that a connect won't hang forever", and pgconn applies that to the whole
// connection process - but two minutes is forty times the tightest deadline above. Once the
// connection is up nothing bounds the query at all, because an http.Server request context
// carries no deadline, and warm is production's normal state: the probe runs every few seconds
// and an idle connection lives for half an hour. A link that drops packets - a security-group
// change, an availability-zone partition, an endpoint that accepts and then stalls - is the
// unbounded case whenever the connection was already open.
const healthProbeTimeout = 2 * time.Second

// ErrPoolClosed is a connection asked for after the pool was released.
var ErrPoolClosed = errors.New("connection pool is closed")

// Rooms is where a document load takes its connection. A load runs while the request that
// triggered it holds an open transaction on this pool - it happens when an anchored write
// stamps its mark in a room nobody has opened yet - and it is deliberately not part of that
// transaction, because the loaded room outlives the request and must not roll back with it. A
// second connection from the shared pool is exactly what deadlocks it (ErrNestedAcquire), so
// the loads have their own, opened on demand and closed with this pool: one per store, not one
// per caller who wants to read a document. Its acquires are bounded at requestAcquireBound like
// the request pool's, and a load that waits that bound out is refused with ErrPoolBusy (RawPool).
//
// The room-sync catch-up confirms here, under the document's lock, that a room's stored history
// was replaced before it closes the room (historyReplacedUnderLock in internal/dispatch/docs).
// That is no request's work, so it waits for the lock until the holder lets go, on a connection
// that carries no lock bound, where the request pool's would refuse it at the write lock bound;
// RoomsPoolSize says why the loads keep their connections. Its transaction bounds its own idle
// time (BoundIdleInTransaction), since every request writing the document waits on that lock; the
// pool's connections carry no idle bound, because a load's read-only transaction here may sit
// idle for longer while it waits for a fold slot or folds a large history (configurePool).
func (p *Pool) Rooms() (*RawPool, error) {
	return p.separate(RoomsPool, RoomsPoolSize)
}

// RoomAppends is where a document room stores its browser updates: a batch of one room's queued
// updates takes a connection here, holds it and the document's advisory lock for one transaction,
// and gives it back (roomAppendsPoolSize says why that cannot deadlock). On a pool of their own
// the appends wait for no request and hold no connection a request could use, and a request
// waits for none of them. Its acquires are bounded at backgroundAcquireBound: no client's answer
// waits on a batch, and what does wait on one - a settlement, a shutdown's drain, another
// process's room query - waits for an update's durability, which is better waited for than lost
// to a refusal that fails the room. A close for a closed issue writes its room's latest edit
// source here too (persistLastActor in internal/dispatch/docs): it is the room's own record going
// durable, and it waits for the document's lock until the holder lets go, as a batch does.
func (p *Pool) RoomAppends() (*RawPool, error) {
	return p.separate(RoomAppendsPool, roomAppendsPoolSize)
}

// Healthy reports the highest schema migration Postgres has applied, read within
// healthProbeTimeout on a connection of a pool nothing else uses; the read is the reachability
// probe too, so an error means the database did not answer. /healthz asks it, and it must never
// queue behind a writer: proving the shared pool has a free connection is the wrong question,
// because during a busy period it legitimately has none. Like the rooms pool this one is outside
// the nested-acquire guard, because a separate pool cannot close the cycle the guard prevents.
//
// The version is read from schema_migrations on every call rather than remembered from this
// process's own Migrate, so it reports what the database holds: a deploy check compares it with
// the highest migration in the commit the image was built from.
//
// It answers while Close waits for the connections still in use, so a caller waiting on Close
// (cmd/dispatch's shutdown) can tell a database still finishing work from one that has stopped
// answering; once Close has closed every other pool it closes this one and Healthy refuses
// (ErrPoolClosed).
func (p *Pool) Healthy(ctx context.Context) (int, error) {
	health, err := p.healthPool()
	if err != nil {
		return 0, err
	}
	// Derived from the caller's context, so a request the client already gave up on ends with
	// it instead of holding the probe's one connection for the full bound.
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	return scanSchemaVersion(health.QueryRow(ctx, schemaVersionRead))
}

// HealthyWithin is Healthy with a bound the caller names, for the one caller whose read is not
// the serving process's: it reads the highest schema migration Postgres has applied, within
// bound, on a connection of a pool nothing else uses, and an error means the database did not
// answer. healthProbeTimeout is argued for a warm process - /healthz runs every few seconds on a
// connection this pool already holds open - and it must stay tight, because the probers that
// read a slow answer as a dead process are tighter still. The boot's one probe
// (cmd/dispatch/registry.go) is the opposite case: the health pool opens on its first read
// (healthPool), so that read always pays a fresh dial, and two seconds bounds the query where it
// has to bound the dial as well. A caller that is always cold names a bound that covers one.
//
// Its read waits for a lock under bound alone, whatever lock_timeout the session carries: it runs
// in a read-only transaction whose BEGIN sets lock_timeout to none for the transaction alone
// (readOnlyWaitingForLocksBegin, in BeginWaitingForLocks' form), at two round trips more than
// Healthy's read. The read waits for a lock only behind one on schema_migrations, such as the
// migration runner's alter of its version table (prepareVersionTable). Behind a pooler that drops
// startup parameters, the database role's lock_timeout, which cmd/dispatch/README.md has such a
// deployment set for the request pool's sake, reaches this pool and would refuse that wait
// (lock_not_available) and end the boot (cmd/dispatch's joinRegistry), where with no such
// setting the read waits out its bound. Healthy keeps its one round trip, since its own two
// seconds end such a wait first.
func (p *Pool) HealthyWithin(ctx context.Context, bound time.Duration) (int, error) {
	health, err := p.healthPool()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	tx, err := health.BeginTx(ctx, pgx.TxOptions{BeginQuery: readOnlyWaitingForLocksBegin})
	if err != nil {
		return 0, err
	}
	// A no-op once the transaction has committed.
	defer func() { _ = tx.Rollback(context.Background()) }()
	version, err := scanSchemaVersion(tx.QueryRow(ctx, schemaVersionRead))
	if err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// schemaVersionRead is the health probes' read of the highest schema migration Postgres has
// applied (Healthy, HealthyWithin). The cast holds the result at bigint whichever type the column
// has: a connection that prepared this read while schema_migrations.version was an integer would
// otherwise fail it once after a boot widens the column ("cached plan must not change result
// type").
const schemaVersionRead = "select coalesce(max(version), 0)::bigint from schema_migrations"

// scanSchemaVersion is the version schemaVersionRead's row answers.
func scanSchemaVersion(row pgx.Row) (int, error) {
	var version int
	if err := row.Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// listenerPoolSize bounds the pool Listener and Dedicated take their connections from. Five
// connections dial through it - the delivery hub's and the event stream relay's listeners, each
// held only while something subscribes to them, the events outbox's listener, the process's
// shared listener hub (pglisten.Hub, on which the document rooms' room sync and cmd/dispatch's
// woken background loops are registered, one connection for all of their channels) and the
// document rooms' NOTIFY sender - and a sixth, the test hooks' fan-out
// (api/test_hooks_fanout.go), under DISPATCH_TEST_HOOKS alone, which production never sets. Each
// connection is hijacked as soon as it is opened, so it leaves with its caller and the pool holds
// none between dials: what one bounds is dials in flight, not connections held. A caller
// redialling while another dials waits that dial out, which is what a loop that dials and then
// waits for notifications for the life of the process can afford, and what the sender's first
// send after a failure can afford too.
const listenerPoolSize = 1

// Listener returns one connection of its own for LISTEN, from a one-connection pool opened from the
// shared pool's configuration and hijacked, so it holds no shared connection. The caller closes it.
// The events outbox (outbox/lease.go) holds one for the life of the process, as does the test
// hooks' fan-out (api/test_hooks_fanout.go) under DISPATCH_TEST_HOOKS; the delivery hub
// (api/delivery_live_hub.go) and the event stream relay (api/event_stream_relay.go) each hold one
// while something subscribes to them, and close it when the last subscriber leaves. A connection
// of the shared pool held that long would be one fewer for every request, and a LISTEN on a
// pooled connection would end when the pool recycled it. A dial that does not get the pool's
// connection within listenAcquireBound is refused with ErrPoolBusy.
func (p *Pool) Listener(ctx context.Context) (*pgx.Conn, error) {
	listen, err := p.separate(ListenPool, listenerPoolSize)
	if err != nil {
		return nil, err
	}
	conn, err := listen.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Hijack(), nil
}

// Dedicated returns one connection of its own, as Listener does, that pg_stat_activity names
// applicationName. The caller closes it. It is outside the nested-acquire guard
// (ErrNestedAcquire) for the reason the other separate pools are: a connection that belongs to no
// request-pool caller cannot close the cycle the guard prevents, so a request holding one of the
// request pool's transactions may use one. Two such connections are held per process: the shared
// listener hub that cmd/dispatch dials, whose name is how every Dispatch process reads which
// processes are listening (docs.ListenerApplicationPrefix), and the document rooms' NOTIFY sender
// (docs/room_sync.go), which a request inside a transaction sends through.
func (p *Pool) Dedicated(ctx context.Context, applicationName string) (*pgx.Conn, error) {
	conn, err := p.Listener(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `select set_config('application_name', $1, false)`, applicationName); err != nil {
		// Bounded, so a connection that went half-open between its dial and this statement
		// cannot hold the caller in its Terminate.
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
		return nil, fmt.Errorf("name the connection %s: %w", applicationName, err)
	}
	return conn, nil
}

// leaseCallerConnections is how many callers of the lease pool can be using one of its
// connections at the same moment: the callers the once-overall leases (duty.Exclusive), the
// delivery intakes' holds and the keyword fill's hold (duty.Hold) are taken by, one of which runs
// the keyword fill's batches too. It is a caller and not a lease or a hold that needs one.
//
// Exclusive takes its connection before it learns whether the lease is held, so a pass that skips
// because the other process holds the lease holds a connection for that round trip too. These
// names are leased - the document compaction, the keyed writes' maintenance sweep
// (api.RunMaintenanceSweep, which keeps the lease name suggestion-outcomes the suggestion sweep
// carried that work under), the reference-excerpt reconciliation, the block-index reconciliation,
// the reference forms drain (duty.RefsFormsLease, beside the block index on a goroutine of its
// own), the term-statistics refresh, the GitHub webhook redelivery sweep, the CI reconcile pass
// (cireconcile.Lease), the CI verify loop (cireconcile.VerifyLease, taken only by a pass with a
// verify request to answer, beside a reconcile pass that holds its own for minutes), the
// architecture import, the delivery reconcile, the edit-notice sweep, the
// document settlement resumption (docs.RunSettlementResumption), the repair of closed and reopened
// issues' asks (api.RunIssueAskRepair), one per delivery durable for the intake's take-over
// (delivery.consumerLease, a name each for delivery-events and delivery-jobs), the server process
// registry's prune (processes.RunPrune), the embeddings project_key fill
// (api.RunEmbeddingProjectKeyFill), the display tree fill (texttreefill.Run), the Legion
// sampler's read of each project's daemon (legion.RunSampler, one pass a minute, one process at a
// time) and R25's one-time fix of parents' statuses at boot (api.RunParentStatusFixAtBoot, lease
// parent-status-fix, taken only with DISPATCH_PARENT_STATUS_RULE on and reserved whether or not
// it is) - one caller each,
// since each intake takes its own durable's name from its own goroutine, so each of them can be
// inside Exclusive at once. The embeddings fill is the one that is no loop of a cadence: it labels
// in a single pass of one to two hours, holds its connection for the whole of it, and gives the
// connection back for good once the rows stored before migration 20261109120000 are labelled, so
// it is counted here like any other caller. The suggestion outcome sweep is not among them: it
// takes no lease, since the cursor row each of its pages holds for update serialises two processes
// by itself (api/suggestion_outcomes.go).
//
// Each of the two intakes also holds its durable's hold, whose every statement is one statement
// on a connection of this pool for that statement alone. Two can be in flight per intake at once:
// the hold's renewal goroutine (duty.Hold's renew, every 5 s, each bounded by the renew interval),
// and the intake's own goroutine (duty.HoldToken, duty.TryHold, SetToken, Release, one at a time),
// whose SetToken runs inside the take-over and so needs a connection beside the one its lease is
// held on. That is four more callers.
//
// The keyword fill (textversions.Run) holds a hold of its own, search-text-versions-fill, for as
// long as its process fills the keyword table. Its own goroutine takes and releases the hold and
// runs the fill's batches and reads on this pool, one statement or one batch's transaction at a
// time, and the hold's renewal goroutine renews it beside them, so a renewal never waits behind a
// batch: that is two more callers (textversions.ReserveLeases), twenty-seven in all.
//
// Nothing counts the callers by hand. Each of them reserves one of these connections by name
// before the server starts any of its loops, whether or not this configuration runs that loop
// (LeaseCallers.For, cmd/dispatch/main.go), and the server refuses to serve unless the
// reservations are exactly leaseCallerConnections (LeaseCallers.Complete). So a leased loop or a
// hold added without a connection here, and a connection added here without a caller, stops the
// process at its start rather than leaving a renewal to wait behind a pass that runs for minutes.
//
// The check sees only callers that reserve a name. For hands every name the same pool, so a
// goroutine added inside a caller that has reserved already, and one reservation passed to a
// second consumer, take a connection nothing counted, and the count still matches: each caller
// that can hold a connection while another does needs a reservation of its own.
const leaseCallerConnections = 27

// leaseSpareConnections is the lease pool's connections beyond its callers', for what holds one
// without being a reserved caller's statement: pgxpool's own health check and lifetime expiry
// take an idle connection while they ping or replace it, and a pass whose unlock failed holds its
// connection until the close that ends its session (duty.FreeSessionLock, leaseCallTimeout).
// Without them the next pass, or a hold's renewal, waits on those (review BG-5). Three such
// holders at once leave the next caller waiting, and an acquire that waits leaseAcquireBound is
// refused (ErrPoolBusy): a leased pass skips its tick (duty.Exclusive) and a renewal is tried again
// on the hold's next one.
const leaseSpareConnections = 2

// LeasePoolSize bounds the pool the once-overall leases, the delivery intakes' holds and the
// keyword fill's hold are taken on, and the keyword fill's batches run on: a connection for each
// caller that can be using one at the same moment (leaseCallerConnections), and two spare
// (leaseSpareConnections). They cannot come from the
// request pool: a lease is held for the whole pass, which is minutes for the compaction sweep and
// hours for the first display tree fill of a display parser version, and the request pool's
// connections are what every request reads through. The compaction sweep and the display tree
// fill hold only their lease here: each reads and writes on a pool of its own (Pool.Compaction,
// Pool.TextTrees).
const LeasePoolSize = leaseCallerConnections + leaseSpareConnections

// Leases is the registry of the lease pool's callers. A periodic maintenance loop takes the
// connection it holds its lease on here, so that only one process runs each pass
// (duty.Exclusive): the lease is a session-level advisory lock, so it lives on one connection for
// the length of the pass and dies with it; a connection of the shared pool held that long would
// be one fewer for every request, and a pass that then read through the shared pool - which is
// what a pass does - would be a caller holding two of its connections (ErrNestedAcquire). The
// delivery intakes and the keyword fill take and renew their holds here too (duty.Hold), so a
// renewal never waits behind a request for a shared connection, and the keyword fill writes its
// batches here beside its hold. Its acquires are bounded at leaseAcquireBound, and a
// caller that waits that bound out is refused with ErrPoolBusy (RawPool).
//
// The pool itself is not handed out: a caller reaches a connection only through the reservation
// it takes by name (For), which is what ties leaseCallerConnections to the callers the process
// really wires. Every call returns the one registry this pool keeps, so every caller counts in the
// same place.
func (p *Pool) Leases() (*LeaseCallers, error) {
	leases, err := p.separate(LeasesPool, LeasePoolSize)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	if p.leaseCallers == nil {
		p.leaseCallers = &LeaseCallers{pool: leases}
	}
	return p.leaseCallers, nil
}

// LeaseCallers is the lease pool handed out as reservations, one per caller that can be inside a
// lease or a hold statement at the same moment. leaseCallerConnections is one connection per such
// caller, and a reservation is how a caller says it is one of them.
//
// What it counts is the reservations taken, not the goroutines that use them: For hands every
// name the same pool, so a caller that reserves once and then runs a second goroutine on the
// reservation, and a reservation passed to a second consumer, are both outside what Complete can
// see.
type LeaseCallers struct {
	pool *RawPool
	// mu guards the reserved names: the server takes them one at a time from the goroutine
	// that wires it, and a caller reserving from a goroutine of its own must not be lost from
	// the count.
	mu       sync.Mutex
	reserved []string
}

// For reserves a connection of the lease pool for the caller named name and returns the pool it
// takes that connection from: a *RawPool is the duty.Leases a lease is taken on and the
// duty.Holds a hold's statements run on. Every caller that can be inside a lease or a hold
// statement while another is reserves a name of its own - one intake's hold is two callers, its
// renewal goroutine and the intake's own - and a name reserved already is refused, since two
// callers cannot share one connection.
func (c *LeaseCallers) For(name string) (*RawPool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if slices.Contains(c.reserved, name) {
		return nil, fmt.Errorf("%q has reserved a connection of the lease pool already: each caller that can hold one while another does reserves a name of its own", name)
	}
	c.reserved = append(c.reserved, name)
	return c.pool, nil
}

// ForAll reserves a connection of the lease pool for each caller of names, in order (For), and
// returns the pool they take them from: a group of callers that work together, such as a hold's
// holder and its renewal goroutine, reserves them here at once. The error of a name For refuses
// names that caller, and the names before it stay reserved. No name at all is refused too, since
// a caller reaches the pool only through a reservation.
func (c *LeaseCallers) ForAll(names ...string) (*RawPool, error) {
	if len(names) == 0 {
		return nil, errors.New("reserve the lease pool: no caller named")
	}
	for _, name := range names {
		if _, err := c.For(name); err != nil {
			return nil, fmt.Errorf("reserve the lease pool for %s: %w", name, err)
		}
	}
	return c.pool, nil
}

// Reserved is the names that have reserved a connection, in the order they took it.
func (c *LeaseCallers) Reserved() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reserved)
}

// Complete answers whether the callers reserved are exactly the callers the lease pool holds a
// connection for (leaseCallerConnections), and names them when they are not. The server calls it
// once every reservation is taken and before it starts any loop, and refuses to serve on its
// error: a caller short of a connection waits for one behind the longest pass, which is minutes
// for the compaction, and a connection short of a caller is one the process opens and nothing
// uses.
func (c *LeaseCallers) Complete() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reserved) == leaseCallerConnections {
		return nil
	}
	return fmt.Errorf("%d callers reserved a connection of the lease pool and it holds one for %d callers and %d spare: it holds one connection per caller that can be inside a lease or a hold statement at the same moment, so a caller added here is a connection added in store.LeasePoolSize; reserved: %s",
		len(c.reserved), leaseCallerConnections, leaseSpareConnections, strings.Join(c.reserved, ", "))
}

// outboxPoolSize bounds the pool the events outbox reads and records events through, and takes
// and renews its publisher hold on (duty.Hold). The outbox's scan is one goroutine that runs one
// statement at a time and closes each cursor before its next statement, and the hold's renewal
// runs beside it on a goroutine of its own, so two connections are every connection it can use: a
// renewal never waits behind a batch's statement, which may take up to the five seconds a batch's
// record is allowed, five times the hold's one second between renewals.
const outboxPoolSize = 2

// registryPoolSize bounds the pool the registry of live server processes stamps this process's row
// through: processes.Register opens it with OpenRegistry and its Deregister closes it, so each
// registration opens a pool of its own, which Stats reports while it is open.
const registryPoolSize = 1

// listenerConnections is how many connections the listener pool dials and hands off at once in
// production: the events outbox's LISTEN on dispatch_events and dispatch_duty_holds
// (outbox/lease.go) and the document rooms' two (Dedicated) - the shared listener hub that room
// sync and cmd/dispatch's woken background loops are registered on, and the NOTIFY sender, which
// opens at the first send and then stays open - each held for the life of the process, and the
// delivery hub's and the event stream relay's, each held only while something subscribes to them.
// A process nobody streams from and no Delivery page is open on holds three of the five; the
// count is what it may hold at once, which is why it is no longer named for permanence. The
// outbox's LISTEN is its own rather than a registration on the hub, as the delivery hub's and the
// relay's are: cmd/dispatch builds the outbox only with NATS, and the LISTEN starts and stops
// inside outbox.Outbox.Run, while the hub takes no registration once it runs. The test hooks'
// fan-out (api/test_hooks_fanout.go) holds a sixth under DISPATCH_TEST_HOOKS alone, which
// production never sets, so it is not counted here.
const listenerConnections = 5

// ConnectionsPerProcess is the most connections one Dispatch server process opens to Postgres, the
// budget of 118 SharedPoolSize describes: every pool's size, the registry's own pool
// (processes.Register) and the event horizon's (Horizon) among them, and one connection per
// listener it may hold at once, which the listener pool dials and hands off rather than holds (its
// one dial in flight is one of those listeners redialling, so it is not counted twice).
const ConnectionsPerProcess = SharedPoolSize + backgroundPoolSize + livePoolSize + settlementsPoolSize +
	RoomsPoolSize + roomAppendsPoolSize + healthPoolSize + LeasePoolSize + outboxPoolSize +
	compactionPoolSize + textTreePoolSize + horizonPoolSize + registryPoolSize + listenerConnections

// Outbox is the pool the events outbox reads and records events through, reads the followers and
// authors a publish routes to, and holds its publisher hold on. Under load the API's requests
// queue for the shared pool's connections, and an outbox statement queued behind them holds back
// every event behind it, or lets the hold lapse; on a pool of its own it waits for none of them,
// and holds no connection a request could use.
func (p *Pool) Outbox() (*Pool, error) {
	return p.wrapped(OutboxPool, outboxPoolSize)
}

// compactionPoolSize bounds the pool a document's compaction reads and writes through. A
// compaction holds one connection at a time - its read transaction, then, after a fold that holds
// none, its write under the document's lock - so one would serve one compaction; two let a second
// compaction (a room closing while another compacts) read while the first writes.
const compactionPoolSize = 2

// Compaction is where a document's compaction takes its connections: the repeatable-read
// transaction it reads the rows to fold in, and the connection it holds the document's advisory
// lock on while it writes the fold back. Compaction is housekeeping. On the request pool its
// write would hold a connection every request reads through while it waits for the document's
// lock, on the background pool it would hold one of the six every background loop shares for the
// length of a fold, and on the rooms pool its read would queue a room's load behind a log it is
// folding; on a pool of its own it takes none of them. Like the rooms pool it is handed out raw
// and its callers stand outside the nested-acquire guard, because a separate pool cannot close
// the cycle the guard prevents. Its acquires are bounded at backgroundAcquireBound, since nobody
// waits on a compaction, and one that waits that bound out is refused with ErrPoolBusy (RawPool),
// which leaves the document to the compaction queue's next try.
func (p *Pool) Compaction() (*RawPool, error) {
	return p.separate(CompactionPool, compactionPoolSize)
}

// textTreePoolSize bounds the pool the background display tree fill reads and writes through
// (texttreefill). A pass reads its next batch of texts only once the batch before it has been
// stored, and stores a batch with at most two readings in flight (texttreefill.fillWorkers,
// which is fewer on a pod with fewer than three cores), so two connections are every connection
// it can use.
const textTreePoolSize = 2

// TextTrees is where the background display tree fill takes its connections: the two scans that
// choose a batch, the reads that take its texts back by key, and the inserts of the trees. The
// fill is housekeeping that runs for hours - its first pass of a display parser version ends only
// once every stored text has a tree - so on the request pool it would hold a connection every
// request reads through for that whole time, on the background pool it would hold one of the six
// every background loop shares, and on the compaction pool it would queue a document's fold
// behind a batch of markdown readings. Unlike the rooms and compaction pools it is wrapped and
// so inside the nested-acquire guard, because the fill reaches it through this type and stores
// its trees through texttrees.Table, which takes a *Pool: the fill drains each batch's cursor
// before it stores anything, so it never asks for a second connection while it holds one. Its
// acquires are bounded at backgroundAcquireBound, since nobody waits on the fill.
func (p *Pool) TextTrees() (*Pool, error) {
	return p.wrapped(TextTreesPool, textTreePoolSize)
}

// horizonPoolSize bounds the pool the event log's safe horizon (events.Horizon) works through. The
// server keeps one Horizon, whose concurrent callers share the one observation in flight, so one
// connection serves every observation. The other statements the Horizon runs share both
// connections with it: the stall diagnosis and the write of the proven horizon (event_horizon),
// which run after an observation has released its waiters, and the reads every GET /auth/whoami
// and the relay's first start make (a mark, the oldest transaction that can write the event log,
// the persisted horizon), each one statement.
const horizonPoolSize = 2

// Horizon is the pool the event log's safe horizon (events.Horizon) takes its observations, its
// marks and the reads beside them, its stall diagnosis and its persist write through. Every
// reader that pages the event log by a global id cursor - the event stream relay, the delivery
// hub, a references read since a cursor - reads only up to what an observation has proven safe.
// Under load the API's requests queue for the shared pool's connections, and an observation
// queued behind them holds back every live reader of the event log; on a pool of its own it waits
// for none of them, and holds no connection a request could use. Like the rooms pool it is handed
// out raw and its callers stand outside the nested-acquire guard, because a separate pool cannot
// close the cycle the guard prevents: none of those statements (each one batch or one row) asks
// for a second connection while it holds one, and a page's whoami takes one while its request
// holds nothing else. Its acquires are bounded at requestAcquireBound, since a page's load waits
// on one, and an acquire that waits that bound out is refused with ErrPoolBusy (RawPool).
func (p *Pool) Horizon() (*RawPool, error) {
	return p.separate(HorizonPool, horizonPoolSize)
}

// backgroundPoolSize bounds the pool for work no request waits on: background loops such as the
// embedding queue and its coverage pass, the suggestion sweep, the delivery reconcile and intakes,
// the architecture importer's ticker, the documents' resumption query, task progress,
// reference-excerpt and block-index reconciliation (its object pass writes the nodes of the rows
// another build wrote with none and states gone the orphaned document nodes, each batch in a
// transaction of its own), the reference forms drain (docs.Service.RunRefsFormsReconciliation:
// its listing, each text's and each document's index and its count), the edit-notice sweep, the
// repair of closed and
// reopened issues' asks, the boot's check that every project has an owner and its grants
// (EnsureProjectOwners), the Legion sampler's reads of its settings and records of each daemon's
// state (legion.RunSampler), the server process registry's prune and the fleet build announcer, and
// what a request leaves running behind its answer: the comment mention sender's claim statements
// (api/comment_mention_sender.go) and the one retry of a related_checks or near_copy_checks row
// its first attempt could not write (api's retryRecord, at most four at once). Six is the
// background's share of the budget SharedPoolSize describes; nobody waits on these loops, so they
// wait for each other here (up to backgroundAcquireBound) instead of for the requests'
// connections.
const backgroundPoolSize = 6

// Background is the pool for background loops and the work a request leaves behind it, so that
// under load a request never queues behind one of their statements for a connection, nor they
// behind the requests.
func (p *Pool) Background() (*Pool, error) {
	return p.wrapped(BackgroundPool, backgroundPoolSize)
}

// livePoolSize bounds the pool for the live-update readers, which are the event stream relay's
// one read loop and the first cursor of each generation it starts, the delivery hub's intake
// (one statement at a time), its worker's recompute (at most concurrentReadWeight at once) and
// its seed of the open P0 issues at a generation's start, and an events subscription's bounded
// replay, its backlog count and its page (api/live_events.go). Four is their share of the budget
// SharedPoolSize describes: the relay's read and the hub's intake and recompute are what read
// continuously, while the seed runs once per generation and a replay once per resume, each one
// statement waiting at most requestAcquireBound for a connection.
const livePoolSize = 4

// Live is the pool for the live-update readers, so an event reaches the open streams while the
// requests hold every connection of theirs; its acquires are bounded at requestAcquireBound, since
// a stream is waiting on each one.
func (p *Pool) Live() (*Pool, error) {
	return p.wrapped(LivePool, livePoolSize)
}

// settlementsPoolSize bounds the pool for document settlements. Four is the settlements' share of
// the budget SharedPoolSize describes.
const settlementsPoolSize = 4

// Settlements is the pool for document settlements, so a burst of settlements - every room a
// shutdown or a timer settles - neither waits for the requests' connections nor takes them. The
// block-id backfill a subcommand runs stores each document's stamp here too, as a settlement
// stores a repair (backfillBlockIDs in internal/dispatch/docs), one document at a time: it waits
// for the document's lock until the holder lets go, where the request pool's write lock bound
// would refuse it. Its connections carry the request pool's idle-in-transaction bound
// (RequestIdleInTransactionTimeout, configurePool): its two transactions, a settlement's pass and
// the backfill's store, both take the document's lock every request writing the document waits
// on, and neither sits idle longer by design; its other work is statements outside any
// transaction, and a settlement's warm-up loads its room on the rooms pool.
func (p *Pool) Settlements() (*Pool, error) {
	return p.wrapped(SettlementsPool, settlementsPoolSize)
}

// wrapped is the separate pool name, wrapped so the nested-acquire guard and the ErrPoolBusy
// mapping apply to it as they do to the request pool.
func (p *Pool) wrapped(name string, size int32) (*Pool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return &Pool{pool: opened.pool, name: opened.name, acquire: opened.acquire}, nil
}

// SetAcquireBoundForTest sets the acquire bound the separate pool named name opens with, in place
// of its own (defaultAcquireBound). It is for tests outside this package alone, which shorten a
// bound so a full pool refuses in milliseconds rather than production's seconds; nothing in
// production calls it, since every pool's bound is set in code. A pool already open keeps its bound.
func (p *Pool) SetAcquireBoundForTest(name string, bound time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.acquireBounds == nil {
		p.acquireBounds = make(map[string]time.Duration)
	}
	p.acquireBounds[name] = bound
}

// SetOpenFailureForTest makes the separate pool named name fail to open with err, in place of
// opening it. It is for tests outside this package alone, which see what a caller does when the
// pool it works through cannot be opened; nothing in production calls it, since a pool opens from
// this pool's own configuration. A pool already open fails the same way, so what the test sees
// does not depend on what opened the pool first.
func (p *Pool) SetOpenFailureForTest(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openFailures == nil {
		p.openFailures = make(map[string]error)
	}
	p.openFailures[name] = err
}

// DropLockTimeoutParameterForTest makes the separate pool named name open without the
// lock_timeout startup parameter configurePool gives it (lockTimeoutParam), as a pooler that
// drops startup parameters opens its connections, so they carry whatever lock_timeout the
// database role or the database sets. It is for tests outside this package alone, which see what
// that pool's work does under such a setting; nothing in production calls it, since Postgres
// takes the parameter wherever nothing between drops it. A pool already open keeps its parameter.
func (p *Pool) DropLockTimeoutParameterForTest(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lockTimeoutDropped == nil {
		p.lockTimeoutDropped = make(map[string]bool)
	}
	p.lockTimeoutDropped[name] = true
}

// separate is the raw pool of the separate pool named name (open): its pgx pool with the acquires
// its callers make mapped to ErrPoolBusy.
func (p *Pool) separate(name string, size int32) (*RawPool, error) {
	opened, err := p.open(name, size)
	if err != nil {
		return nil, err
	}
	return opened.raw, nil
}

// open opens the pool named name on demand, sized at size (newSeparate), and closes it with this
// pool. It refuses from the moment Close begins (ErrPoolClosed); the health probe's pool, whose
// close rule is its own, is opened through healthPool instead. Each caller holds mu for the whole
// check-and-open so two of them cannot open two pools, and one opened after Close would be a pool
// nobody closes.
func (p *Pool) open(name string, size int32) (openedPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return openedPool{}, ErrPoolClosed
	}
	return p.openLocked(name, size)
}

// healthPool is open for the health probe's pool, whose close rule is its own: it opens and is
// handed out while Close waits for the other pools' connections, and refuses once Close has
// closed every other pool (healthClosed), so Healthy answers for as long as Close waits.
func (p *Pool) healthPool() (*RawPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.healthClosed {
		return nil, ErrPoolClosed
	}
	opened, err := p.openLocked(HealthPool, healthPoolSize)
	if err != nil {
		return nil, err
	}
	return opened.raw, nil
}

// openLocked is the pool named name, opened when this pool has none under that name. Its caller
// holds mu and has checked the close rule that applies to the pool it asks for.
func (p *Pool) openLocked(name string, size int32) (openedPool, error) {
	if err, failing := p.openFailures[name]; failing {
		return openedPool{}, err
	}
	if opened, ok := p.separates[name]; ok {
		return opened, nil
	}
	opened, err := p.newSeparate(context.Background(), name, size)
	if err != nil {
		return openedPool{}, err
	}
	p.recordLocked(opened)
	return opened, nil
}

// newSeparate opens one separate pool: size connections copied from this pool's configuration and
// configured as its own (configurePool: its application_name and its acquire bound, acquireBounds'
// where a test set one, else defaultAcquireBound's), with no lock_timeout startup parameter where
// a test dropped it (lockTimeoutDropped). The copy keeps the shared pool's tracer, which
// configurePool composes the pool's own acquire tracer with, so its connections give back a large
// statement's arguments on release too (argumentReleaser). Its callers hold mu, which is what
// keeps two of them from opening two pools under one name and an open from racing this pool's
// close; pgx opens no connection here, so nothing waits on the database under the lock.
func (p *Pool) newSeparate(ctx context.Context, name string, size int32) (openedPool, error) {
	config := p.pool.Config()
	config.MaxConns = size
	// Both minimums, not just MinConns: a floor inherited from the shared pool's DSN would be
	// a target these pools cannot reach - pool_min_idle_conns=2 on a one-connection health
	// pool - which pgxpool re-attempts, and puddle refuses as full, every health-check period.
	config.MinConns = 0
	config.MinIdleConns = 0
	bound, ok := p.acquireBounds[name]
	if !ok {
		bound = defaultAcquireBound(name)
	}
	acquire := configurePool(config, name, bound)
	if p.lockTimeoutDropped[name] {
		delete(config.ConnConfig.RuntimeParams, lockTimeoutParam)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return openedPool{}, fmt.Errorf("open %s pool: %w", name, err)
	}
	return openedPool{name: name, pool: pool, acquire: acquire, raw: &RawPool{Pool: pool, name: name}}, nil
}

// recordLocked keeps opened among this pool's separate pools, in place of one already under its
// name, so Stats reports it and Close closes it. Its callers hold mu.
func (p *Pool) recordLocked(opened openedPool) {
	if p.separates == nil {
		p.separates = make(map[string]openedPool, len(poolNames))
	}
	p.separates[opened.name] = opened
}

// Close releases every connection the pool holds, every separate pool's included, and returns
// once every connection in use has come back. It closes the health probe's pool last, after every
// other pool has closed: Healthy answers for as long as Close waits, and Stats and Use keep
// counting the closing pools' connections. No other separate pool opens or is handed out once
// Close has begun.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	// The registry goes with the pool its reservations were on: a caller after the close is
	// told the pool is closed rather than handed a reservation on a pool nothing serves.
	p.leaseCallers = nil
	separates := make([]openedPool, 0, len(p.separates))
	for name, separate := range p.separates {
		if name != HealthPool {
			separates = append(separates, separate)
		}
	}
	p.mu.Unlock()
	for _, separate := range separates {
		separate.pool.Close()
	}
	p.pool.Close()
	p.mu.Lock()
	p.healthClosed = true
	health, open := p.separates[HealthPool]
	p.separates = nil
	p.mu.Unlock()
	if open {
		health.pool.Close()
	}
}

// OpenRegistry opens the server process registry's own pool, which processes.Register uses and its
// Deregister closes: this pool's configuration sized at registryPoolSize and named as the
// registry's (configurePool, RegistryPool), so its connection is dispatch-registry in
// pg_stat_activity and its acquires are counted apart from the requests'. The pool is recorded
// with the others, replacing one an earlier registration opened, so Stats and the metrics report
// the heartbeat's connection and its waits as they report every other pool's, and Close closes it
// with them; closing it twice is harmless, since a pgx pool closes once.
func (p *Pool) OpenRegistry(ctx context.Context) (*RawPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrPoolClosed
	}
	opened, err := p.newSeparate(ctx, RegistryPool, registryPoolSize)
	if err != nil {
		return nil, err
	}
	p.recordLocked(opened)
	return opened.raw, nil
}

// opened lists this pool and every separate pool it has opened, in poolNames order.
func (p *Pool) opened() []openedPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	opened := make([]openedPool, 1, 1+len(p.separates))
	opened[0] = openedPool{name: p.name, pool: p.pool, acquire: p.acquire}
	for _, name := range poolNames {
		if separate, ok := p.separates[name]; ok {
			opened = append(opened, separate)
		}
	}
	return opened
}

// Stats reports, for this pool and every separate pool it has opened, the pool's connection
// counts and what its acquire tracer has counted: what the metrics a scrape reads are made of.
func (p *Pool) Stats() []PoolStats {
	opened := p.opened()
	stats := make([]PoolStats, len(opened))
	for i, pool := range opened {
		stats[i] = PoolStats{Name: pool.name, Stat: pool.pool.Stat()}
		if pool.acquire != nil {
			stats[i].Acquire = pool.acquire.stats()
		}
	}
	return stats
}

// RoomPoolStats is the document-rooms and the room-appends pools' Stat, each nil while its pool
// has not been opened: reading them opens neither, so a reader of the room figures costs no
// connection.
func (p *Pool) RoomPoolStats() (rooms, roomAppends *pgxpool.Stat) {
	p.mu.Lock()
	roomsPool, roomsOpen := p.separates[RoomsPool]
	appendsPool, appendsOpen := p.separates[RoomAppendsPool]
	p.mu.Unlock()
	if roomsOpen {
		rooms = roomsPool.pool.Stat()
	}
	if appendsOpen {
		roomAppends = appendsPool.pool.Stat()
	}
	return rooms, roomAppends
}

// ConnectionUse is how many connections of a set of pools are in use, how many those pools have
// taken back since they opened, whether returned or discarded, and how many of those came back
// finished: handed back open and idle, so the work on them ended (acquireTracer.TraceRelease). A
// connection discarded because the database stopped answering - its statement cut off by its
// context, or its ping on acquire unanswered - counts in Returned and not in Finished.
type ConnectionUse struct {
	InUse    int32
	Returned int64
	Finished int64
}

// Use reports the ConnectionUse of the pools named, of those this pool has opened, or of every
// pool it has opened when no name is given. A caller asking whether work in flight has finished
// names the pools that work runs on and leaves the health probe's out: the probe is how a caller
// asks whether the database answers, and its connection taken back says nothing about the work. A
// pool opened without the acquire tracer counts no connection finished.
func (p *Pool) Use(names ...string) ConnectionUse {
	var use ConnectionUse
	for _, pool := range p.opened() {
		if len(names) > 0 && !slices.Contains(names, pool.name) {
			continue
		}
		stat := pool.pool.Stat()
		use.InUse += stat.AcquiredConns()
		use.Returned += stat.AcquireCount() - int64(stat.AcquiredConns())
		if pool.acquire != nil {
			use.Finished += int64(pool.acquire.finished.Load())
		}
	}
	return use
}
