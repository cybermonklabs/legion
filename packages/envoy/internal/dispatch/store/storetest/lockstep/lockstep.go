// Package lockstep follows a writer of the node and tuple store through the store's lock order,
// written on api/inbox_read.go's markInboxReadThreads. A test holds one row of each step in a
// transaction of its own (Hold), runs the real writer, lets go of each hold the writer queues
// behind, one at a time, reads where the writer waits and what it holds each time
// (RequireStoppedAt), and holds the order it queued in to that lock order
// (RequireTheStoreLockOrder).
//
// The backfill's tests (graphcensus), and the tests of other writers of the store, follow their
// writers through it. It imports no writer's package, so graphcensus's own tests can import it. It
// writes no table: the source scans that hold every writer of the store and of its projections to
// a register (graphstore's TestEveryStoreWriteCallSiteIsListed, docs'
// requireEveryPassReadsItsSource) read every Go file but the tests, so a fixture that writes one -
// a seed, a hold written through the store's writer - stays in the _test.go files of the package
// that uses it.
//
// It also holds the register of the store's writers, each beside the test that drives its lock
// steps (StoreWriteCallSites), which graphstore's and graphcensus's tests both read.
package lockstep

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Wait bounds each wait of a lock-step test: for the writer to queue behind the next hold once the
// one before it let go, and for the writer to answer once it has queued behind the last. It is far
// above what the work takes; a wait that passes it is the writer waiting on something the test
// does not hold.
const Wait = 30 * time.Second

// The steps a hold stands for, named as a failure names them: the owner's lock; the two steps a
// writer under the revision fence (R23) adds after it, the document's document_block_versions row
// and the projection rows it reads again under those two locks, `for update`, before it writes
// anything (graphcensus's heldBlockRows, heldBlockRefRows); the node rows; the tuple append; the
// projection rows a writer writes; the head rows (issue_head_nodes, graphstore.QueueHeads), the last
// rows a write takes; and the project of an event. RequireTheStoreLockOrder holds the order a writer
// takes them in to the invariants on api/inbox_read.go's markInboxReadThreads, not to one sequence
// of these steps.
//
// A hold holds the rows of its own step alone: a hold written through the store's writer writes no
// head row (a fixture's write phase of QueueWrite, not graphstore.Write), and a head row is held by
// a hold of StepHead, so a writer queued behind a hold queued at that hold's step and no other.
const (
	StepOwner      = "the owner's lock"
	StepFence      = "the revision fence's document_block_versions row"
	StepHeld       = "the projection rows, read again under the fence"
	StepNode       = "the node rows"
	StepTuple      = "the tuple append"
	StepProjection = "the projection rows"
	StepHead       = "the head rows"
	StepEvent      = "the project of the event"
)

// fencedSteps are the steps whose rows the revision fence guards: a writer under it takes the
// document's document_block_versions row before it reads a projection row again under it, or
// writes a node, a tuple, a projection row or a head row.
var fencedSteps = []string{StepHeld, StepNode, StepTuple, StepProjection, StepHead}

// headAfter are the steps whose rows a writer takes before its head rows: every row of its write
// but the owner's lock, which comes first anyway, and the project of an event, which the backfill's
// writers take none of.
var headAfter = []string{StepFence, StepHeld, StepNode, StepTuple, StepProjection}

// Hold is one row of a step of the store's lock order, held in a transaction of its own while a
// writer runs, and what the test expects of the writer while it waits behind it.
type Hold struct {
	// Name is the row as a failure names it, and Step the step of the lock order it stands for.
	Name, Step string
	// Taken is whether the writer takes the row. A row it does not take is held for the whole
	// run, and the writer must never queue behind it.
	Taken bool
	// Kind and ID order the row among the writer's other rows of its step: a node's kind and id,
	// an owner row's owner key split at its colon ("issue", "CORE-1"), a fence row's document id.
	Kind, ID string
	// Names is, for a projection row the writer writes, the hold of the node row its foreign key
	// names, when the writer writes that node, and nil otherwise.
	Names *Hold
	// Statement is how the statement the writer waits at behind this hold starts, lower-cased
	// with its whitespace collapsed: the statement that takes the step.
	Statement string
	// Unlocked are the tables the writer's transaction may not hold a lock on while it waits
	// behind this hold: those of its later steps, and those of a step it does not take.
	Unlocked []string
	// Tx is the transaction the row is held in.
	Tx pgx.Tx

	released bool
}

// Begin opens the transaction hold holds its row in. Its release is registered as a cleanup too,
// so a failed assertion never leaves the writer queued on a lock nothing will drop.
func Begin(t testing.TB, database *store.Store, hold *Hold) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the transaction holding %s: %v", hold.Name, err)
	}
	hold.Tx = tx
	t.Cleanup(hold.Release)
}

// Release lets go of the row, once.
func (h *Hold) Release() {
	if h.released {
		return
	}
	h.released = true
	_ = h.Tx.Rollback(context.Background())
}

// Released is whether the row has been let go of.
func (h *Hold) Released() bool { return h.released }

// HoldRow holds a row that is already there, with statement, which locks it.
func HoldRow(t testing.TB, database *store.Store, hold *Hold, statement string, args ...any) *Hold {
	t.Helper()
	Begin(t, database, hold)
	tag, err := hold.Tx.Exec(context.Background(), statement, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("hold %s: %d rows, %v", hold.Name, tag.RowsAffected(), err)
	}
	return hold
}

// OwnerLockStatement is how the statement that takes an owner's lock starts, lower-cased with its
// whitespace collapsed as RequireStoppedAt reads it: store.LockOwners' advisory lock.
var OwnerLockStatement = strings.ToLower(store.OwnerLockStatement)

// HoldOwner holds owner's lock, the transaction advisory lock every writer of that owner takes
// first (store.LockOwner), as one of those writers takes it.
func HoldOwner(t testing.TB, database *store.Store, hold *Hold, owner store.Owner) *Hold {
	t.Helper()
	Begin(t, database, hold)
	if err := store.LockOwner(context.Background(), hold.Tx, owner); err != nil {
		t.Fatalf("hold %s: %v", hold.Name, err)
	}
	return hold
}

// RequireStoppedAt reads where the writer is while it waits behind hold: the statement it waits
// at, which has to be the statement that takes hold's step, and the tables its transaction holds a
// lock on, none of which may be a table of a later step. Postgres keeps every table lock a
// transaction takes until it ends, so a step the writer has already taken shows as a lock on its
// table, where the statement alone names only the step the writer is stopped at. run names the
// writer for a failure.
func RequireStoppedAt(t testing.TB, ctx context.Context, database *store.Store, pid uint32, run string, hold *Hold) {
	t.Helper()
	running := strings.ToLower(WaitingStatement(t, database, pid))
	locked := TablesLockedBy(t, ctx, database, pid)
	// The statement is shown by its head, which names it; pg_stat_activity keeps at most its first
	// track_activity_query_size bytes anyway.
	shown := running
	if len(shown) > 120 {
		shown = shown[:120] + "..."
	}
	t.Logf("%s waits behind %s (%s) at %q, holding locks on %v", run, hold.Name, hold.Step, shown, locked)
	if hold.Statement != "" && !strings.HasPrefix(running, hold.Statement) {
		t.Errorf("%s waits behind %s (%s) at %q, which is not the statement that takes that step (it starts %q)",
			run, hold.Name, hold.Step, shown, hold.Statement)
	}
	for _, table := range hold.Unlocked {
		if slices.Contains(locked, table) {
			t.Errorf("%s already holds a lock on %s while it waits behind %s (%s): it ran a statement on %s, a table of a later step or of a step it does not take, before it took %s (it holds locks on %v)",
				run, table, hold.Name, hold.Step, table, hold.Step, locked)
		}
	}
}

// RequireTheStoreLockOrder holds the order the writer queued behind holds in to the invariants
// written on api/inbox_read.go's markInboxReadThreads, as api's requireTheWrittenOrderAdmits holds
// that package's writers to them, and not to one sequence of steps: those invariants leave a tuple
// append wherever its writer puts it, before a node row of the same write as the comment path puts
// its comment's tuples before its author's node, or after a projection row. It holds the writer
// to:
//
//   - no row of a step the writer does not take queued behind: the owner's lock comes first or not
//     at all, and a writer that queued behind one took it after a step it had already taken;
//   - every row of a step the writer takes queued behind once, or the step was not shown to be
//     taken while it was held;
//   - the owner's lock first: no row of another step taken before an owner's lock;
//   - the revision fence's row (R23) after the owner's lock, which the rule before says, and
//     before the rows it guards (fencedSteps);
//   - a node's row before the projection row whose foreign key names it (Hold.Names);
//   - the head rows after every row of the write's other steps but the owner's lock and the
//     event (headAfter): its fence row, the rows it reads again under the fence, its node rows,
//     its tuples and its projection rows;
//   - the project of the event last;
//   - several rows of one step in ascending order: node rows in ascending (kind, id), and owner
//     locks in ascending owner key and fence rows in document id order, the order every writer of
//     several documents takes them in (graphcensus's lockBlockDocuments, events.LockOwnerRows,
//     store.LockOwners).
//
// The third invariant, the event before the tuples it causes, holds of a writer with nothing to
// read: the backfill appends no event, and every cause its tuples carry is an event the log
// already holds.
func RequireTheStoreLockOrder(t testing.TB, run string, holds, queued []*Hold) {
	t.Helper()
	order := HoldNames(queued)
	t.Logf("%s queued behind, in order: %s", run, order)
	at := func(hold *Hold) int { return slices.Index(queued, hold) }
	for _, hold := range holds {
		position := at(hold)
		switch {
		case !hold.Taken && position >= 0:
			t.Errorf("%s queued behind %s (%s), a step it does not take, after %s: it took %s after a step it had already taken, out of the store's lock order (queued in order: %s)",
				run, hold.Name, hold.Step, HoldNames(queued[:position]), hold.Step, order)
		case hold.Taken && position < 0:
			t.Errorf("%s never queued behind %s (%s) (queued in order: %s): the step was not taken while its row was held",
				run, hold.Name, hold.Step, order)
		}
	}
	// before fails the run when it queued behind later, a row it takes, without having queued
	// behind earlier, another row it takes, first: the invariant rule puts earlier first. A row of
	// a step the run takes and never queued behind at all did not come first either, so it counts
	// as taken after.
	before := func(earlier, later *Hold, rule string) {
		t.Helper()
		first, second := at(earlier), at(later)
		if second >= 0 && (first < 0 || first > second) {
			t.Errorf("%s took %s (%s) before %s (%s): %s (queued in order: %s)",
				run, later.Name, later.Step, earlier.Name, earlier.Step, rule, order)
		}
	}
	for _, hold := range holds {
		if !hold.Taken {
			continue
		}
		for _, other := range holds {
			if !other.Taken || other.Step == hold.Step {
				continue
			}
			switch {
			case hold.Step == StepOwner:
				before(hold, other, "the owner's lock comes first")
			case hold.Step == StepFence && slices.Contains(fencedSteps, other.Step):
				before(hold, other, "the revision fence's row comes before the rows it guards")
			case other.Step == StepHead && slices.Contains(headAfter, hold.Step):
				before(hold, other, "the head rows come last, after every node row, tuple and projection row of the write")
			case other.Step == StepEvent:
				before(hold, other, "the project of the event comes last")
			}
		}
		if hold.Names != nil && hold.Names.Taken {
			before(hold.Names, hold, "a node's row comes before the projection row whose foreign key names it")
		}
	}
	for _, ascending := range []struct{ step, rule string }{
		{StepOwner, "several owners' locks are taken in ascending owner key"},
		{StepFence, "several documents' fence rows are taken in document id order"},
		{StepNode, "several node rows are taken in ascending (kind, id)"},
	} {
		var previous *Hold
		for _, hold := range queued {
			if !hold.Taken || hold.Step != ascending.step {
				continue
			}
			if previous != nil && (hold.Kind < previous.Kind || hold.Kind == previous.Kind && hold.ID < previous.ID) {
				t.Errorf("%s took %s before %s: %s, so %s %s before %s %s (queued in order: %s)",
					run, previous.Name, hold.Name, ascending.rule, hold.Kind, hold.ID, previous.Kind, previous.ID, order)
			}
			previous = hold
		}
	}
}

// HoldNames names holds in their order, for a failure or a log line.
func HoldNames(holds []*Hold) string {
	names := make([]string, len(holds))
	for index, hold := range holds {
		names[index] = hold.Name
	}
	return "[" + strings.Join(names, ", ") + "]"
}

// TablesLockedBy names every table backend pid holds or asks a lock on, in name order, which for a
// writer stopped on a lock is every table its transaction has read or written so far. It reads
// pg_locks through the pool, a fresh view on each read, and leaves indexes and sequences out: a
// step is named by the table it writes.
func TablesLockedBy(t testing.TB, ctx context.Context, database *store.Store, pid uint32) []string {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select distinct c.relname::text from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and c.relkind in ('r', 'p')
		order by 1`, pid)
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tables backend %d has locked: %v", pid, err)
	}
	return tables
}

// WaitingStatement is the statement backend pid is running, read through the test's own pool,
// with its whitespace collapsed: once the backend has queued behind a lock, that is the statement
// it waits at.
func WaitingStatement(t testing.TB, database *store.Store, pid uint32) string {
	t.Helper()
	var statement string
	if err := database.Pool.QueryRow(context.Background(),
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&statement); err != nil {
		t.Fatalf("read the statement backend %d waits at: %v", pid, err)
	}
	return strings.Join(strings.Fields(statement), " ")
}

// Follow follows a writer already running on backend pid, whose result arrives on done, through
// holds: it lets go of each hold the writer queues behind, one at a time, reads where the writer
// waits each time (RequireStoppedAt), and once the writer answers holds the order it queued in to
// the store's lock order (RequireTheStoreLockOrder), fails on the writer's own error, and fails on
// a node or tuple row the writer committed with a negative cause (storetest.RequireNoMarkedCause).
// A statement waits on one lock at a time, so a writer queued behind two holds at once is a
// failure of the test's own reading, and a writer that queues behind none and does not answer is
// waiting on something the test does not hold. It answers the holds in the order the writer
// queued behind them.
func Follow(t testing.TB, database *store.Store, run string, pid uint32, done <-chan error, holds []*Hold) []*Hold {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		for _, hold := range holds {
			hold.Release()
		}
	})
	var queued []*Hold
	deadline := time.Now().Add(Wait)
	for {
		select {
		case err := <-done:
			RequireTheStoreLockOrder(t, run, holds, queued)
			if err != nil {
				t.Fatalf("%s failed after it queued behind %s: %v", run, HoldNames(queued), err)
			}
			storetest.RequireNoMarkedCause(t, database, run)
			return queued
		default:
		}
		var behind []*Hold
		for _, hold := range holds {
			if hold.Released() {
				continue
			}
			waits, err := storetest.QueuedBehind(ctx, hold.Tx, pid)
			if err != nil {
				t.Fatalf("read what %s waits on: %v", run, err)
			}
			if waits {
				behind = append(behind, hold)
			}
		}
		switch len(behind) {
		case 0:
			if time.Now().After(deadline) {
				t.Fatalf("%s queued behind none of the holds still held and did not answer within %s (it queued behind %s so far)",
					run, Wait, HoldNames(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			RequireStoppedAt(t, ctx, database, pid, run, behind[0])
			queued = append(queued, behind[0])
			behind[0].Release()
			deadline = time.Now().Add(Wait)
		default:
			t.Fatalf("%s queued behind %d holds at once (%s), which one statement cannot do", run, len(behind), HoldNames(behind))
		}
	}
}
