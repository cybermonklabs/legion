// # Safe horizon
//
// Event ids come from one sequence, events_id_seq, and a transaction draws its id when it inserts
// its row, which becomes visible only when the transaction commits. Two writes that do not wait
// for each other can therefore commit out of id order: the write that drew id 7 can still be open
// after the write that drew id 8 has committed. A reader that pages the log by a global id cursor
// (id > cursor), read 8 and moved its cursor there would never read 7. Such a reader reads only
// up to the safe horizon H, the highest id at or below which every event is final: committed and
// visible to every later statement, or rolled back for good. A Horizon computes H from Postgres's
// own transaction bookkeeping.
//
// An observation is one statement in a transaction of its own, sent as one batch
// (horizonObserveSQL). It yields A, the newest id visible to the statement's snapshot S, the
// created_at of the event at A (which persisting H stamps it with), F, a transaction id below which
// no event writer was running as the statement ran (below), the xmax of S and whether S lists any
// transaction in progress, and X, the observer's own transaction id, which pg_current_xact_id
// assigns while the statement runs, after S was taken.
//
// An event writer is a transaction that holds RowExclusiveLock on events in this database. An
// insert into events takes that lock on the table when it is parsed (setTargetTable in
// parse_clause.c) or, for a prepared statement, when its cached plan is locked before it runs
// (AcquireExecutorLocks and AcquirePlannerLocks in plancache.c): before its column default calls
// nextval for its row's id. It keeps the lock until its transaction ends, even when the
// transaction also holds a stronger lock on events: pg_locks lists one row for each mode a
// transaction holds. A lock of another mode alone (VACUUM's, ANALYZE's or CREATE INDEX
// CONCURRENTLY's ShareUpdateExclusiveLock, ALTER TABLE's AccessExclusiveLock) draws no event id
// and makes no writer. Every transaction also holds an ExclusiveLock on its own transaction id, and
// one on each id its subtransactions took, until it ends. Postgres releases a transaction's locks
// only after it has left the proc array (CommitTransaction and AbortTransaction in xact.c), so
// once its locks are gone its commit is visible to every later statement, or it has rolled back.
//
// A is final once no event writer that holds a transaction id below X is still running. Take any
// transaction T that owns an id g <= A and had not committed when S was taken. T is an event
// writer of this database: it inserted g into this database's events, and the insert's column
// default drew g, so T took its write lock before nextval handed it g and holds it until it ends.
// No id is drawn, and no row of events written, any other way: no Go statement of the module
// names events_id_seq but to read with pg_catalog.currval the id its own insert's default just
// drew (store.LatestEventID), which draws none, or in the offline seed tool's setval; a bare or
// otherwise qualified currval could resolve to a function of another schema that draws, so it is
// refused, and so is every nextval or setval but a call of one string literal of a sequence's name
// (one of digits alone is an OID), and their C code under any name, since each can draw from
// events_id_seq without spelling it. No Go source or migration runs SQL a text guard cannot read
// (execute of a built string, query_to_xml, cursor_to_xml), since such a query can draw the
// sequence without naming it. No insert lists an explicit id, no source but that tool
// copies or merges rows into events, no statement makes a table from events, no migration the
// server runs names the sequence, in a function, view, trigger or default, or draws from a
// sequence it does not spell, and the schema they build gives no other table events' id default,
// events no child or partition, whose rows would be rows of events written under another table's
// lock, and no function of a sequence function's name, oid or C code but Postgres's own (held by
// the name's pg_proc.dat oid, in pg_catalog and the fixed internal language oid).
// TestOnlyAnEventInsertsDefaultDrawsAnEventID and TestOnlyTheBrokerInsertsEvents
// (internal/dispatch/api) hold the Go half, TestNoMigrationDrawsAnEventIDOutsideTheInsertDefault
// (internal/dispatch/store) the migrations' text, and
// TestOnlyTheEventsIDDefaultDrawsFromItsSequence (internal/dispatch/store) the migrated catalog.
// T held its transaction id before nextval handed it g: that is the writer invariant, which
// Broker.Append must keep by taking its transaction id before it inserts.
// nextval handed out g before it handed out A, because events_id_seq has CACHE 1, so every
// backend draws from the one shared counter in time order. A's row had committed before S was
// taken, and X was assigned after S. So xid(T) < X, and once no event writer holding an id below X
// is running, T has ended: its row is visible to every later statement, or it rolled back and g is
// a gap for good.
//
// No other transaction can hold such a g. A transaction of another database on the instance
// cannot write this database's events, and a transaction of this database that holds no write
// lock on events holds no event id: any it draws later, nextval hands it after S, above A. So
// neither holds H back: not another service's work on the same Cloud SQL instance, another
// database's CREATE or DROP DATABASE, an operator's open psql session, nor a Dispatch transaction
// that has not yet inserted its event.
//
// An observation is proven by a later one, or by its own snapshot (below). A later observation's
// F' is the greater of two such bounds. One is the xmin of its snapshot S', the oldest transaction
// id still running on the whole instance when S' was taken: no transaction below it was running at
// all. The other is read from the lock
// table, once (pg_locks, in a materialized CTE, so every row comes from one call): the oldest
// transaction id among the event writers it lists, or the observer's own X' when it lists none.
// A writer's id is the least of the ids it holds an ExclusiveLock on under its own
// virtualtransaction, which is its top transaction id: a subtransaction takes an id only after its
// parent has one. Each is widened from the 32 bits pg_locks gives to the 64 bits of
// pg_current_xact_id before the least is taken, since a subtransaction's id can lie past a
// multiple of 2^32 that its top id lies below. T's write lock and the lock on its own id are both
// granted from before it drew g until it ends, so no event writer holding an id below that bound
// was running when the lock table was read. Observations of one Horizon run one at a time, so X'
// is above the X of every observation it checks. F' > X therefore means no event writer holding an
// id below X was running, and proves the earlier observation safe. H is the A of the newest
// observation a later one proved safe this way, and it only grows until Reset.
//
// The statement reads the lock table only when it is needed: when the xmin of S' does not already
// pass the X of the newest observation still pending. On a quiet instance the xmin proves every
// pending observation at the cost of a snapshot; the lock table decides only while some
// transaction elsewhere, in this database or another, holds the xmin back.
//
// A write lock whose transaction id the read does not list makes the lock table's bound 0, which
// proves nothing beyond the xmin. A transaction takes its RowExclusiveLock when its statement is
// parsed (parse_clause.c, setTargetTable) or its cached plan locked (plancache.c), and its
// transaction id only when it first writes a tuple (heapam.c, heap_insert and heap_update call
// GetCurrentTransactionId), so a writer whose transaction had no id yet holds the lock undated in
// between, and an UPDATE that matched no row holds it undated until its transaction ends. Such a
// holder holds H back only when the lock table is read: an observation whose snapshot's xmin
// already passes the newest pending observer's X does not read it, and an observation that proves
// itself from its own snapshot never reads it. That is safe by the writer invariant alone: a
// transaction that has drawn an event id took its transaction id first (Broker.Append does, and
// the guards above hold every writer of the module to it), so an undated holder holds no event id
// yet, and any it draws later is above every id observed so far. A writer that drew an id before
// its transaction had one, outside the module, is outside this proof. When the lock table is
// read, it cannot place an undated holder, so it proves nothing while one stands. A prepared
// transaction is always dated: MarkAsPreparingGuts gives its dummy proc the preparing backend's
// virtualtransaction (twophase.c), so its write lock and the lock on its id carry one
// virtualtransaction while PostPrepare_Locks moves them and after.
//
// S proves its own A when it leaves no room for a running id below X: xip(S) is empty and
// xmax(S) = X. A snapshot lists every id running at or above its xmin and below its xmax in its
// xip, and every id below xmin has ended, so an empty xip means every id below xmax(S) had ended
// when S was taken. An id at or above xmax(S) can be running unlisted, but with xmax(S) = X the
// range [xmax(S), X) is empty, so no id there is below X either. Every transaction id below X, an
// event writer's or any other's, had therefore ended before S was taken, and A is final at once:
// the observation records H = A without waiting for a later one. Its own F never proves it, since
// F is at most its own X. An observation that does not meet both conditions waits for a later
// one.
//
// Both conditions are needed, and the xmax hole is why. A snapshot's xmax is one past the newest
// transaction id that had completed, and a transaction holding an id at or above xmax is not
// listed in its xip at all (Postgres documents that "there might be in-progress IDs higher than
// xmax"). Say T2 takes transaction id 100, T takes 101 and inserts event 7, then T2 inserts event
// 8 and commits. A snapshot taken now has xmin = xmax = 101, lists nothing in progress and sees
// 8, while T is still open holding 7 and its write lock. The observer's own X, assigned after the
// snapshot, is what closes that hole: it is at least 102, above xmax = 101, so the empty xip
// proves nothing, the shortcut does not fire, and no later observation proves it safe until T
// ends.
//
// # Cadence
//
// An observation whose own snapshot proves it needs no other, so an id a reader sees commit on a
// quiet instance is final in the read that saw it. Otherwise it is proven by the next observation
// at the earliest, and only by one that ran after every event writer holding an id below the
// first observer's had ended. Gap tells a reader which of the two it waits for. When the newest
// observation is the only one unproven (GapUnproven), every event writer that held back the ones
// before it had ended when it ran, so observing again at once proves it unless an event writer
// holding an id below its own is still running: the reader observes again at once rather than
// after a delay. When an older observation is still unproven (GapHeld), an event writer holding
// an id below that observation's own was still running when the newest observation ran, and
// observing again proves nothing until that writer ends: the reader waits for a commit to be
// announced, which a Dispatch write that appends events does (pg_notify on dispatch_events), with
// a bounded fallback for one that ends unannounced. Await runs the same cadence with its backoff
// as that fallback.
//
// # Costs
//
// H trails the longest event write open in this database: Dispatch's own appends, or an
// operator's insert that takes its transaction id before it draws its event id. That delays live
// readers only; it blocks no writer. A
// pending observation older than five seconds logs a warning naming the oldest such writer, so a
// stall can be found. An observation that fails logs its error, and an Await whose every
// observation has failed for ten seconds returns that error to its caller rather than wait on.
//
// Each observation consumes one transaction id, which brings anti-wraparound vacuum sooner. One
// that reads the lock table costs more: pg_locks visits every backend's fast-path locks and every
// lock partition of the instance. Measured as a prepared statement on PostgreSQL 18.6 with
// max_connections 1200, an observation took 0.54 ms on an idle instance and 3.0 ms beside 300 busy
// clients when it read the lock table, and 0.10 ms and 0.24 ms when it did not, about what an
// observation cost when it read only the snapshot (0.10 ms and 0.19 ms).
// A Horizon observes only when a caller asks it to (Safe, Await) and runs no timer of its own, so a
// process whose readers are idle consumes none, and concurrent callers share one observation.
// pg_locks needs no privilege: every role sees every backend's locks.
//
// # Primary only
//
// Observations, and every read capped at H, run on the primary. A hot standby refuses
// pg_current_xact_id, and a standby that has not yet replayed a committed row would serve a read
// at or below H without it.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// horizonPendingLimit bounds the observations waiting for a later one to prove them safe. An
	// entry is 48 bytes, so the list stays under 48 KiB. Writes that overlap without a break keep
	// every observation unproven for D, the length of the open transactions they overlap. While
	// fewer than horizonPendingLimit observations come in D, every one is kept, and H reaches each
	// as soon as a later observation proves it. Past that, thinPending spreads the kept observations
	// evenly over D rather than starve the newest, so H trails by at most about
	// 4D/horizonPendingLimit more.
	horizonPendingLimit = 1024
	// Await's re-observation backoff while an older observation is held (GapHeld): 10 ms doubling
	// to 250 ms. An unproven newest observation alone (GapUnproven) is observed again at once.
	horizonBackoffMin = 10 * time.Millisecond
	horizonBackoffMax = 250 * time.Millisecond
	// horizonAwaitFailFor is how long Await keeps re-observing while every observation fails
	// before it returns the last one's error; one observation that succeeds starts it again. Its
	// long-lived caller (the event relay, at the start of each generation of it) waits on a
	// lifetime, not a request, so without it an observation that never succeeds (a
	// horizon pool that cannot dial, say) would leave it waiting for ever with no error to log
	// or retry on. Ten seconds is forty retries at Await's longest backoff: long enough to ride
	// out a redial, short enough that the caller's own warning and retry come soon.
	horizonAwaitFailFor = 10 * time.Second
	// A pending observation older than horizonStallAfter logs a warning, at most once every
	// horizonWarnEvery, so a transaction held open for minutes names itself without flooding
	// the log.
	horizonStallAfter = 5 * time.Second
	horizonWarnEvery  = 30 * time.Second
	// StallWarning is the message warnStall logs. A test in another package (cmd/dispatch)
	// matches it to tell "the server is waiting on a transaction" from "the server is wedged",
	// so rewording it is a contract change, which that test catches.
	StallWarning = "dispatch: event horizon held back by an open transaction; live event readers wait for it"
	// horizonDiagnosisTimeout bounds the stall warning's query of the event writers
	// (StallHolderSQL), which runs after the observation and must never hold up a reader for long.
	horizonDiagnosisTimeout = 2 * time.Second
	// horizonPersistEvery bounds how often a Horizon that persists (PersistProven) tries to write
	// its H to event_horizon: at most once a second, by the observation that finds H above what it
	// wrote, whether the last try succeeded or failed.
	horizonPersistEvery = time.Second
	// horizonPersistTimeout bounds one such write.
	horizonPersistTimeout = 2 * time.Second
)

// persistHorizonSQL writes H ($1) to event_horizon with $2, the created_at of the event at H as the
// observation that saw it read it, keeping the higher value, unless the row holds a value of a log
// since reset (no event at its safe carries its safe_at), which H replaces. It writes only while
// the event at H still carries $2: after a reset of the log, the row at that id, if any, is a new
// log's, whose created_at is not the one observed, and an H of the old log is never stamped as a
// value of the new one.
const persistHorizonSQL = `
	insert into event_horizon (id, safe, safe_at)
	select 1, e.id, e.created_at from events e where e.id = $1 and e.created_at = $2
	on conflict (id) do update set safe = excluded.safe, safe_at = excluded.safe_at, updated_at = now()
	where event_horizon.safe < excluded.safe
		or not exists (select 1 from events e where e.id = event_horizon.safe and e.created_at = event_horizon.safe_at)
`

// persistedHorizonSQL reads the persisted H when it is a value of the log as it stands now.
const persistedHorizonSQL = `
	select coalesce((select h.safe from event_horizon h
		join events e on e.id = h.safe and e.created_at = h.safe_at
		where h.id = 1), 0)
`

// HorizonDB is what a Horizon needs from the store: the shared pool satisfies it.
type HorizonDB interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row // the stall diagnosis, WriterFloor and Persisted
}

// Horizon tracks the safe horizon of the event log (see the package documentation's "Safe
// horizon" section): one per process, shared by every reader that pages the log by a global id
// cursor. It is safe for concurrent use.
type Horizon struct {
	db HorizonDB

	// now, stallAfter, warnEvery and logger are the stall and failure warnings' clock, thresholds
	// and sink, and now is also the clock Await measures horizonAwaitFailFor by; NewHorizon sets
	// the production values and a test replaces them. A nil logger is slog.Default at the time of
	// the warning.
	now        func() time.Time
	stallAfter time.Duration
	warnEvery  time.Duration
	logger     *slog.Logger
	// pause is Await's backoff wait, which returns once d has passed or ctx has ended; NewHorizon
	// sets pauseFor and a test replaces it to see the waits Await takes.
	pause func(ctx context.Context, d time.Duration)
	// afterObservationParked runs, outside the lock, each time a caller finds an observation
	// already in flight and parks on it rather than running one of its own, and
	// afterParkedObservationEnded each time that observation has ended, before the caller looks
	// again. Nothing in production sets them; the package's own tests of concurrent callers wait
	// on the first instead of sleeping for a share of them to arrive, and hold callers on the
	// second to make an observation end before they look.
	afterObservationParked      func()
	afterParkedObservationEnded func()
	// afterObservationBoundary runs, outside the lock, after Safe or Await has read its call
	// boundary and before it calls observe. Nothing in production sets it; the package's tests
	// hold a caller here to prove it can take an observation that began after its call.
	afterObservationBoundary func()

	mu sync.Mutex
	// generation counts Resets. An observation records only into the generation it started in,
	// so one in flight across a Reset cannot bring back what the Reset forgot.
	generation uint64
	// started counts observations ever started and is never reset: an observation's number is
	// its order, and Safe and Await each wait for one numbered above the count when called.
	// inFlight is the observation running now, if any, and ended the newest one that has ended
	// without being abandoned (run records it): an abandoned observation is no answer a live
	// caller may take, so it does not replace ended. One observation runs at a time, so every
	// observation that has ended is numbered at or below the newest that started.
	started  uint64
	inFlight *horizonFlight
	ended    *horizonFlight
	// safe is H. safeThrough is the number of the newest observation H covers: proven safe, or
	// with an A at or below H when recorded or later. Both are this generation's.
	safe        int64
	safeThrough uint64
	// safeAt is the created_at of the event at safe, as the observation that saw it read it.
	safeAt time.Time
	// newest is the highest A observed this generation (Gap), and recorded the number of the newest
	// observation recorded this generation, which tells GapUnproven from GapHeld.
	newest   int64
	recorded uint64
	// pending holds the observations still waiting for a later one to prove them safe, oldest
	// first, each with an A above safe.
	pending []horizonObservation
	// warnedAt is when the last stall warning was logged, and failWarnedAt when the last failed
	// observation was.
	warnedAt     time.Time
	failWarnedAt time.Time
	// diagnosing is whether a warnStall is in flight. Its read of the event writers runs on h.db,
	// which for a server is the horizon pool (horizonPoolSize, store/pool.go): the two
	// connections the horizon's own observations use. One diagnosis is bounded by
	// horizonDiagnosisTimeout and cannot overlap the next at the production warnEvery, but a
	// test process runs warnEvery an order of magnitude below that bound, and several
	// diagnoses at once would queue for those two connections, each queued acquire burning
	// its context into a diagnosis_error warning that names no holder, with the horizon's own
	// observation - the one the waiting reader needs - behind them. So one at a time.
	diagnosing bool
	// persist is whether H is written to event_horizon as it advances (PersistProven);
	// persistedSafe is the H last written this generation, persistedAt when the last write was
	// tried, succeeded or failed, and persisting whether a write is under way. persistWarnedAt is
	// when a failed write was last logged.
	persist         bool
	persistedSafe   int64
	persistedAt     time.Time
	persisting      bool
	persistWarnedAt time.Time
}

// horizonObservation is one observation: its number, A, X, whether its own snapshot proved it
// (see the package documentation's "Safe horizon" section: an empty in-progress list and an xmax
// equal to X) and when it was recorded.
type horizonObservation struct {
	number uint64
	newest int64
	// newestAt is the created_at of the event at newest as the observation read it, zero when the
	// log was empty: what persisting an H of it stamps (persistHorizonSQL).
	newestAt   time.Time
	xid        uint64
	selfProven bool
	at         time.Time
}

// horizonFlight is the one observation in flight, which concurrent callers share. Its fields
// are written by the caller running it before done closes and read by the others after.
type horizonFlight struct {
	number     uint64
	generation uint64
	done       chan struct{}
	err        error
	// abandoned is an observation that failed because its runner's context ended; a waiter
	// whose own context is still live runs another rather than return a cancellation not its own.
	abandoned bool
}

// NewHorizon returns a Horizon observing the event log through db, with H at 0.
func NewHorizon(db HorizonDB) *Horizon {
	return &Horizon{
		db:         db,
		now:        time.Now,
		stallAfter: horizonStallAfter,
		warnEvery:  horizonWarnEvery,
		pause:      pauseFor,
	}
}

// pauseFor waits for d, or until ctx ends.
func pauseFor(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Gap is what a reader whose read left an observed id above H waits for (Horizon.Gap).
type Gap int

const (
	// GapNone: every observed id is at or below H, and there is nothing to re-read for.
	GapNone Gap = iota
	// GapUnproven: an observed id stands above H, and the newest observation, which saw it, is
	// the only one unproven: it proved every observation before it. Observing again at once
	// proves it unless an event writer holding a transaction id below its own is still running.
	GapUnproven
	// GapHeld: an observation older than the newest is still unproven, so an event writer holding
	// a transaction id below that observation's own was still running when the newest one ran.
	// Observing again proves nothing until that writer ends.
	GapHeld
)

// PersistProven has the Horizon write its H to event_horizon as it advances, at most once every
// horizonPersistEvery, so a process that starts later can read the log above it (Persisted). Call it
// before the Horizon is shared.
func (h *Horizon) PersistProven() { h.persist = true }

// SetStallWarning replaces two thresholds: stallAfter, how long a pending observation waits
// before warnStall logs it, and warnEvery, how often every warning this Horizon logs repeats -
// the stall warning while the stall stands, the failed-observation warning of run, and the
// failed-persist warning of writePersisted, which hold their own last-warned times but share
// this one rate. A zero argument leaves that threshold as it is, so a caller may set either
// alone and one that sets neither keeps horizonStallAfter and horizonWarnEvery. Call it before
// the Horizon is shared.
//
// A test process needs shorter ones: warnedAt is one field of the one Horizon per process, shared
// with the delivery hub's event seed, so at the production horizonWarnEvery a warning logged for
// the hub can hold the next one back for 30 s, and a test that keys on the warning would then see
// no signal while a transaction genuinely held the horizon.
func (h *Horizon) SetStallWarning(stallAfter, warnEvery time.Duration) {
	if stallAfter > 0 {
		h.stallAfter = stallAfter
	}
	if warnEvery > 0 {
		h.warnEvery = warnEvery
	}
}

// Persisted answers the H some process persisted (PersistProven), when it is a value of the event
// log as it stands now, and 0 when none is. Every event at or below it is final: finality is
// permanent, whichever process proved it. It runs one statement and waits on no transaction.
func (h *Horizon) Persisted(ctx context.Context) (int64, error) {
	var safe int64
	if err := h.db.QueryRow(ctx, persistedHorizonSQL).Scan(&safe); err != nil {
		return 0, fmt.Errorf("read the persisted event horizon: %w", err)
	}
	return safe, nil
}

// Safe runs one observation that starts after the call (shared with concurrent callers), checks
// the pending observations against it and returns H, the highest id at or below which every
// event is final (0 before any observation has been proven safe).
//
// Like Await, Safe never settles for an observation already in flight when it is called: that
// one took its snapshot before the call and can miss an id committed just before it, and if its
// A were at or below H it would leave no gap (Gap), so the reader would arm no re-read for that
// id. Safe waits it out and joins or runs the next. Its own observation saw every id committed
// before the call, so each such id is at or below H, or stands above it and keeps a gap open
// until H reaches it.
func (h *Horizon) Safe(ctx context.Context) (int64, error) {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	if h.afterObservationBoundary != nil {
		h.afterObservationBoundary()
	}
	if _, err := h.observe(ctx, after); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.safe, nil
}

// Await returns H once an observation taken during this call is proven safe, so H is at or
// above every id committed before the call. It returns early with ctx's error, or with an
// observation's error once every observation has failed for horizonAwaitFailFor.
//
// It re-observes on the reader's cadence (see the package documentation's "Cadence" section),
// which it runs only for an observation its own snapshot did not prove: one that proved itself
// is proven as it is recorded, and the call returns on it. The cadence observes again at
// once while the newest observation is the only one unproven (GapUnproven), which the next one
// proves unless an event writer holding a transaction id below its own is still running, and
// otherwise after a backoff of 10 ms doubling to 250 ms, which runs only while the caller waits.
// An immediate observation either proves the one before it, which this call took or followed,
// and so returns, or finds an older event writer running and backs off: two never follow each
// other without a proof.
//
// The observation has to start after the call: one already in flight took its snapshot before
// the call and can miss an id committed just before it, so Await waits that one out rather than
// share it. An observation error is retried after the backoff, like a held observation, until
// observations have failed for horizonAwaitFailFor with no success between, measured from the
// first failure since the last success, and is then returned; if ctx ends first, the last one is
// reported with ctx's error. A failure the caller's own ctx caused counts for neither.
func (h *Horizon) Await(ctx context.Context) (int64, error) {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	if h.afterObservationBoundary != nil {
		h.afterObservationBoundary()
	}
	// seen is the number of the newest observation this call has used: each round takes one
	// numbered above it, so no round reads the same observation twice.
	seen := after
	wait := horizonBackoffMin
	var failed error
	var failingSince time.Time
	for {
		number, err := h.observe(ctx, seen)
		seen = max(seen, number)
		again := false
		if err == nil {
			failed = nil
			h.mu.Lock()
			proven, safe, gap := h.safeThrough > after, h.safe, h.gapLocked()
			h.mu.Unlock()
			if proven {
				return safe, nil
			}
			again = gap == GapUnproven
		} else if ctx.Err() == nil {
			now := h.now()
			if failed == nil {
				failingSince = now
			}
			failed = err
			if failingFor := now.Sub(failingSince); failingFor >= horizonAwaitFailFor {
				return 0, fmt.Errorf("await event horizon: every observation failed for %s: %w", failingFor, err)
			}
		}
		if !again && ctx.Err() == nil {
			h.pause(ctx, wait)
			wait = min(2*wait, horizonBackoffMax)
		}
		if ctx.Err() != nil {
			if failed != nil {
				return 0, fmt.Errorf("await event horizon: %w (last observation: %v)", ctx.Err(), failed)
			}
			return 0, fmt.Errorf("await event horizon: %w", ctx.Err())
		}
	}
}

// Gap reports what a gap above H waits for, as the observations recorded so far leave it: none
// when every observed id is at or below H, GapUnproven when only the newest observation is
// unproven, GapHeld when an older one is too.
func (h *Horizon) Gap() Gap {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gapLocked()
}

// gapLocked is Gap with h.mu held. The pending list keeps the newest observation whenever its A
// stands above H (thinPending never drops it), so a gap whose list holds that observation alone
// waits on nothing older.
func (h *Horizon) gapLocked() Gap {
	switch {
	case h.newest <= h.safe:
		return GapNone
	case len(h.pending) == 1 && h.pending[0].number == h.recorded:
		return GapUnproven
	}
	return GapHeld
}

// Proven returns H as already proven, running no observation: 0 before any observation has been
// proven safe, and again after a Reset. The proof that set it ran before the call, so every event
// at or below the id it returns was final before the call too. Safe and Await answer an
// observation taken during the call, so an id at or below theirs may have committed during it; a
// reader that must miss nothing committed after the call, and may be sent some of what committed
// just before it, starts at or below Proven instead.
func (h *Horizon) Proven() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.safe
}

// Mark is one observation of the event log that a reader keeps for itself: Newest, the newest id
// its snapshot saw, NewestAt, that event's created_at (zero for an empty log), and Xid, the
// transaction id the observation took after that snapshot.
type Mark struct {
	Newest   int64
	NewestAt time.Time
	Xid      uint64
}

// markSQL is one mark's statement: the newest event and its created_at, as the statement's
// snapshot sees them, and the transaction id pg_current_xact_id assigns while the statement runs,
// after that snapshot was taken.
const markSQL = `
	select coalesce(newest.id, 0), newest.created_at, pg_current_xact_id()::text
	from (select 1) one
	left join lateral (select id, created_at from events order by id desc limit 1) newest on true
`

// Mark takes one observation now and answers it, without recording it among the observations H is
// proven from, and without waiting on any transaction. It consumes one transaction id.
//
// A mark bounds the ids a writer can still hold open (WriterFloor): a transaction whose own id
// came after Xid drew every event id it holds after the mark's snapshot, by the writer invariant,
// and so above Newest. That holds only while the log the mark saw stands: a reset restarts the ids
// at 1, and a writer of the new log draws ids at or below Newest after the mark. NewestAt is what
// tells the two logs apart (WriterFloor).
func (h *Horizon) Mark(ctx context.Context) (Mark, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin`)
	batch.Queue(`set local synchronous_commit = off`)
	batch.Queue(markSQL)
	batch.Queue(`commit`)
	results := h.db.SendBatch(ctx, batch)
	var mark Mark
	var newestAt *time.Time
	var xidText string
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&mark.Newest, &newestAt, &xidText)
	}
	if err == nil {
		_, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Mark{}, fmt.Errorf("mark the event log: %w", err)
	}
	if newestAt != nil {
		mark.NewestAt = *newestAt
	}
	if mark.Xid, err = strconv.ParseUint(xidText, 10, 64); err != nil {
		return Mark{}, fmt.Errorf("mark the event log: parse transaction id %q: %w", xidText, err)
	}
	return mark, nil
}

// WriterFloor answers the highest id at or below which no event can still be open, read from the
// marks given, the newest first, and from H: the Newest of the first mark older than every
// transaction that holds RowExclusiveLock on events in this database now (oldestEventWriterSQL),
// or H when no mark is, so H is the floor too when no mark is given. A transaction holding an
// event id that is not yet final drew it after it took its transaction id and holds its lock on
// events until it ends. If it drew that id before a mark's snapshot, at or below the mark's
// Newest, it took its transaction id before the mark, below the mark's Xid, and holds its lock
// now; so once no such transaction is older than the mark, every id at or below the mark's
// Newest is final.
// ok is false when no mark qualified, so the floor is H alone.
//
// A mark counts only while the log it saw stands: the row at its Newest still carries its
// NewestAt, or it saw an empty log. A mark from before a reset of the log is passed over. So is H
// when it stands above the first mark's Newest, which only a reset the horizon has not yet
// forgotten (Reset) leaves it: the first mark is the newest, and ids only grow until a reset.
func (h *Horizon) WriterFloor(ctx context.Context, marks ...Mark) (floor int64, ok bool, err error) {
	var oldestText *string
	if err := h.db.QueryRow(ctx, oldestEventWriterSQL).Scan(&oldestText); err != nil {
		return 0, false, fmt.Errorf("read the oldest transaction that can write the event log: %w", err)
	}
	var oldest uint64
	if oldestText != nil {
		if oldest, err = strconv.ParseUint(*oldestText, 10, 64); err != nil {
			return 0, false, fmt.Errorf("read the oldest transaction that can write the event log: parse %q: %w", *oldestText, err)
		}
	}
	floor = h.Proven()
	if len(marks) > 0 && floor > marks[0].Newest {
		floor = 0
	}
	for _, mark := range marks {
		if mark.Xid == 0 || (oldestText != nil && oldest <= mark.Xid) {
			continue
		}
		if mark.Newest > 0 {
			var stands bool
			if err := h.db.QueryRow(ctx, `select exists (select 1 from events where id = $1 and created_at = $2)`, mark.Newest, mark.NewestAt).Scan(&stands); err != nil {
				return 0, false, fmt.Errorf("check a mark's log against the event log: %w", err)
			}
			if !stands {
				continue
			}
		}
		return max(floor, mark.Newest), true, nil
	}
	return floor, false, nil
}

// Reset forgets every observation and H; called when a reader detects a reset event log.
func (h *Horizon) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generation++
	h.safe, h.safeThrough, h.newest, h.recorded = 0, 0, 0, 0
	h.safeAt = time.Time{}
	h.pending = nil
	h.persistedSafe = 0
}

// observe returns once an observation numbered above after has ended, recorded or failed, and
// answers that observation's number and error. An observation numbered at or below after started
// before the caller needed it, so the caller waits it out and takes the next; one numbered above
// it started after, whoever ran it. So the caller takes the newest observation that has ended
// when that one is numbered above after, joins the one in flight, or runs one itself when none
// is. Taking the one that has ended is what keeps callers that waited out one observation
// together on one more: the first of them to wake runs the next, and one that wakes only after
// that has ended takes it rather than run another of its own.
func (h *Horizon) observe(ctx context.Context, after uint64) (uint64, error) {
	for {
		h.mu.Lock()
		if ended := h.ended; ended != nil && ended.number > after && !(ended.abandoned && ctx.Err() == nil) {
			h.mu.Unlock()
			return ended.number, ended.err
		}
		flight := h.inFlight
		run := flight == nil
		if run {
			h.started++
			flight = &horizonFlight{number: h.started, generation: h.generation, done: make(chan struct{})}
			h.inFlight = flight
		}
		h.mu.Unlock()
		if run {
			h.run(ctx, flight)
		} else {
			if h.afterObservationParked != nil {
				h.afterObservationParked()
			}
			select {
			case <-flight.done:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			if h.afterParkedObservationEnded != nil {
				h.afterParkedObservationEnded()
			}
		}
		if flight.number <= after || (flight.abandoned && ctx.Err() == nil) {
			continue
		}
		return flight.number, flight.err
	}
}

// run takes flight's observation and records it, unless a Reset came between. A stall warning it
// finds due runs on a goroutine of its own: its diagnosis query can take up to
// horizonDiagnosisTimeout, which would otherwise hold up the relay, the hub or the handler that
// ran the observation. stalled has already marked the warning logged and the diagnosis started,
// so the rate limit and the one-diagnosis-at-a-time rule both hold however long the diagnosis
// takes.
//
// An observation that failed while ctx was still live logs its error, at most once every
// warnEvery: Await retries it every 250 ms for up to horizonAwaitFailFor, and its long-lived
// callers retry after that, so without the warning readers would wait with nothing in the log to
// say why. One that failed because ctx ended is the caller's own doing, and logs nothing.
func (h *Horizon) run(ctx context.Context, flight *horizonFlight) {
	h.mu.Lock()
	var newestPending uint64
	if n := len(h.pending); n > 0 {
		newestPending = h.pending[n-1].xid
	}
	h.mu.Unlock()
	observation, writer, err := h.take(ctx, newestPending)
	h.mu.Lock()
	h.inFlight = nil
	flight.err = err
	flight.abandoned = err != nil && ctx.Err() != nil
	// An abandoned observation is not an answer a live caller may use, so it must not hide the
	// newest ended observation that is. observe skips an abandoned flight for such a caller.
	if !flight.abandoned {
		h.ended = flight
	}
	var stalled *horizonObservation
	var safe, toPersist int64
	var toPersistAt time.Time
	var generation uint64
	if err == nil && flight.generation == h.generation {
		observation.number, observation.at = flight.number, h.now()
		h.record(observation, writer)
		stalled, safe = h.stalled(observation.at), h.safe
		if h.persist && !h.persisting && h.safe > h.persistedSafe && observation.at.Sub(h.persistedAt) >= horizonPersistEvery {
			h.persisting, toPersist, toPersistAt, generation = true, h.safe, h.safeAt, h.generation
		}
	}
	warnFailure := false
	if err != nil && ctx.Err() == nil {
		now := h.now()
		if h.failWarnedAt.IsZero() || now.Sub(h.failWarnedAt) >= h.warnEvery {
			h.failWarnedAt, warnFailure = now, true
		}
	}
	h.mu.Unlock()
	close(flight.done)
	if warnFailure {
		h.log().Warn("dispatch: event horizon observation failed; live event readers cannot advance until one succeeds",
			"error", err)
	}
	if stalled != nil {
		go h.warnStall(*stalled, observation.at.Sub(stalled.at), safe)
	}
	if toPersist > 0 {
		h.writePersisted(toPersist, toPersistAt, generation)
	}
}

// writePersisted writes safe to event_horizon (persistHorizonSQL) with safeAt, the created_at of
// its event as observed, on the observer's goroutine after the observation's waiters have their
// answer. It writes nothing once a Reset has moved the generation past the one safe was proven in,
// checked under h.mu just before the write, and the statement itself writes nothing once the event
// at safe is another log's, which covers a reset of the log after that check. A try, successful or
// not, is the last one for horizonPersistEvery. A failed write is logged at most once every
// warnEvery and leaves the row as it was: a later process then reads the log above an older value.
func (h *Horizon) writePersisted(safe int64, safeAt time.Time, generation uint64) {
	h.mu.Lock()
	if generation != h.generation {
		h.persisting = false
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), horizonPersistTimeout)
	defer cancel()
	batch := &pgx.Batch{}
	batch.Queue(persistHorizonSQL, safe, safeAt)
	results := h.db.SendBatch(ctx, batch)
	_, err := results.Exec()
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	h.mu.Lock()
	now := h.now()
	h.persisting, h.persistedAt = false, now
	warn := false
	if err == nil && generation == h.generation {
		h.persistedSafe = safe
	} else if err != nil && (h.persistWarnedAt.IsZero() || now.Sub(h.persistWarnedAt) >= h.warnEvery) {
		h.persistWarnedAt, warn = now, true
	}
	h.mu.Unlock()
	if warn {
		h.log().Warn("dispatch: could not persist the event horizon; a process that starts later may read more of the event log", "error", err)
	}
}

// take runs one observation (horizonObserveSQL): A with its event's created_at, F, a transaction id
// below which no event writer of this database was running as its statement ran, X, the
// observer's own transaction id, and whether its snapshot S proved A itself, which it did when its
// in-progress list is empty and its xmax is X (see the package documentation's "Safe horizon"
// section). newestPending is the X of the newest observation still pending, 0 when none is: while
// the snapshot's xmin passes it, the statement does not read the lock table. synchronous_commit is
// off for it alone, since the observer writes nothing a crash could lose, and its commit need not
// wait for a WAL flush to end its transaction id.
func (h *Horizon) take(ctx context.Context, newestPending uint64) (horizonObservation, uint64, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin`)
	batch.Queue(`set local synchronous_commit = off`)
	batch.Queue(horizonObserveSQL, strconv.FormatUint(newestPending, 10))
	batch.Queue(`commit`)
	results := h.db.SendBatch(ctx, batch)
	var newest int64
	var newestAt *time.Time
	var writerText, xmaxText, xidText string
	var xipEmpty bool
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&newest, &newestAt, &writerText, &xmaxText, &xipEmpty, &xidText)
	}
	if err == nil {
		_, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: %w", err)
	}
	writer, err := strconv.ParseUint(writerText, 10, 64)
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: parse the bound on open event writers %q: %w", writerText, err)
	}
	xmax, err := strconv.ParseUint(xmaxText, 10, 64)
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: parse snapshot xmax %q: %w", xmaxText, err)
	}
	xid, err := strconv.ParseUint(xidText, 10, 64)
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: parse transaction id %q: %w", xidText, err)
	}
	observation := horizonObservation{newest: newest, xid: xid, selfProven: xipEmpty && xmax == xid}
	if newestAt != nil {
		observation.newestAt = *newestAt
	}
	return observation, writer, nil
}

// record checks every pending observation against the new one's F, writer, raises H to the A of
// each it proves safe, and to the new one's A when its own snapshot proved it, and keeps the new
// observation if its A still stands above H. h.mu is held.
//
// An observation its own snapshot proved also proves every pending one: its snapshot's xmin then
// equals its X (an empty in-progress list leaves xmin at xmax), F is at least that xmin, and a
// pending observation's X is below this one's, which was assigned later, so the check below
// passes for each of them too.
func (h *Horizon) record(observation horizonObservation, writer uint64) {
	for _, pending := range h.pending {
		if writer > pending.xid && pending.newest > h.safe {
			h.safe, h.safeAt = pending.newest, pending.newestAt
		}
	}
	kept := h.pending[:0]
	for _, pending := range h.pending {
		if writer > pending.xid || pending.newest <= h.safe {
			h.safeThrough = max(h.safeThrough, pending.number)
			continue
		}
		kept = append(kept, pending)
	}
	h.pending = kept
	h.newest, h.recorded = max(h.newest, observation.newest), observation.number
	if observation.selfProven && observation.newest > h.safe {
		h.safe, h.safeAt = observation.newest, observation.newestAt
	}
	if observation.newest <= h.safe {
		h.safeThrough = max(h.safeThrough, observation.number)
		return
	}
	if len(h.pending) >= horizonPendingLimit {
		h.thinPending()
	}
	h.pending = append(h.pending, observation)
}

// thinPending makes room in the full pending list. Any later observation can prove any earlier
// one, so dropping one costs only latency: when a later observation proves a dropped one, H stops
// at the A of the kept one before it. The kept observations are therefore spread evenly in time,
// not by position. Walking from the oldest, which it keeps (the stall warning reports its age),
// it keeps an observation only when the next one was recorded more than step after the last one
// kept, and it always keeps the newest. step is four times the list's span per slot,
// 4·span/horizonPendingLimit, rounded up to at least a nanosecond.
//
// Two observations it keeps next to each other were then recorded at most step apart, unless
// they were next to each other already. So any two neighbours in the list are at most the
// largest step any thinning used, or the time between two observations, apart, and each step is
// at most 4/horizonPendingLimit of the oldest observation's wait, the length of the open
// transactions holding H back. Of any three observations it keeps in a row, the third was
// recorded more than step after the first, so at most 2·span/step + 2, about half the limit,
// survive, and the newest observation always gets a slot. h.mu is held.
func (h *Horizon) thinPending() {
	last := len(h.pending) - 1
	span := h.pending[last].at.Sub(h.pending[0].at)
	step := max((4*span+horizonPendingLimit-1)/horizonPendingLimit, 1)
	kept := h.pending[:1]
	for i := 1; i < last; i++ {
		if h.pending[i+1].at.Sub(kept[len(kept)-1].at) > step {
			kept = append(kept, h.pending[i])
		}
	}
	h.pending = append(kept, h.pending[last])
}

// stalled returns the oldest pending observation when it has waited stallAfter, no warning has
// been logged within warnEvery and no diagnosis is in flight, and marks the warning logged and
// the diagnosis started. h.mu is held.
func (h *Horizon) stalled(now time.Time) *horizonObservation {
	if len(h.pending) == 0 || now.Sub(h.pending[0].at) < h.stallAfter {
		return nil
	}
	if h.diagnosing {
		return nil
	}
	if !h.warnedAt.IsZero() && now.Sub(h.warnedAt) < h.warnEvery {
		return nil
	}
	h.warnedAt, h.diagnosing = now, true
	oldest := h.pending[0]
	return &oldest
}

// warnStall logs that oldest has waited pendingFor to be proven safe, naming the oldest event
// writer of this database (StallHolderSQL). It runs on a goroutine of its own (see run), and the
// diagnosis runs on a context of its own under a short deadline, never one derived from the
// caller's: that one carries the caller's store hold mark (store.WithTransactionTracking,
// store.ForConcurrentRead), with which a Horizon on the shared pool (the fallback when the
// horizon pool cannot open) would have the diagnosis's QueryRow refused as a nested acquire. A
// failed diagnosis is logged in the warning, never returned.
func (h *Horizon) warnStall(oldest horizonObservation, pendingFor time.Duration, safe int64) {
	// The clear is deferred so that it happens however this function ends, and under h.mu
	// because stalled reads the flag: a path that left diagnosing set would have stalled
	// return nil for ever and silence the stall warning for the rest of the process's life.
	defer func() {
		h.mu.Lock()
		h.diagnosing = false
		h.mu.Unlock()
	}()
	attrs := []any{
		"pending_for", pendingFor, "observed_id", oldest.newest, "safe_horizon", safe,
		"observer_xid", oldest.xid,
	}
	diagnosis, cancel := context.WithTimeout(context.Background(), horizonDiagnosisTimeout)
	defer cancel()
	var kind, state, application, database, query string
	var pid int32
	var started *time.Time
	err := h.db.QueryRow(diagnosis, StallHolderSQL).
		Scan(&kind, &pid, &started, &state, &application, &database, &query)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		attrs = append(attrs, "holder", "none open now")
	case err != nil:
		attrs = append(attrs, "diagnosis_error", err)
	default:
		attrs = append(attrs, "holder", kind, "pid", pid)
		if started != nil {
			attrs = append(attrs, "xact_start", *started)
		}
		attrs = append(attrs, "state", state, "application_name", application, "database", database, "query", query)
	}
	h.log().Warn(StallWarning, attrs...)
}

// log is the warnings' sink: logger, or slog.Default at the time of the warning when it is nil.
func (h *Horizon) log() *slog.Logger {
	if h.logger == nil {
		return slog.Default()
	}
	return h.logger
}
