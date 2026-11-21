package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// OwnerLockNamespace is the first key of every owner lock ("OWNR"). Postgres keeps the two-key
// advisory key space apart from the single-key one, and within it the other namespaces of this
// module are the owner lock gate's (OwnerLockGateNamespace, "OWNG"), the copy locks'
// (docs.copiedAsksLockNamespace, "COPY"), the duty leases' (duty.leaseNamespace, "LOOP"), a
// conditional edit's admission's (docs.conditionalEditQueueLockNamespace, "QUEU"), the API's test
// hooks' (api.testHookLockNamespace, "HOOK") and the hashtext of a name (the dependency and
// repository-mapping locks), none of which is this value, so an owner lock never waits on one of
// those whatever its owner hashes to. It is exported, with LockKey, so a test reads an owner's lock
// out of pg_locks by the keys this file takes it with rather than by spelling them again
// (api/files_test.go).
const OwnerLockNamespace int32 = 0x4F574E52

// OwnerLockGateNamespace and OwnerLockGateKey are the two keys of the owner lock gate ("OWNG", 0),
// a transaction advisory lock that migration 20261115200014_owner_lock_legacy_gate's functions take
// and nothing in Go does: every read-phase owner lock of a transaction that finds no earlier build
// in the fleet tries it shared, without waiting, before its first owner lock
// (public.owner_lock_fleet), and the arrival of an earlier build takes it exclusively - its
// dashboard publish (public.owner_lock_legacy_arrival) for as long as that publish waits, its
// registration (public.owner_lock_legacy_registration) for at most 5 seconds - so that arrival
// waits for every transaction that holds it. A try that fails, because an arrival holds the gate
// or waits for it, reads as an earlier build in the fleet, so no owner lock waits on an arrival.
// Its first key is a namespace of its own among the module's two-key locks (OwnerLockNamespace's
// list). They are exported, and spelled here as the migration spells them, so a test reads the
// gate out of pg_locks.
const (
	OwnerLockGateNamespace int32 = 0x4F574E47
	OwnerLockGateKey       int32 = 0
)

// OwnerLockProtocol is the owner lock protocol this build registers in server_processes
// (owner_lock_protocol, migration 20261115100924_server_processes_owner_lock_protocol;
// processes.Register) and names in the dashboard_builds row it publishes (migration
// 20261115200014_owner_lock_legacy_gate; dashboard.Publish): 1, a build that serialises an
// owner's writers on its advisory lock (LockOwner) and takes the owner's row only in its write
// phase. A row at 0, the column's default, is written by a build before this one, which
// serialises them on the owner's row alone: it takes the row `for no key update` (the Inbox `for
// share`) first and holds it for its whole transaction, and never takes the advisory lock.
const OwnerLockProtocol = 1

// The two windows of the fleet the owner lock reads (public.owner_lock_fleet): an earlier build is
// in it while a server_processes row at owner lock protocol 0 has heartbeated within
// OwnerLockLegacyWindow, or while the mark its last arrival stamped (owner_lock_legacy_mark) is
// younger than OwnerLockMarkWindow. Variables rather than constants only so tests can shorten them.
//
// OwnerLockLegacyWindow is processes.PruneAge, so a row of an earlier build counts until it is
// deregistered or pruned. A process of an earlier build silent past it - frozen, or cut from the
// database - has its row pruned and is not in the fleet again until a heartbeat's admit of it,
// whose insert is an arrival, commits. Each such admit waits for the gate at most 2 seconds, its
// heartbeat's own bound, so a write of this build that holds the gate longer defers it, and that
// process serves writes in its row-first order meanwhile. That is the registry's own model:
// every other decision over the fleet (the signing keys, the embedder, the schema admission)
// counts a process silent past processes.LiveWindow as gone.
//
// OwnerLockMarkWindow covers an earlier build's boot from its dashboard publish, the arrival its
// boot makes first, to its registration, which then keeps it in the fleet. Every step of that
// boot is bounded at 30 to 60 seconds and its admission at 20, but SeedRepoProjects is not, so the
// mark holds an hour; an earlier build whose boot takes longer than that from its publish to its
// registration is not in the fleet from the hour's end until it registers. The migration stamps
// the mark too, at the newest dashboard_builds row an earlier build wrote when that is within the
// hour (it spells the hour again), so an earlier boot whose publish came before the migration is
// covered as one after it is.
var (
	OwnerLockLegacyWindow = 10 * time.Minute
	OwnerLockMarkWindow   = time.Hour
)

// OwnerKind names what owns a collaboration: an issue, a document no issue owns (a project
// document, keyed by the artifact's id), or a project.
type OwnerKind string

// The owner kinds, each the prefix of its owners' lock keys.
const (
	OwnerIssue    OwnerKind = "issue"
	OwnerArtifact OwnerKind = "artifact"
	OwnerProject  OwnerKind = "project"
)

// ownerRowKinds is the order the owner rows of several kinds are taken in (lockOwnerRows):
// documents, then issues, then projects, which is the order of the kinds' names. A build that
// serialises on the owner row orders the owners of one transaction by `<kind>:<key>` as Go
// compares strings (its events.lockOwners; its Inbox takes documents, then issues), so taking the
// rows in the same order, and each kind's keys in byte order, takes any two rows in the order
// that build takes them, and no cycle forms between the two.
var ownerRowKinds = []OwnerKind{OwnerArtifact, OwnerIssue, OwnerProject}

// Owner is one owner whose lock a write transaction takes.
type Owner struct {
	Kind OwnerKind
	Key  string
}

// IssueOwner is the issue key's owner.
func IssueOwner(key string) Owner { return Owner{Kind: OwnerIssue, Key: key} }

// ArtifactOwner is the owner of a document no issue owns: the artifact itself.
func ArtifactOwner(id string) Owner { return Owner{Kind: OwnerArtifact, Key: id} }

// ProjectOwner is the project key's owner.
func ProjectOwner(key string) Owner { return Owner{Kind: OwnerProject, Key: key} }

// OwnerOf is the owner of a row that names the issue it belongs to, the document it belongs to, or
// neither: the issue when it names one, since an issue owns its document and every comment, ask
// and message under it, and otherwise the document, which owns the comments and asks on a document
// no issue owns. A row naming neither has no owner and nil is returned, a direct message among
// them, whose conversation is no owner. The rule is one rule, so every reader and writer of such a
// row takes the same lock (refs.sourceOwner).
func OwnerOf(issueKey, artifactID *string) *Owner {
	switch {
	case issueKey != nil:
		owner := IssueOwner(*issueKey)
		return &owner
	case artifactID != nil:
		owner := ArtifactOwner(*artifactID)
		return &owner
	}
	return nil
}

// LockKey is the text whose hashtext is the lock's second key: `<kind>:<key>`, the same spelling
// the event broker orders owners by. It is exported, with OwnerLockNamespace, for a test that
// reads an owner's lock out of pg_locks (api/files_test.go).
func (o Owner) LockKey() string { return string(o.Kind) + ":" + o.Key }

// OwnerLocker is what an owner lock runs on: a transaction, or a savepoint of one, whose rollback
// gives back the locks taken under it. The pool satisfies it too, for a try that holds its lock
// only for its own statement. A lock that takes the owners' rows beside an earlier build, or runs
// the read it guards in its own round trip, sends a batch (lockOwnersAt).
type OwnerLocker interface {
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// The owner lock statements. Each names what it calls owner_lock and spells its keys' text type,
// so a session waiting on one is told apart in pg_stat_activity from a wait on the copy lock
// (docs.projectCopiesLock), and no scan of this module's SQL mistakes one for the other.
//
// A read phase's owner lock is one call of public.owner_lock (migration
// 20261115200014_owner_lock_legacy_gate), its arguments ownerLockArgs': $1 OwnerLockNamespace, $2
// the owners' lock keys, $3 OwnerLockLegacyWindow and $4 OwnerLockMarkWindow in seconds. It runs
// the fleet read first (public.owner_lock_fleet): whether an earlier build is in the fleet, a
// server_processes row at owner lock protocol 0 that heartbeated within the one window or the mark
// an earlier build's arrival stamped within the other. When it finds none it tries the owner lock
// gate shared (OwnerLockGateNamespace), without waiting. A try that fails - an earlier build's
// arrival holds the gate or waits for it, and Postgres grants no conditional request that
// conflicts with a waiting one - answers as an earlier build in the fleet; one that succeeds reads
// the fleet again under a snapshot of its own, so an arrival that committed after the first read
// is read. Then it takes the owners' locks: exclusive or shared, every key once in the order of
// its hash, or tried, each key in the order given. Every row it answers carries the fleet read as
// legacy_fleet; a try answers one row per owner it took, the others one row.
//
// One function rather than one statement of several calls, because Postgres fixes no order among
// the expressions of one statement: the function reads the fleet and tries the gate before it
// takes the first owner lock, so the answer is the mode those locks were taken in, in one
// statement and one round trip, as the statement it replaced was. The gate is only tried, so an
// owner lock never queues behind an arrival, and an arrival waits only for the transactions that
// held the gate when it asked for it.
//
// The write phase's statement (QueueLockOwner), which takes again a lock the transaction holds,
// carries no fleet read and takes no gate: the transaction's read phase took both.
const (
	ownerLock        = `select pg_advisory_xact_lock($1, hashtext($2::text)) as owner_lock`
	ownersLock       = `select locked_key, legacy_fleet from public.owner_lock('exclusive', $1, $2::text[], $3, $4) as owner_lock`
	ownersLockShared = `select locked_key, legacy_fleet from public.owner_lock('shared', $1, $2::text[], $3, $4) as owner_lock`
	ownersTryLock    = `select locked_key, legacy_fleet from public.owner_lock('try', $1, $2::text[], $3, $4) as owner_lock`
)

// ownerLockArgs is the arguments of the read phase's owner lock statements above for the lock
// keys keys.
func ownerLockArgs(keys []string) []any {
	return []any{OwnerLockNamespace, keys, OwnerLockLegacyWindow.Seconds(), OwnerLockMarkWindow.Seconds()}
}

// lockWait bounds every wait a write makes for a transaction advisory lock another writer holds:
// a read-phase owner lock's (lockOwnersAt) - the wait for the advisory lock, and beside an earlier
// build the wait for the owner's row (lockOwnerRows) - and every other advisory lock a request's
// write takes behind another writer's (LockWithinBound: api's dependency, rank-allocation, upload
// and repository-mapping locks, prioritystore's priority-rank lock, delivery's session-groups
// lock, docs' copy and conditional-edit queue locks, and a request's wait for a document's own
// lock, docs.lockDocumentRoom). It bounds too every row lock a write takes on a row that some
// other locker takes without an owner lock the write holds, so that no owner lock orders the two,
// and every write of such a row, which takes the row in the bound first or runs in the bound
// itself (LockRowsWithinBound and LockRowsWithinBoundAndRead, and WriteBatch.QueueWithinBound in a
// write phase or in a write of its own sent on a pool: a step's node, a message's row and its
// delivery attempts, a comment delivery's attempts, the Inbox's and agent state's own rows, an
// agent's read mark, a person's row, a pull request's head, an architecture source's rows and an
// item's block state). A wait that runs it out is refused as a pool that could not serve the call
// is refused (ErrPoolBusy, boundedWait.refused): a request answers 503 DATABASE_BUSY with
// Retry-After and its client sends the write again; a background pass logs it and tries again at
// its next tick; a settlement stops short and its pending row resumes it. Without it the wait
// ended only with its holder: a write has no deadline once admitted (api's defaultReadDeadline),
// so a writer queued behind a stalled holder held its pool connection, and a request its admission
// unit, for as long as that holder stood. It is the request pool's lock_timeout too, on every
// connection and in every transaction (configurePool, Pool.begin), so a request's statement that
// no bounded wait covers waits no longer than this either, and is refused lock_not_available,
// which IsPoolBusy reads as busy; an advisory lock's bounded wait still refuses inside its
// savepoint, leaving its transaction usable.
//
// It is the commit's bound (commitTimeout), because that is how long a holder that has not stalled
// keeps such a lock past its read phase: it holds it through its write phase's statements and its
// COMMIT, and both run under CommitContext's ten seconds (docs.Ledger.commit, trackedTx.Commit).
// Ordinary contention, one writer after another each holding the lock for milliseconds, waits far
// less, so it is a holder whose database stopped answering it that a waiter runs this out behind.
// A holder can hold an owner lock longer in its read phase by design - a write that folds a cold
// document under it waits out one cold read (ColdReadBound) - and a writer queued behind that is
// refused too, and retried by its client. Ten seconds is under a read's twenty-second deadline
// (api's defaultReadDeadline) and the request pool's seventy-second idle bound
// (RequestIdleInTransactionTimeout).
//
// Postgres bounds each lock a statement waits for by lock_timeout, so the bound is per lock: a
// statement that locks several owners, every one of them held, waits at most this long for each.
const lockWait = commitTimeout

// lockWaitDefault is lockWait as lock_timeout's value (lockTimeoutSetting).
var lockWaitDefault = lockTimeoutSetting(lockWait)

// lockTimeoutSetting is bound as lock_timeout's value: whole milliseconds with their unit, at
// least one, since zero would be no bound at all.
func lockTimeoutSetting(bound time.Duration) string {
	return strconv.FormatInt(max(bound.Milliseconds(), 1), 10) + "ms"
}

// lockWaitKey carries the bound a test puts on the lock waits made under a context
// (WithLockWaitForTest).
type lockWaitKey struct{}

// WithLockWaitForTest returns ctx with bound in place of lockWait for every bounded lock wait made
// under it (lockOwnersAt, LockWithinBound, LockRowsWithinBound, and WriteBatch.Send's). It is for
// tests, which make a wait run out its bound in milliseconds rather than in production's ten
// seconds; nothing in production calls it, and a lock taken under a context carrying none waits
// under lockWait.
func WithLockWaitForTest(ctx context.Context, bound time.Duration) context.Context {
	return context.WithValue(ctx, lockWaitKey{}, bound)
}

// lockWaitSetting is the lock_timeout a bounded lock wait made under ctx waits under.
func lockWaitSetting(ctx context.Context) string {
	if bound, ok := ctx.Value(lockWaitKey{}).(time.Duration); ok {
		return lockTimeoutSetting(bound)
	}
	return lockWaitDefault
}

// The statements of a bounded wait: the transaction's own lock_timeout kept in a setting of the
// transaction's (dispatch.lock_timeout_outside_bounded_wait), lock_timeout set to the bound, the
// lock's statements, and the kept value set again (boundedWaitKeep, boundedWaitBound,
// boundedWaitRestore), all in the lock's own batch, so a wait that ends in time costs no round
// trip more than the lock alone; the kept value is kept in the database rather than read back by
// the caller for that. A wait that runs out fails its lock statement with lock_not_available
// (55P03), and the server skips the rest of the batch.
//
// A row lock - a locking read, or the row's own write - waits so in its transaction itself
// (rowLockWait, and WriteBatch.QueueWithinBound in a write phase): its refusal aborts the
// transaction, which every caller rolls back, since it answers the refusal as a pool that could
// not serve it. A savepoint around it would leave the row locked by the savepoint's
// subtransaction, and the transaction's own write of the row behind the lock - the reason for most
// such locks - would then make Postgres record the two as a MultiXact (compute_new_xmax_infomask)
// on every such write.
//
// An advisory lock waits so inside a savepoint (advisoryLockWait), docs' lockSettlementCursor's
// form: the savepoint opens before the kept value (boundedWaitBegin) and is released after it is
// set again (boundedWaitRelease), which moves every lock taken under it to the transaction. A
// setting made with set_config's local flag outlives a released savepoint, as lockSettlementCursor
// says, so the transaction's own is set again. A wait that runs out is left (boundedWait.refused):
// the transaction rolls back to the savepoint, which gives back every lock taken under it and both
// settings, and releases it, so the transaction is as it was before the lock. A savepoint,
// set_config, a release and an advisory lock write nothing, so none of them assigns a transaction
// id, and a lock a read phase takes still comes before the transaction's first.
const (
	boundedWaitBegin   = `savepoint bounded_lock_wait`
	boundedWaitKeep    = `select set_config('dispatch.lock_timeout_outside_bounded_wait', current_setting('lock_timeout'), true)`
	boundedWaitBound   = `select set_config('lock_timeout', $1, true)`
	boundedWaitRestore = `select set_config('lock_timeout', current_setting('dispatch.lock_timeout_outside_bounded_wait'), true)`
	boundedWaitRelease = `release savepoint bounded_lock_wait`
	boundedWaitUndo    = `rollback to savepoint bounded_lock_wait`
)

// boundedWait is how a lock's statements wait (sendWithinBound): wait is the bound as
// lock_timeout's value (lockWaitSetting), and savepoint whether they wait inside a savepoint of
// their own, as an advisory lock does. Its zero value is no bounded wait, for a lock that never
// waits (a level that skips).
type boundedWait struct {
	wait      string
	savepoint bool
}

// advisoryLockWait is the bounded wait of a transaction advisory lock taken under ctx: inside a
// savepoint, so a refusal leaves the transaction as it was before the lock.
func advisoryLockWait(ctx context.Context) boundedWait {
	return boundedWait{wait: lockWaitSetting(ctx), savepoint: true}
}

// rowLockWait is the bounded wait of a row lock taken under ctx: in the transaction itself, so a
// refusal aborts it.
func rowLockWait(ctx context.Context) boundedWait {
	return boundedWait{wait: lockWaitSetting(ctx)}
}

// begin queues on batch the wait's statements before its lock's, and answers how many.
func (w boundedWait) begin(batch *pgx.Batch) int {
	if w.wait == "" {
		return 0
	}
	queued := 2
	if w.savepoint {
		batch.Queue(boundedWaitBegin)
		queued++
	}
	batch.Queue(boundedWaitKeep)
	batch.Queue(boundedWaitBound, w.wait)
	return queued
}

// end queues on batch the wait's statements after its lock's, and answers how many.
func (w boundedWait) end(batch *pgx.Batch) int {
	if w.wait == "" {
		return 0
	}
	batch.Queue(boundedWaitRestore)
	if !w.savepoint {
		return 1
	}
	batch.Queue(boundedWaitRelease)
	return 2
}

// refused answers err, the failure of a statement of the wait's batch, whose results the caller
// has closed. A wait that ran out its bound (ranOutItsBound) is answered ErrPoolBusy
// (heldPastItsBound), which every caller already answers as a pool that could not serve it, and
// one in a savepoint is left first: the transaction rolls back to the savepoint and releases it.
// Any other failure is returned as it is.
//
// The rollback and the release go in two round trips, the rollback first. The error has aborted
// the transaction, and Postgres parses no statement in an aborted transaction but one that ends it
// or rolls it back to a savepoint; pgx prepares every statement of a batch before it runs any, and
// it dropped the release from its statement cache with the rest of the failed batch, so a batch of
// the two would have the release refused (25P02) before the rollback ran.
func (w boundedWait) refused(ctx context.Context, q OwnerLocker, err error) error {
	if w.wait == "" || !ranOutItsBound(err) {
		return err
	}
	if !w.savepoint {
		return heldPastItsBound(w.wait, err)
	}
	undoErr := sendOne(ctx, q, boundedWaitUndo)
	if undoErr == nil {
		undoErr = sendOne(ctx, q, boundedWaitRelease)
	}
	if undoErr != nil {
		return fmt.Errorf("%w: another transaction held the lock past its wait's bound of %s, and leaving the wait failed (%w): %w", ErrPoolBusy, w.wait, undoErr, err)
	}
	return heldPastItsBound(w.wait, err)
}

// boundedLock is one lock statement sendWithinBound sends: sql with args, and take, which reads its
// answer, the next result of results.
type boundedLock struct {
	sql  string
	args []any
	take func(results pgx.BatchResults) error
}

// execLock is the take of a lock statement whose answer carries nothing its caller reads.
func execLock(results pgx.BatchResults) error {
	_, err := results.Exec()
	return err
}

// sendWithinBound is every lock that waits in a bounded wait, and the one place its batch is
// built: it sends on q, in one batch, locks in the bounded wait w, and after them read when it is
// not nil, the statement the locks guard. It reads every answer before read's - past the wait's
// own statements, and each lock's through its take - and answers results with read's answer next,
// for the caller to read or close. A failure before read's answer closes results and is answered
// as w answers it (boundedWait.refused). The server starts read, taking its snapshot, only once
// the wait has ended and every lock is the transaction's, so read sees every commit of the locks'
// previous holders and runs under the transaction's own lock_timeout again.
func sendWithinBound(ctx context.Context, q OwnerLocker, w boundedWait, locks []boundedLock, read *guardedRead) (pgx.BatchResults, error) {
	batch := &pgx.Batch{}
	begun := w.begin(batch)
	for _, lock := range locks {
		batch.Queue(lock.sql, lock.args...)
	}
	ended := w.end(batch)
	if read != nil {
		batch.Queue(read.sql, read.args...)
	}
	results := q.SendBatch(ctx, batch)
	err := skipResults(results, begun)
	for _, lock := range locks {
		if err != nil {
			break
		}
		err = lock.take(results)
	}
	if err == nil {
		err = skipResults(results, ended)
	}
	if err != nil {
		_ = results.Close()
		return nil, w.refused(ctx, q, err)
	}
	return results, nil
}

// skipResults reads the next n answers of results, which carry nothing the caller needs, and
// answers the first failure among them.
func skipResults(results pgx.BatchResults, n int) error {
	for range n {
		if _, err := results.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// ranOutItsBound reports whether err is a statement whose wait for a lock ran out its bound:
// lock_not_available (55P03), which lock_timeout raises - a bounded wait's lock statement, or a
// plain statement under the request pool's own bound (IsPoolBusy).
func ranOutItsBound(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// heldPastItsBound is err, a lock statement that ran out a bounded wait of wait (ranOutItsBound),
// named ErrPoolBusy.
func heldPastItsBound(wait string, err error) error {
	return fmt.Errorf("%w: another transaction held the lock past its wait's bound of %s: %w", ErrPoolBusy, wait, err)
}

// sendOne runs sql, a statement that answers nothing the caller needs, on q in a round trip of its
// own.
func sendOne(ctx context.Context, q OwnerLocker, sql string) error {
	batch := &pgx.Batch{}
	batch.Queue(sql)
	return q.SendBatch(ctx, batch).Close()
}

// LockWithinBound runs lock with args, a statement that takes a transaction advisory lock another
// writer may hold and answers nothing its caller reads, in a bounded wait (lockWait) inside a
// savepoint (advisoryLockWait), as a read-phase owner lock waits: a lock still held when the bound
// runs out is refused ErrPoolBusy, and the transaction is as it was before the statement
// (boundedWait.refused). It is how a request's write takes every advisory lock of its own besides
// the owner lock (api's dependency, rank-allocation, upload and repository-mapping locks,
// prioritystore's priority-rank lock, delivery's session-groups lock, docs' copy and
// conditional-edit queue locks, and a document's lock where a request waits for it), so no write
// waits on one without end. It is one round trip, as the statement alone was. A row lock takes
// LockRowsWithinBound instead.
func LockWithinBound(ctx context.Context, q OwnerLocker, lock string, args ...any) error {
	results, err := sendWithinBound(ctx, q, advisoryLockWait(ctx), []boundedLock{{sql: lock, args: args, take: execLock}}, nil)
	if err != nil {
		return err
	}
	return results.Close()
}

// LockRowsWithinBound runs lock with args, a statement that takes a row lock on rows no owner lock
// of the caller's keeps every other locker from - a locking read, or the rows' own write - and
// answers nothing its caller reads, in a bounded wait (lockWait) in the transaction itself
// (rowLockWait), as WriteBatch.QueueWithinBound waits: a row still held when the bound runs out is
// refused ErrPoolBusy (heldPastItsBound), and the refusal aborts the transaction, which the caller
// then rolls back. Every caller does: it answers the refusal as a pool that could not serve it. So
// the lock takes no savepoint, which would make every later write of the rows in the transaction a
// MultiXact of the savepoint's lock and that write. It is how a write takes every such row, so no
// write waits on one without end. A caller that reads the locked rows, or what its write returned,
// reads them in a statement after this one, which finds them already held, and sends it in the
// same batch (LockRowsWithinBoundAndRead). It is one round trip, as the statement alone was.
func LockRowsWithinBound(ctx context.Context, q OwnerLocker, lock string, args ...any) error {
	return LockRowsWithinBoundAndRead(ctx, q, lock, args, "", nil, nil)
}

// LockRowsWithinBoundAndRead is LockRowsWithinBound followed by read with readArgs, the statement
// that reads the row lock took, or writes it and returns what it wrote (an update, a delete or an
// upsert, `returning` the row), whose one row scan reads. The two go in one batch, one round trip
// and one statement of the transaction's as the locking read or the write alone was, and stay two
// statements: the server starts read, taking its snapshot, only once the bounded wait has ended
// (sendWithinBound), so read sees every commit of the row's previous holder, waits on nothing the
// lock took, and runs under the transaction's own lock_timeout again. The lock's failure comes back
// as LockRowsWithinBound's does, ErrPoolBusy at its bound, and scan is not called; read's comes
// back as scan returned it, pgx.ErrNoRows included, which leaves the transaction usable. An empty
// read sends none.
func LockRowsWithinBoundAndRead(
	ctx context.Context, q OwnerLocker, lock string, lockArgs []any, read string, readArgs []any, scan func(pgx.Row) error,
) error {
	var guarded *guardedRead
	if read != "" {
		guarded = &guardedRead{sql: read, args: readArgs, scan: scan}
	}
	results, err := sendWithinBound(ctx, q, rowLockWait(ctx), []boundedLock{{sql: lock, args: lockArgs, take: execLock}}, guarded)
	if err != nil {
		return err
	}
	if guarded == nil {
		return results.Close()
	}
	return guarded.answer(results)
}

// The head of each kind's row lock (ownerLockLevel.rowLocks): the rows of kind's owners named by
// $1, each answering its key as text (an artifact's id canonical), whose `order by` takes them in
// byte order of their keys (an issue's or a project's key collated "C", an artifact's id as a
// uuid, whose order is its text's). A level's row-lock clause follows it.
const (
	ownerRowsArtifacts = `select id::text from artifacts where id = any($1::uuid[]) order by id`
	ownerRowsIssues    = `select key from issues where key = any($1::text[]) order by key collate "C"`
	ownerRowsProjects  = `select key from projects where key = any($1::text[]) order by key collate "C"`
)

// ownerLockLevel is a level a read phase takes its owners' locks at, with both locks the level
// decides in one value, so that the advisory lock and the row lock of one level cannot drift
// apart: statement, the owner lock statement above that takes the owners' advisory locks at it,
// and rowLocks, by kind, the statements that take the owners' rows at it beside an earlier build
// (lockOwnerRows), each with the clause that build takes the row with for the same work. At a
// level that skips, an owner whose lock or row another transaction holds is left out of what the
// lock answers rather than waited for. named is what the level's failures say after the owners
// they name. Every read-phase owner lock runs at one of the three levels below, through
// lockOwnersAt.
type ownerLockLevel struct {
	statement string
	rowLocks  map[OwnerKind]string
	skips     bool
	named     string
}

// The three levels. Each spells its row locks whole, a kind's head and the level's clause in one
// constant expression, so api.TestNoForUpdateOnOwnerTables reads each owner table with its lock
// clause in one statement. These and the write phase's events.lockEventOwner are the only
// statements of this module that lock an owner row.
var (
	// ownerLockExclusive is every writer's (LockOwner, LockOwners, LockOwnersAndRead): the rows
	// `for no key update`.
	ownerLockExclusive = &ownerLockLevel{
		statement: ownersLock,
		rowLocks: map[OwnerKind]string{
			OwnerArtifact: ownerRowsArtifacts + " for no key update",
			OwnerIssue:    ownerRowsIssues + " for no key update",
			OwnerProject:  ownerRowsProjects + " for no key update",
		},
	}
	// ownerLockShared is the Inbox's (LockOwnersShared): the rows `for share`.
	ownerLockShared = &ownerLockLevel{
		statement: ownersLockShared,
		rowLocks: map[OwnerKind]string{
			OwnerArtifact: ownerRowsArtifacts + " for share",
			OwnerIssue:    ownerRowsIssues + " for share",
			OwnerProject:  ownerRowsProjects + " for share",
		},
		named: " for share",
	}
	// ownerLockTry is a background pass's (TryLockOwners): each owner's lock tried, and the rows
	// `for no key update skip locked`.
	ownerLockTry = &ownerLockLevel{
		statement: ownersTryLock,
		rowLocks: map[OwnerKind]string{
			OwnerArtifact: ownerRowsArtifacts + " for no key update skip locked",
			OwnerIssue:    ownerRowsIssues + " for no key update skip locked",
			OwnerProject:  ownerRowsProjects + " for no key update skip locked",
		},
		skips: true,
		named: " without waiting",
	}
)

// failed is err, the failure of level's owner lock statement for owners, naming what it locked.
func (level *ownerLockLevel) failed(owners []Owner, err error) error {
	if len(owners) == 1 {
		return fmt.Errorf("lock %s %s%s: %w", owners[0].Kind, owners[0].Key, level.named, err)
	}
	return fmt.Errorf("lock owners %v%s: %w", lockKeys(owners), level.named, err)
}

// scan reads rows, the answer of level's owner lock statement for owners, whose lock keys are
// keys, to their end, which runs the statement to its end and so takes every lock it names. It
// answers the owners whose locks the statement took - every one at a level that waits, those its
// rows name at one that skips - and the fleet read, which it records (noteOwnerLockMode). A try
// that took none answers no row and so no fleet read, and nothing is recorded.
func (level *ownerLockLevel) scan(rows pgx.Rows, owners []Owner, keys []string) ([]Owner, bool, error) {
	defer rows.Close()
	var byKey map[string]Owner
	if level.skips {
		byKey = make(map[string]Owner, len(owners))
		for index, owner := range owners {
			byKey[keys[index]] = owner
		}
	}
	var held []Owner
	legacy, answered := false, false
	for rows.Next() {
		if !level.skips {
			if err := rows.Scan(nil, &legacy); err != nil {
				return nil, false, err
			}
			answered = true
			continue
		}
		var key string
		if err := rows.Scan(&key, &legacy); err != nil {
			return nil, false, err
		}
		answered = true
		held = append(held, byKey[key])
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if !answered {
		return nil, false, nil
	}
	noteOwnerLockMode(legacy)
	if !level.skips {
		held = owners
	}
	return held, legacy, nil
}

// ownerLockMode is which owner lock a transaction of this process took, by its fleet read
// (public.owner_lock_fleet, the owner lock statements above). Its zero value, which String names
// unknown, is a process that has taken no owner lock yet.
type ownerLockMode int32

const (
	// ownerLockAdvisory is the advisory lock alone, behind the owner lock gate taken shared: no
	// earlier build, which serialises on the owner row, is in the fleet.
	ownerLockAdvisory ownerLockMode = iota + 1
	// ownerLockAdvisoryAndRow is the advisory lock and then the owner's row, before any read the
	// lock guards, and no gate: an earlier build, which serialises on that row, is in the fleet,
	// or its arrival holds the gate or waits for it (lockOwnerRows).
	ownerLockAdvisoryAndRow
)

func (m ownerLockMode) String() string {
	switch m {
	case ownerLockAdvisory:
		return "advisory"
	case ownerLockAdvisoryAndRow:
		return "advisory and row"
	}
	return "unknown"
}

// latestOwnerLockMode is the mode the latest read-phase owner lock of this process took.
var latestOwnerLockMode atomic.Int32

// lastOwnerLockMode is the mode the latest read-phase owner lock this process took ran in: what
// its fleet read answered. A test reads it after a write of its own; across concurrent writers it
// is whichever answered last.
func lastOwnerLockMode() ownerLockMode { return ownerLockMode(latestOwnerLockMode.Load()) }

// noteOwnerLockMode records the mode a read-phase owner lock took, and logs once each time the
// process's mode changes, not once per write. The common case, an unchanged mode, is one atomic
// load.
func noteOwnerLockMode(legacy bool) {
	mode := ownerLockAdvisory
	if legacy {
		mode = ownerLockAdvisoryAndRow
	}
	previous := ownerLockMode(latestOwnerLockMode.Load())
	if previous == mode || !latestOwnerLockMode.CompareAndSwap(int32(previous), int32(mode)) {
		return
	}
	if legacy {
		slog.Info("dispatch: owner locks now take the owner's row too: an earlier build, which serialises on the owner row, is in the fleet (owner_lock_protocol 0)",
			"was", previous.String(), "mode", mode.String())
		return
	}
	slog.Info("dispatch: owner locks take the advisory lock alone: no earlier build, which serialises on the owner row, is in the fleet",
		"was", previous.String(), "mode", mode.String())
}

// LockOwner takes owner's lock: a transaction advisory lock, exclusive, held until the transaction
// ends. It is Dispatch's per-owner serialization. Every transaction that writes an owner's
// collaboration rows, its own row or anything a write to the owner decides on takes it before it
// reads what that decision rests on, so writers of one owner run one at a time, as they did under
// the owner row's `for no key update`; unlike that row lock it assigns the transaction no
// transaction id, so a transaction holds one only from its first write on.
//
// A build before this one serialises on the owner row instead, and its writers never take this
// lock, so while one is in the fleet (the fleet read the lock's statement makes before it takes
// the lock, public.owner_lock_fleet) LockOwner also takes the owner's row `for no key update`, in a
// statement of its own right after the lock and before any read the lock guards (lockOwnerRows).
// That row lock assigns the transaction its id from there on, which is the cost of a fleet with an
// earlier build in it. The order is the lock and then the row, never the other way round: a writer
// of this build in the advisory mode holds the lock and takes the row in its write phase
// (events.lockEventOwner), so taking the row first would wait for it while it waits for the lock.
//
// While no earlier build is in the fleet, the statement tries the owner lock gate shared before
// the lock (OwnerLockGateNamespace) and holds it to the transaction's end, so an earlier build that
// arrives meanwhile - its dashboard publish, or its registration for up to 5 seconds - waits for
// this transaction to end before that arrival commits, and every owner lock after it reads the
// earlier build in the fleet. A try that fails, while an arrival holds the gate or waits for it,
// takes the owner's row as beside an earlier build, so no owner lock waits on an arrival.
//
// Its wait for an owner another transaction holds is bounded (lockWait): one that runs out is
// refused ErrPoolBusy. A wait for the advisory lock leaves the transaction as it was before the
// lock; beside an earlier build, a wait for the owner's row aborts it (lockOwnerRows), and the
// caller rolls it back, as every caller does on that refusal.
//
// The lock and the read it guards must be separate statements. Under READ COMMITTED a statement's
// snapshot is taken when it starts, before it waits, so a statement that both took this lock and
// read the owner (`select pg_advisory_xact_lock(...), closed_at from issues ...`) would read the
// row as it stood before the wait and miss a close the lock's previous holder committed meanwhile.
// The statement after the lock takes a fresh snapshot, which holds every commit of that holder.
// LockOwnerAndRead sends the two in one round trip.
//
// A transaction that locks several owners takes them all through LockOwners, which orders them by
// lock key. Taking the lock again in the transaction that holds it returns at once.
func LockOwner(ctx context.Context, q OwnerLocker, owner Owner) error {
	return LockOwners(ctx, q, owner)
}

// LockOwners takes the locks of every owner in owners (LockOwner) in one statement, sorted and
// deduplicated by the lock's key rather than by the owner's name: two owners whose names hash to
// the same key share one lock, and ordering by name could take that lock before a third owner's in
// one transaction and after it in another, a cycle Postgres breaks with `deadlock detected`. Sorted
// by key, every transaction takes any two keys in the same order. As with LockOwner, the reads the
// locks guard come in statements after this one, and while the fleet read answers that an earlier
// build is in the fleet, the owners' rows are taken between the two (lockOwnerRows).
func LockOwners(ctx context.Context, q OwnerLocker, owners ...Owner) error {
	if len(owners) == 0 {
		return nil
	}
	_, _, err := lockOwnersAt(ctx, q, ownerLockExclusive, owners, nil)
	return err
}

// lockOwnersAt is every read-phase owner lock, and the one place its rule is written. It takes the
// advisory locks of owners at level in one statement, behind the fleet read and the gate
// (level.statement), and while that fleet read answers that an earlier build is in the fleet, the
// owners' rows at level in the round trip right after it (lockOwnerRows): that build's writers
// serialise on the row and never take the advisory lock, so the row alone excludes them. It answers
// the owners whose locks the transaction now holds - every one of owners, or at a level that
// skips, those whose lock it took and, beside an earlier build, whose row it took too - and the
// fleet read. With no earlier build in the fleet the lock is that one statement, which assigns the
// transaction no transaction id.
//
// Only a test reads the fleet read it answers: raceWriter (ownerlock_fleet_test.go), whose writers
// in the owner lock's races run at once and so each need their own transaction's answer, which
// lastOwnerLockMode, the latest of the process's, is not. Every caller here drops it.
//
// read, when it is not nil, is the statement the locks guard, sent with them (LockOwnersAndRead):
// in one batch behind the advisory lock's statement, one round trip. The batch is sent before the
// fleet read is answered, so read has already run when the answer says the owners' rows must be
// taken too: read then ran before the rows were locked, and a process of an earlier build may have
// been writing them. So in that mode its answer is dropped unread, and the round trip that takes
// the rows runs read again behind them: one more round trip, in that mode alone.
//
// At a level that waits, every wait is bounded (lockWait): the lock's statement runs in an
// advisory lock's bounded wait (advisoryLockWait, sendWithinBound) sent in its batch, ahead of
// read, so read runs under the transaction's own lock_timeout again, and the row locks beside an
// earlier build run in a row lock's (lockOwnerRows). A level that skips never waits, so its
// statement runs alone.
func lockOwnersAt(ctx context.Context, q OwnerLocker, level *ownerLockLevel, owners []Owner, read *guardedRead) ([]Owner, bool, error) {
	keys := lockKeys(owners)
	var (
		held    []Owner
		legacy  bool
		results pgx.BatchResults
		err     error
	)
	if level.skips && read == nil {
		var rows pgx.Rows
		if rows, err = q.Query(ctx, level.statement, ownerLockArgs(keys)...); err == nil {
			held, legacy, err = level.scan(rows, owners, keys)
		}
	} else {
		var wait boundedWait
		if !level.skips {
			wait = advisoryLockWait(ctx)
		}
		lock := boundedLock{sql: level.statement, args: ownerLockArgs(keys), take: func(results pgx.BatchResults) error {
			rows, err := results.Query()
			if err != nil {
				return err
			}
			held, legacy, err = level.scan(rows, owners, keys)
			return err
		}}
		results, err = sendWithinBound(ctx, q, wait, []boundedLock{lock}, read)
	}
	if err != nil {
		return nil, false, level.failed(owners, err)
	}
	if legacy && len(held) > 0 {
		if results != nil {
			// read, when there is one, ran ahead of the row locks: drop its answer (Close reads it
			// and keeps only its failure) and run it again behind them.
			if err := results.Close(); err != nil {
				return nil, legacy, err
			}
		}
		held, err = lockOwnerRows(ctx, q, level, held, read)
		return held, legacy, err
	}
	switch {
	case read != nil:
		err = read.answer(results)
	case results != nil:
		err = results.Close()
	}
	return held, legacy, err
}

// LockOwnerAndRead takes owner's lock (LockOwner) and then runs read with args, the statement the
// lock guards, scanning its one row into dest. The two go in one batch, one round trip, and stay
// two statements: the server starts read, taking its snapshot, only once the lock is granted, so
// read sees every commit of the lock's previous holder. A session waiting on the lock shows the
// lock's statement (owner_lock) in pg_stat_activity. The lock's failure is wrapped as LockOwner
// wraps it; read's comes back as Scan returned it, pgx.ErrNoRows included.
func LockOwnerAndRead(ctx context.Context, tx pgx.Tx, owner Owner, read string, args []any, dest ...any) error {
	return LockOwnersAndRead(ctx, tx, []Owner{owner}, read, args, dest...)
}

// LockOwnersAndRead is LockOwnerAndRead for the locks of several owners, taken as LockOwners takes
// them, in one statement ahead of read. Beside an earlier build read runs again behind the
// owners' rows, one round trip more (lockOwnersAt).
func LockOwnersAndRead(ctx context.Context, tx pgx.Tx, owners []Owner, read string, args []any, dest ...any) error {
	_, _, err := lockOwnersAt(ctx, tx, ownerLockExclusive, owners, &guardedRead{sql: read, args: args, scan: func(row pgx.Row) error {
		return row.Scan(dest...)
	}})
	return err
}

// guardedRead is the read a lock guards when the lock sends it (LockOwnersAndRead,
// LockRowsWithinBoundAndRead): its statement and arguments, and what reads its one row.
type guardedRead struct {
	sql  string
	args []any
	scan func(pgx.Row) error
}

// answer reads read's row, the next result of results, with its scan and closes results. Its
// failure comes back as scan returned it, pgx.ErrNoRows included.
func (read *guardedRead) answer(results pgx.BatchResults) error {
	err := read.scan(results.QueryRow())
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	return err
}

// QueueLockOwner queues owner's lock (LockOwner) on a write phase's batch, for a statement after it
// in the same batch that the lock guards: a write that takes its owner's lock again where the
// transaction took it in its read phase, which returns at once. It carries no fleet read: the
// transaction's read phase made the one it acts on. The lock waits within the write lock bound
// (WriteBatch.QueueWithinBound), because not every write took it in its read phase: a create's
// owner is new (a project's create appends on the project it inserts), and an append can name an
// owner beside the one its read phase locked (an anchor refresh's appends on the ask's or the
// comment's own owner). An admitted write has no deadline, so a bare wait there lasted as long as
// another holder of that key stood; a wait that runs the bound out is ErrPoolBusy. Its failure is
// otherwise wrapped as LockOwner wraps it.
func QueueLockOwner(b *WriteBatch, owner Owner) {
	b.QueueWithinBound(fmt.Sprintf("lock %s %s", owner.Kind, owner.Key), ownerLock, OwnerLockNamespace, owner.LockKey())
}

// lockKeys is each owner's lock key, in owners' order.
func lockKeys(owners []Owner) []string {
	keys := make([]string, len(owners))
	for index, owner := range owners {
		keys[index] = owner.LockKey()
	}
	return keys
}

// ownerRowLock is one statement of lockOwnerRows: the rows of one kind of owner, and their keys.
type ownerRowLock struct {
	kind      OwnerKind
	statement string
	keys      []string
}

// failed is err, the failure of the row lock's statement, naming the rows it locked.
func (l ownerRowLock) failed(err error) error {
	return fmt.Errorf("lock %s rows %v: %w", l.kind, l.keys, err)
}

// ownerRowLocks is the statements that take the rows of owners at level, one per kind of owner
// among them, in ownerRowKinds' order: a statement of its own for each kind, level's row lock of
// that kind, which takes the rows in byte order of their keys (ownerRowsArtifacts, ownerRowsIssues,
// ownerRowsProjects). A document's id that no uuid spells has no row, and is left out rather than
// refused by the uuid cast.
func ownerRowLocks(owners []Owner, level *ownerLockLevel) []ownerRowLock {
	locks := make([]ownerRowLock, 0, len(ownerRowKinds))
	for _, kind := range ownerRowKinds {
		var keys []string
		for _, owner := range owners {
			if owner.Kind != kind {
				continue
			}
			if key, ok := owner.rowKey(); ok && !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			locks = append(locks, ownerRowLock{kind: kind, statement: level.rowLocks[kind], keys: keys})
		}
	}
	return locks
}

// rowKey is the key owner's row is named by in its table and its row lock's answer, an artifact's
// id canonical, and false for a document's id that no uuid spells, which has no row.
func (o Owner) rowKey() (string, bool) {
	if o.Kind != OwnerArtifact {
		return o.Key, true
	}
	id, err := uuid.Parse(o.Key)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// lockOwnerRows takes the rows of owners, whose advisory locks the transaction holds, at level
// (ownerRowLocks), one statement per kind, in one round trip before any read those locks guard,
// and runs read behind them in that round trip when it is not nil: the mode a fleet with a process
// of an earlier build in it needs (ownerLockAdvisoryAndRow), since that build's writers serialise
// on the row and never take the advisory lock. It answers the owners it holds: every one of owners,
// or at a level that skips, in owners' order, those whose row it took, so an owner whose row a
// writer of that build holds, or that has no row, is left out.
//
// The row locks of a level that waits run in a row lock's bounded wait (rowLockWait,
// sendWithinBound), sent in their batch ahead of read, so a row that build's writer holds past
// the bound is refused ErrPoolBusy, as an advisory lock held past it is; the refusal aborts the
// caller's transaction, which then rolls back, and the advisory locks lockOwnersAt took stay held
// until it does. The row locks assign the transaction its id, as LockOwner says, and are the
// transaction's own, so its write phase's update of an owner's row (the append's last_seq, behind
// events.lockEventOwner) makes no MultiXact of a savepoint's lock and that update.
func lockOwnerRows(ctx context.Context, q OwnerLocker, level *ownerLockLevel, owners []Owner, read *guardedRead) ([]Owner, error) {
	rowLocks := ownerRowLocks(owners, level)
	var taken map[Owner]bool
	if level.skips {
		taken = make(map[Owner]bool, len(owners))
	}
	if len(rowLocks) > 0 || read != nil {
		var wait boundedWait
		if !level.skips && len(rowLocks) > 0 {
			wait = rowLockWait(ctx)
		}
		locks := make([]boundedLock, len(rowLocks))
		for index, lock := range rowLocks {
			locks[index] = boundedLock{sql: lock.statement, args: []any{lock.keys}, take: func(results pgx.BatchResults) error {
				if err := lock.take(results, level, taken); err != nil {
					return lock.failed(err)
				}
				return nil
			}}
		}
		results, err := sendWithinBound(ctx, q, wait, locks, read)
		if err != nil {
			return nil, err
		}
		if read != nil {
			err = read.answer(results)
		} else {
			err = results.Close()
		}
		if err != nil {
			return nil, err
		}
	}
	if !level.skips {
		return owners, nil
	}
	var held []Owner
	for _, owner := range owners {
		if key, ok := owner.rowKey(); ok && taken[Owner{Kind: owner.Kind, Key: key}] {
			held = append(held, owner)
		}
	}
	return held, nil
}

// take reads the row lock's answer at level, the next result of results. At a level that waits,
// whose statement takes every row it names, the answer is left unread and taken is nil; at one
// that skips, every row the statement took is marked in taken.
func (l ownerRowLock) take(results pgx.BatchResults, level *ownerLockLevel, taken map[Owner]bool) error {
	if !level.skips {
		_, err := results.Exec()
		return err
	}
	rows, err := results.Query()
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, key := range keys {
		taken[Owner{Kind: l.kind, Key: key}] = true
	}
	return nil
}

// OwnersLockShared is the statement LockOwnersShared runs, exported so a test can watch for a
// session waiting on it in pg_stat_activity (api/inbox_dismiss_race_test.go).
const OwnersLockShared = ownersLockShared

// OwnerLockStatement is the statement LockOwner and LockOwners run in a read phase, for one owner
// or several, exported so a test can find a session waiting on it in pg_stat_activity
// (cmd/dispatch/shutdown_test.go).
const OwnerLockStatement = ownersLock

// LockOwnersShared takes the locks of every owner in owners in shared mode, in one statement. A
// shared lock conflicts with LockOwner's exclusive one exactly as `for share` conflicted with the
// writers' `for no key update` on the owner row, and not with another shared one: a transaction
// that writes only rows every owner writer orders itself against, and must not interleave with
// one (the Inbox's Mark all read and its dismissal, api's lockInboxReadOwners), waits for the
// writer that holds the owner and holds off the next one. The reads it guards come in statements
// after this one, as LockOwner's do, and while the fleet read answers that an earlier build is in
// the fleet, the owners' rows are taken `for share` between the two (lockOwnerRows),
// which is the lock that build's Inbox takes. The owners are sorted and deduplicated by the lock's
// key as LockOwners sorts them, so a transaction taking several waits for them in the one order
// every other transaction does. It runs the same statement however many owners it takes, so a
// session waiting on it is told apart by the owners it named rather than by its text. Owners of
// none takes nothing.
func LockOwnersShared(ctx context.Context, q OwnerLocker, owners ...Owner) error {
	if len(owners) == 0 {
		return nil
	}
	_, _, err := lockOwnersAt(ctx, q, ownerLockShared, owners, nil)
	return err
}

// TryLockOwners tries each owner's exclusive lock without waiting and answers, in owners' order,
// the owners whose lock this transaction now holds; an owner another transaction holds is left out.
// It is the advisory form of `for no key update skip locked`: a background pass takes the owners
// nobody is writing and leaves the rest. As with LockOwner, what the pass reads of an owner it took
// is read in a statement after this one. Run on the pool, outside a transaction, a lock it takes is
// released when its statement ends, which makes it a probe of whether anyone holds the owner.
//
// While the fleet read answers that an earlier build is in the fleet, the rows of the owners it
// took are tried too, `for no key update skip locked`, and an owner whose row it did not take - a
// writer of that build holds it, or it has no row - is left out of the answer. Its advisory lock
// stays held to the end of the transaction, as a transaction lock does: that holds off this
// build's writers of that owner for the rest of the pass, and excludes nothing else. While the
// fleet read answers that none is, the statement holds the owner lock gate shared, which it tried
// before the first owner as every read-phase owner lock does (LockOwner): only the owners' locks
// are tried.
func TryLockOwners(ctx context.Context, q OwnerLocker, owners ...Owner) ([]Owner, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	taken, _, err := lockOwnersAt(ctx, q, ownerLockTry, owners, nil)
	return taken, err
}
