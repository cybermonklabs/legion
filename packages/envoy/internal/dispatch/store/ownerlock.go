package store

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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
// database - has its row pruned and is not in the fleet again until its next heartbeat's admit,
// whose insert is an arrival. That is the registry's own model: every other decision over the
// fleet (the signing keys, the embedder, the schema admission) counts a process silent past
// processes.LiveWindow as gone.
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
// gives back the locks taken under it. The pool satisfies it too, for a try that holds its lock only
// for its own statement.
type OwnerLocker interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error)
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

// ownerLockMode is which owner lock a transaction of this process took, by its fleet read
// (public.owner_lock_fleet, the owner lock statements above).
type ownerLockMode int32

const (
	// ownerLockModeUnknown is a process that has taken no owner lock yet.
	ownerLockModeUnknown ownerLockMode = iota
	// ownerLockAdvisory is the advisory lock alone, behind the owner lock gate taken shared: no
	// earlier build, which serialises on the owner row, is in the fleet.
	ownerLockAdvisory
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
// this transaction to end before it writes anything, and every owner lock after that arrival reads
// it in the fleet. A try that fails, while an arrival holds the gate or waits for it, takes the
// owner's row as beside an earlier build, so no owner lock waits on an arrival.
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
	legacy, err := lockOwnerKeys(ctx, q, owners)
	if err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	return lockOwnerRows(ctx, q, owners, rowLockExclusive)
}

// lockOwnerKeys takes the advisory locks of owners, at least one, in one statement
// (lockOwnersStatement), and answers its fleet read: whether an earlier build, which serialises on
// the owner row, is in the fleet. It records the answer (noteOwnerLockMode).
func lockOwnerKeys(ctx context.Context, q OwnerLocker, owners []Owner) (bool, error) {
	lock, args := lockOwnersStatement(owners)
	rows, err := q.Query(ctx, lock, args...)
	if err != nil {
		return false, lockOwnersFailed(owners, err)
	}
	legacy, err := scanLegacyFleet(rows)
	if err != nil {
		return false, lockOwnersFailed(owners, err)
	}
	return legacy, nil
}

// scanLegacyFleet reads an owner lock statement's rows to their end, which runs the statement to
// its end and so takes every lock it names, and answers their fleet read.
func scanLegacyFleet(rows pgx.Rows) (bool, error) {
	defer rows.Close()
	legacy := false
	for rows.Next() {
		if err := rows.Scan(nil, &legacy); err != nil {
			return false, err
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	noteOwnerLockMode(legacy)
	return legacy, nil
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
// them, in one statement ahead of read.
//
// The batch is sent before the lock's fleet read is answered, so read has already run when the
// answer says the owners' rows must be taken too: read then ran before the rows were locked, and
// a process of an earlier build may have been writing them. So in that mode its answer is
// dropped unread, and a second batch takes the rows (lockOwnerRows) and runs read again behind
// them: one more round trip, in that mode alone.
func LockOwnersAndRead(ctx context.Context, tx pgx.Tx, owners []Owner, read string, args []any, dest ...any) error {
	batch := &pgx.Batch{}
	lock, lockArgs := lockOwnersStatement(owners)
	batch.Queue(lock, lockArgs...)
	batch.Queue(read, args...)
	results := tx.SendBatch(ctx, batch)
	rows, err := results.Query()
	if err == nil {
		var legacy bool
		if legacy, err = scanLegacyFleet(rows); err == nil && legacy {
			// The read ran ahead of the row locks: drop its answer (Close reads it and keeps only
			// its failure) and read again behind the rows.
			if err := results.Close(); err != nil {
				return err
			}
			return lockOwnerRowsAndRead(ctx, tx, owners, read, args, dest...)
		}
	}
	if err != nil {
		results.Close()
		return lockOwnersFailed(owners, err)
	}
	err = results.QueryRow().Scan(dest...)
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	return err
}

// lockOwnerRowsAndRead takes the rows of owners (lockOwnerRows) and then runs read, in one batch.
func lockOwnerRowsAndRead(ctx context.Context, tx pgx.Tx, owners []Owner, read string, args []any, dest ...any) error {
	batch := &pgx.Batch{}
	locks := ownerRowLocks(owners, rowLockExclusive)
	for _, lock := range locks {
		batch.Queue(lock.statement, lock.keys)
	}
	batch.Queue(read, args...)
	results := tx.SendBatch(ctx, batch)
	for _, lock := range locks {
		if _, err := results.Exec(); err != nil {
			results.Close()
			return lock.failed(err)
		}
	}
	err := results.QueryRow().Scan(dest...)
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	return err
}

// QueueLockOwner queues owner's lock (LockOwner) on a write phase's batch, for a statement after it
// in the same batch that the lock guards: a write that takes its owner's lock again where the
// transaction took it in its read phase, which returns at once. It carries no fleet read: the
// transaction's read phase made the one it acts on. Its failure is wrapped as LockOwner wraps it.
func QueueLockOwner(b *WriteBatch, owner Owner) {
	b.Queue(fmt.Sprintf("lock %s %s", owner.Kind, owner.Key), ownerLock, OwnerLockNamespace, owner.LockKey())
}

// lockOwnersStatement is the read-phase statement that takes the exclusive locks of owners, at
// least one, behind the fleet read and the gate (ownersLock), and its arguments.
func lockOwnersStatement(owners []Owner) (string, []any) {
	return ownersLock, ownerLockArgs(lockKeys(owners))
}

// lockOwnersFailed is err, the failure of lockOwnersStatement's statement, naming what it locked.
func lockOwnersFailed(owners []Owner, err error) error {
	if len(owners) == 1 {
		return fmt.Errorf("lock %s %s: %w", owners[0].Kind, owners[0].Key, err)
	}
	return fmt.Errorf("lock owners %v: %w", lockKeys(owners), err)
}

// lockKeys is each owner's lock key, in owners' order.
func lockKeys(owners []Owner) []string {
	keys := make([]string, len(owners))
	for index, owner := range owners {
		keys[index] = owner.LockKey()
	}
	return keys
}

// rowLock is the level lockOwnerRows takes an owner's row at, the one a build that serialises on
// the row takes for the same work.
type rowLock int

const (
	// rowLockExclusive is `for no key update`, every writer's (LockOwner, LockOwners).
	rowLockExclusive rowLock = iota
	// rowLockShared is `for share`, the Inbox's (LockOwnersShared).
	rowLockShared
	// rowLockTry is `for no key update skip locked`, a background pass's (TryLockOwners).
	rowLockTry
)

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
// among them, in ownerRowKinds' order: a statement of its own for each kind, whose rows its
// `order by` takes in byte order of their keys (an issue's or a project's key collated "C", an
// artifact's id as a uuid, whose order is its text's). A document's id that no uuid spells has no
// row, and is left out rather than refused by the uuid cast.
func ownerRowLocks(owners []Owner, level rowLock) []ownerRowLock {
	locks := make([]ownerRowLock, 0, len(ownerRowKinds))
	for _, kind := range ownerRowKinds {
		var keys []string
		for _, owner := range owners {
			if owner.Kind != kind {
				continue
			}
			key := owner.Key
			if kind == OwnerArtifact {
				id, err := uuid.Parse(key)
				if err != nil {
					continue
				}
				key = id.String()
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			locks = append(locks, ownerRowLock{kind: kind, statement: ownerRowLockStatement(kind, level), keys: keys})
		}
	}
	return locks
}

// ownerRowLockStatement is the statement that takes the rows of kind's owners named by $1 at
// level, each answering its key as text (an artifact's id canonical). These and the write phase's
// events.lockEventOwner are the only statements of this module that lock an owner row
// (api.TestNoForUpdateOnOwnerTables).
func ownerRowLockStatement(kind OwnerKind, level rowLock) string {
	switch kind {
	case OwnerArtifact:
		switch level {
		case rowLockShared:
			return `select id::text from artifacts where id = any($1::uuid[]) order by id for share`
		case rowLockTry:
			return `select id::text from artifacts where id = any($1::uuid[]) order by id for no key update skip locked`
		default:
			return `select id::text from artifacts where id = any($1::uuid[]) order by id for no key update`
		}
	case OwnerIssue:
		switch level {
		case rowLockShared:
			return `select key from issues where key = any($1::text[]) order by key collate "C" for share`
		case rowLockTry:
			return `select key from issues where key = any($1::text[]) order by key collate "C" for no key update skip locked`
		default:
			return `select key from issues where key = any($1::text[]) order by key collate "C" for no key update`
		}
	default:
		switch level {
		case rowLockShared:
			return `select key from projects where key = any($1::text[]) order by key collate "C" for share`
		case rowLockTry:
			return `select key from projects where key = any($1::text[]) order by key collate "C" for no key update skip locked`
		default:
			return `select key from projects where key = any($1::text[]) order by key collate "C" for no key update`
		}
	}
}

// lockOwnerRows takes the rows of owners at level (ownerRowLocks), one statement per kind, after
// their advisory locks and before any read those locks guard: the mode a fleet with a process of
// an earlier build in it needs (ownerLockAdvisoryAndRow), since that build's writers serialise on
// the row and never take the advisory lock.
func lockOwnerRows(ctx context.Context, q OwnerLocker, owners []Owner, level rowLock) error {
	for _, lock := range ownerRowLocks(owners, level) {
		if _, err := q.Exec(ctx, lock.statement, lock.keys); err != nil {
			return lock.failed(err)
		}
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
	rows, err := q.Query(ctx, ownersLockShared, ownerLockArgs(lockKeys(owners))...)
	if err != nil {
		return fmt.Errorf("lock owners %v for share: %w", lockKeys(owners), err)
	}
	legacy, err := scanLegacyFleet(rows)
	if err != nil {
		return fmt.Errorf("lock owners %v for share: %w", lockKeys(owners), err)
	}
	if !legacy {
		return nil
	}
	return lockOwnerRows(ctx, q, owners, rowLockShared)
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
	keys := make([]string, len(owners))
	byKey := make(map[string]Owner, len(owners))
	for index, owner := range owners {
		keys[index] = owner.LockKey()
		byKey[keys[index]] = owner
	}
	rows, err := q.Query(ctx, ownersTryLock, ownerLockArgs(keys)...)
	if err != nil {
		return nil, fmt.Errorf("try owner locks: %w", err)
	}
	legacy := false
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Owner, error) {
		var key string
		if err := row.Scan(&key, &legacy); err != nil {
			return Owner{}, err
		}
		return byKey[key], nil
	})
	if err != nil {
		return nil, fmt.Errorf("try owner locks: %w", err)
	}
	if len(taken) == 0 {
		return nil, nil
	}
	noteOwnerLockMode(legacy)
	if !legacy {
		return taken, nil
	}
	return tryLockOwnerRows(ctx, q, taken)
}

// tryLockOwnerRows tries the rows of owners, whose advisory locks the caller holds, `for no key
// update skip locked` (ownerRowLocks) and answers, in owners' order, those whose row it took.
func tryLockOwnerRows(ctx context.Context, q OwnerLocker, owners []Owner) ([]Owner, error) {
	locked := map[Owner]bool{}
	for _, lock := range ownerRowLocks(owners, rowLockTry) {
		rows, err := q.Query(ctx, lock.statement, lock.keys)
		if err != nil {
			return nil, lock.failed(err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, lock.failed(err)
		}
		for _, key := range keys {
			locked[Owner{Kind: lock.kind, Key: key}] = true
		}
	}
	var taken []Owner
	for _, owner := range owners {
		key := owner.Key
		if owner.Kind == OwnerArtifact {
			if id, err := uuid.Parse(key); err == nil {
				key = id.String()
			}
		}
		if locked[Owner{Kind: owner.Kind, Key: key}] {
			taken = append(taken, owner)
		}
	}
	return taken, nil
}
