package delivery

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The outbox and lock-order tests assert which writes take the change log's lock and when.
// They log hold times without asserting them, since the build farm measures the timing bound.

// The two keys of the change log's lock, pg_advisory_xact_lock(6900, 0) (lockChangesStatement).
const (
	changeLockClass  = 6900
	changeLockObject = 0
)

// clearPending removes any pending changes from the clone's seed so the test reads only its own.
func clearPending(t *testing.T, ctx context.Context, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `delete from delivery_changes_pending`); err != nil {
		t.Fatalf("clear the pending changes: %v", err)
	}
}

// recordPending records count pending issue_facts changes of project, each naming its issue
// `<project>-<n>` for n from 1, in one statement, as a Legion pass of count issues records them.
func recordPending(t *testing.T, ctx context.Context, pool *store.Pool, project string, count int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		select delivery_change_log_pending('issue_facts', null, $1, $1 || '-' || n, null, null)
		from generate_series(1, $2) n
	`, project, count); err != nil {
		t.Fatalf("record %d pending changes of %s: %v", count, project, err)
	}
}

// wideTestPool is a pool of its own on pool's database with room for every connection a test of
// several writers at once holds, beside the shared one's few.
func wideTestPool(t *testing.T, pool *store.Pool) *store.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.MaxConns = 16
	pgxPool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open a wider pool: %v", err)
	}
	t.Cleanup(pgxPool.Close)
	return store.NewPool(pgxPool)
}

// takeChangeLock begins a transaction holding the change log's lock, which the test commits to let
// the paths waiting on it on.
func takeChangeLock(t *testing.T, ctx context.Context, pool *store.Pool) pgx.Tx {
	t.Helper()
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock($1, $2)`, changeLockClass, changeLockObject); err != nil {
		t.Fatalf("take the change log's lock: %v", err)
	}
	return gate
}

// timedCall is a call's error and the moment it answered.
type timedCall struct {
	err error
	at  time.Time
}

// goTimed runs call on a goroutine of its own and answers what it answers, and when.
func goTimed(call func() error) <-chan timedCall {
	done := make(chan timedCall, 1)
	go func() {
		err := call()
		done <- timedCall{err: err, at: time.Now()}
	}()
	return done
}

// rowLockedByAnother reports whether a statement of another transaction finds the row the query
// names locked: the probe takes it `for no key update nowait`, as an update of it would, and is
// refused with lock_not_available (55P03) where another transaction holds it.
func rowLockedByAnother(t *testing.T, ctx context.Context, pool *store.Pool, query string, args ...any) bool {
	t.Helper()
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe: %v", err)
	}
	defer func() { _ = probe.Rollback(context.Background()) }()
	_, err = probe.Exec(ctx, query+` for no key update nowait`, args...)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.As(err, &pgErr) && pgErr.Code == "55P03":
		return true
	default:
		t.Fatalf("probe the row's lock: %v", err)
		return false
	}
}

// requireNotDeadlocked fails the test where err is not nil, saying where it is Postgres's deadlock.
func requireNotDeadlocked(t *testing.T, what string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "40P01":
		t.Errorf("%s deadlocked (SQLSTATE 40P01): %v", what, err)
	case err != nil:
		t.Errorf("%s: %v", what, err)
	}
}

// TestEachPendingChangeIsMovedOnceUnderConcurrentDrains: eight drains at once, from connections of
// their own, over 3,000 pending changes while a writer records 300 more one commit at a time, then
// one drain more: together they move 3,300 rows, the log holds each change exactly once, every
// drain moves the rows it takes in their id order, and nothing is left pending.
func TestEachPendingChangeIsMovedOnceUnderConcurrentDrains(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	clearPending(t, ctx, pool)
	before := latestChange(t, ctx, pool)
	const recorded, trickled, drains = 3000, 300, 8
	recordPending(t, ctx, pool, "BURST", recorded)
	gate := takeChangeLock(t, ctx, pool)

	var moved atomic.Int64
	var running sync.WaitGroup
	for range drains {
		running.Go(func() {
			for range 5 {
				n, err := DrainPending(ctx, pool)
				if err != nil {
					t.Errorf("a drain: %v", err)
					return
				}
				moved.Add(n)
			}
		})
	}
	running.Go(func() {
		for n := 1; n <= trickled; n++ {
			if _, err := pool.Exec(ctx, `select delivery_change_log_pending('issue_facts', null, 'TRICKLE', $1, null, null)`, fmt.Sprintf("TRICKLE-%d", n)); err != nil {
				t.Errorf("record a pending change: %v", err)
				return
			}
		}
	})
	// Two drains must reach the database before either can move a row.
	storetest.WaitForLockWaiters(t, gate, 2)
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	running.Wait()
	last, err := DrainPending(ctx, pool)
	if err != nil {
		t.Fatalf("the last drain: %v", err)
	}
	moved.Add(last)

	if got := moved.Load(); got != recorded+trickled {
		t.Errorf("the drains moved %d rows together, want %d", got, recorded+trickled)
	}
	var changes []Change
	for page := changesAfter(t, ctx, pool, before); len(page) > 0; page = changesAfter(t, ctx, pool, page[len(page)-1].Seq) {
		changes = append(changes, page...)
	}
	seen := map[string]int{}
	var burst []string
	for _, change := range changes {
		seen[change.Ref]++
		if change.Repo == "BURST" {
			burst = append(burst, change.Ref)
		}
	}
	twice := 0
	for ref, times := range seen {
		if times != 1 {
			twice++
			if twice <= 5 {
				t.Errorf("the log holds %s %d times, want once", ref, times)
			}
		}
	}
	if len(seen) != recorded+trickled || len(changes) != recorded+trickled {
		t.Errorf("the log holds %d changes naming %d refs, want %d of each", len(changes), len(seen), recorded+trickled)
	}
	want := make([]string, recorded)
	for n := range recorded {
		want[n] = fmt.Sprintf("BURST-%d", n+1)
	}
	if !slices.Equal(burst, want) {
		t.Errorf("the burst's changes hold seqs in another order than their pending ids")
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from delivery_changes_pending`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d changes are still pending after the last drain", left)
	}
	t.Logf("%d drains at once and a trickle of %d commits moved %d changes, %d of them by the last drain; %d refs held more than once",
		drains, trickled, moved.Load(), last, twice)
}

// TestAReaderCursorNeverSkipsAChangeWhoseWriterCommitsAfterALaterOne: writer A records its pending
// change, writer B records its own after it and commits, a drain moves B's alone (A's is not
// committed), a reader reads the log and keeps its cursor at B's seq, A commits, and a second drain
// moves A's: A's pending id is below B's, its seq above, and the reader reading from its cursor
// sees A's change. Each change reaches the reader exactly once.
func TestAReaderCursorNeverSkipsAChangeWhoseWriterCommitsAfterALaterOne(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	clearPending(t, ctx, pool)
	cursor := latestChange(t, ctx, pool)
	record := func(tx pgx.Tx, ref string) int64 {
		t.Helper()
		var id int64
		if err := tx.QueryRow(ctx, `select delivery_change_log_pending('issue_facts', null, 'CURSOR', $1, null, null)`, ref).Scan(&id); err != nil {
			t.Fatalf("record %s's pending change: %v", ref, err)
		}
		return id
	}
	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Rollback(context.Background()) }()
	aID := record(a, "CURSOR-A")
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bID := record(b, "CURSOR-B")
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if aID >= bID {
		t.Fatalf("A's pending id %d is not below B's %d, so the writers did not commit out of id order", aID, bID)
	}

	read := func(what string) []Change {
		t.Helper()
		if _, err := DrainPending(ctx, pool); err != nil {
			t.Fatalf("drain %s: %v", what, err)
		}
		changes := changesAfter(t, ctx, pool, cursor)
		if len(changes) > 0 {
			cursor = changes[len(changes)-1].Seq
		}
		return changes
	}
	first := read("before A commits")
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := read("after A commits")

	seen := map[string]int{}
	for _, change := range slices.Concat(first, second) {
		seen[change.Ref]++
	}
	if len(first) != 1 || first[0].Ref != "CURSOR-B" {
		t.Errorf("the reader's first read, between the drains, saw %+v, want B's change alone", first)
	}
	if len(second) != 1 || second[0].Ref != "CURSOR-A" {
		t.Errorf("the reader's read from its cursor after A committed saw %+v, want A's change", second)
	}
	if seen["CURSOR-A"] != 1 || seen["CURSOR-B"] != 1 {
		t.Errorf("the reader saw A's change %d times and B's %d, want each once", seen["CURSOR-A"], seen["CURSOR-B"])
	}
	if len(first) == 1 && len(second) == 1 {
		t.Logf("pending ids A %d < B %d; seqs B %d < A %d: seq follows the drains, and the cursor at B's seq still reads A's",
			aID, bID, first[0].Seq, second[0].Seq)
	}
}

// drainTransaction is one transaction that inserted change rows into delivery_changes, as
// recordDrainTransactions records it: its id, how many rows it inserted, and whether it held the
// change log's lock while it did.
type drainTransaction struct {
	txid  int64
	moved int
	holds bool
}

// recordDrainTransactions makes every statement that inserts into delivery_changes record, in a
// table of the test's own, its transaction's id, how many rows it inserted and whether its backend
// held the change log's lock then. It returns a reader of the records in insertion order.
func recordDrainTransactions(t *testing.T, ctx context.Context, pool *store.Pool) func() []drainTransaction {
	t.Helper()
	for _, statement := range []string{
		`create table outbox_test_drains (id bigserial primary key, txid bigint not null, moved integer not null, holds boolean not null)`,
		fmt.Sprintf(`create function outbox_test_record_drain() returns trigger language plpgsql as $$
		begin
			insert into outbox_test_drains (txid, moved, holds)
			select txid_current(), count(*), exists (
				select 1 from pg_locks where locktype = 'advisory' and classid = %d and objid = %d and objsubid = 2
				  and pid = pg_backend_pid() and granted)
			from inserted;
			return null;
		end $$`, changeLockClass, changeLockObject),
		`create trigger outbox_test_record_drain after insert on delivery_changes
			referencing new table as inserted for each statement execute function outbox_test_record_drain()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("record the drain's transactions: %v", err)
		}
	}
	return func() []drainTransaction {
		t.Helper()
		rows, err := pool.Query(ctx, `
			with taken as (delete from outbox_test_drains returning *)
			select txid, moved, holds from taken order by id`)
		if err != nil {
			t.Fatalf("read the drain's transactions: %v", err)
		}
		recorded, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (drainTransaction, error) {
			var transaction drainTransaction
			err := row.Scan(&transaction.txid, &transaction.moved, &transaction.holds)
			return transaction, err
		})
		if err != nil {
			t.Fatalf("read the drain's transactions: %v", err)
		}
		return recorded
	}
}

// TestTheDrainMovesABurstABatchPerTransaction: DrainPending moves a burst a batch of drainBatch
// rows at a time, each batch a transaction of its own that holds the change log's lock, so no burst
// holds the lock for longer than one batch takes. A burst of 1,400 pending changes, the rows a
// Legion pass over 1,400 issues records, is 6 transactions, and a burst of 10,000 is 40, each
// moving at most 250 rows under the lock, each its own transaction id. How long each transaction
// held the lock is logged from the samples and asserted nowhere: the wall clock is the build
// farm's measure (k20), which a busy runner would fail here. A drain with nothing pending moves
// nothing.
func TestTheDrainMovesABurstABatchPerTransaction(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	clearPending(t, ctx, pool)
	if moved, err := DrainPending(ctx, pool); err != nil || moved != 0 {
		t.Fatalf("a drain of an empty outbox moved %d, %v; want 0", moved, err)
	}
	transactions := recordDrainTransactions(t, ctx, pool)
	// The contract the test states, written out rather than read from drainBatch, so a batch that
	// grows past it fails here: at most 250 rows a transaction, 6 transactions for 1,400 and 40 for
	// 10,000.
	const batchRows = 250
	for _, burst := range []struct{ rows, transactions int }{{1400, 6}, {10000, 40}} {
		recordPending(t, ctx, pool, fmt.Sprintf("BATCHED%d", burst.rows), burst.rows)
		stop := storetest.SampleAdvisoryHolds(t, pool, changeLockClass, changeLockObject)
		moved, err := DrainPending(ctx, pool)
		holds := stop()
		if err != nil || moved != int64(burst.rows) {
			t.Fatalf("DrainPending of %d pending changes moved %d, %v", burst.rows, moved, err)
		}
		recorded := transactions()
		want := burst.transactions
		ids := map[int64]bool{}
		total, unlocked, oversized := 0, 0, 0
		for _, transaction := range recorded {
			ids[transaction.txid] = true
			total += transaction.moved
			if !transaction.holds {
				unlocked++
			}
			if transaction.moved > batchRows {
				oversized++
			}
		}
		if len(recorded) != want || len(ids) != want || total != burst.rows {
			t.Errorf("the drain of %d moved %d rows in %d statements of %d transactions, want %d rows in %d transactions of one statement each",
				burst.rows, total, len(recorded), len(ids), burst.rows, want)
		}
		if oversized > 0 {
			t.Errorf("%d of the drain's transactions moved more than %d rows", oversized, batchRows)
		}
		if unlocked > 0 {
			t.Errorf("%d of the drain's transactions moved rows without holding the change log's lock", unlocked)
		}
		held := make([]string, len(holds))
		for i, hold := range holds {
			held[i] = fmt.Sprintf("%.1fms", float64(hold.Held().Microseconds())/1000)
		}
		sizes := make([]int, len(recorded))
		for i, transaction := range recorded {
			sizes[i] = transaction.moved
		}
		t.Logf("the drain of %d pending changes moved them in %d transactions of %v rows; the samples saw %d of them hold the lock, for %v",
			burst.rows, len(recorded), sizes, len(holds), held)
	}
}

// TestADrainBatchMovesOneBatchAndSaysWhetherRowsRemain checks that DrainPendingBatch
// moves one batch of the lowest pending ids and no more, and says
// rows may remain exactly when a whole batch moved, which is when DrainPending moves another. Of
// 260 pending changes it moves 250, then 10, then none; of 250 it moves 250 and says more may
// remain, then finds none. Each batch is the lowest ids still pending, in id order.
func TestADrainBatchMovesOneBatchAndSaysWhetherRowsRemain(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	clearPending(t, ctx, pool)
	// The contract the test states, written out rather than read from drainBatch, as
	// TestTheDrainMovesABurstABatchPerTransaction writes it.
	const batchRows = 250
	type batch struct {
		moved int64
		more  bool
	}
	for _, burst := range []struct {
		rows    int
		batches []batch
	}{
		{batchRows + 10, []batch{{batchRows, true}, {10, false}, {0, false}}},
		{batchRows, []batch{{batchRows, true}, {0, false}}},
	} {
		project := fmt.Sprintf("ONEBATCH%d", burst.rows)
		recordPending(t, ctx, pool, project, burst.rows)
		// A delayed pending change must not age out before an earlier sequence in the log.
		if _, err := pool.Exec(ctx, `update delivery_changes_pending set at = now() - interval '25 hours'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into delivery_changes (kind) values ('settings')`); err != nil {
			t.Fatal(err)
		}
		taken := 0
		for index, want := range burst.batches {
			var head int64
			if err := pool.QueryRow(ctx, `select coalesce(max(seq), 0) from delivery_changes`).Scan(&head); err != nil {
				t.Fatalf("read the log's head: %v", err)
			}
			moved, more, err := DrainPendingBatch(ctx, pool)
			if err != nil || moved != want.moved || more != want.more {
				t.Errorf("batch %d of %d pending changes moved %d and said more %t (%v), want %d and %t",
					index, burst.rows, moved, more, err, want.moved, want.more)
			}
			rows, err := pool.Query(ctx, `select ref from delivery_changes where seq > $1 order by seq`, head)
			if err != nil {
				t.Fatalf("read the moved changes: %v", err)
			}
			refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("read the moved changes: %v", err)
			}
			wanted := make([]string, want.moved)
			for i := range wanted {
				wanted[i] = fmt.Sprintf("%s-%d", project, taken+i+1)
			}
			if !slices.Equal(refs, wanted) {
				t.Errorf("batch %d of %d pending changes moved %v, want the %d lowest pending ids in id order, %v", index, burst.rows, refs, want.moved, wanted)
			}
			taken += len(refs)
			var early int
			if err := pool.QueryRow(ctx, `
				select count(*) from delivery_changes moved
				where moved.seq > $1 and exists (
					select 1 from delivery_changes earlier where earlier.seq < moved.seq and earlier.at > moved.at
				)`, head).Scan(&early); err != nil {
				t.Fatal(err)
			}
			if early != 0 {
				t.Errorf("batch %d moved %d changes whose at precedes an earlier seq", index, early)
			}
		}
	}

	t.Run("after waiting for the change log lock", func(t *testing.T) {
		recordPending(t, ctx, pool, "LATE", 1)
		gate := takeChangeLock(t, ctx, pool)
		drained := goTimed(func() error {
			n, more, err := DrainPendingBatch(ctx, pool)
			if err != nil {
				return err
			}
			if n != 1 || more {
				return fmt.Errorf("drained %d, more %t; want one and false", n, more)
			}
			return nil
		})
		storetest.WaitForLockWaiters(t, gate, 1)
		var earlier Change
		// A direct writer can stamp a change after the drain's transaction has already started.
		if err := gate.QueryRow(ctx, `insert into delivery_changes (kind) values ('settings') returning seq, at`).Scan(&earlier.Seq, &earlier.At); err != nil {
			t.Fatal(err)
		}
		if err := gate.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := testwait.Receive(t, "the waiting drain to finish", drained).err; err != nil {
			t.Fatal(err)
		}
		got := changesAfter(t, ctx, pool, earlier.Seq)
		if len(got) != 1 || got[0].Ref != "LATE-1" || got[0].At.Before(earlier.At) {
			t.Errorf("drain wrote %+v after seq %d at %v; want LATE-1 stamped no earlier", got, earlier.Seq, earlier.At)
		}
	})
}

// TestARunsJobsWriteTakesTheChangeLogLockLast: a run's jobs write (UpsertRunJobs) of 400 jobs, sent
// while the change log's lock is held elsewhere, waits for the lock holding its run's row and the
// row of each of its jobs already, since it takes the lock once, in the statement's change
// triggers, after its rows; let go, it lands. The run's 400 jobs are written unfinished first, so
// the gated write updates and locks each existing row, and the run carries a jobs-unfetchable mark
// that the write clears, so its run row moves too. A write that cleared the mark in a statement of
// its own before its jobs' (main's two statements before INFRA-5577) would take the lock in that
// statement's trigger, holding the run's row and none of its jobs', and take it again in the jobs'
// statement: another transaction would then find the 400 job rows unlocked. The time from the
// lock's release to its answer, which bounds its hold from above, is logged and asserted nowhere:
// the wall clock is the build farm's measure (k20). Its deadlock test against a run write of the
// same run is TestAJobsWriteAndARunWriteOfOneRunNeverDeadlock.
func TestARunsJobsWriteTakesTheChangeLogLockLast(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := UpsertRun(ctx, pool, changeTestRun(811, DeliveryRunKindDeploy, started, started)); err != nil {
		t.Fatalf("UpsertRun: %v", err)
	}
	unfinished := make([]DeliveryRunJob, 400)
	for i := range unfinished {
		unfinished[i] = DeliveryRunJob{Repo: "acme/widgets", RunID: 811, Name: fmt.Sprintf("job-%d", i), StartedAt: &started}
	}
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 811, unfinished, false); err != nil {
		t.Fatalf("write the run's jobs unfinished: %v", err)
	}
	// MarkRunJobsUnfetchable refuses a run that holds job rows, so the mark is set by hand: what
	// matters here is that the write's clearing of it moves the run's row, which the run's change
	// trigger logs (0100's delivery_runs_changed ignores jobs_complete alone).
	if _, err := pool.Exec(ctx, `
		update delivery_runs set jobs_unfetchable_at = now(), jobs_unfetchable_reason = 'a listing that failed'
		where repo = 'acme/widgets' and run_id = 811
	`); err != nil {
		t.Fatalf("mark the run's jobs unfetchable: %v", err)
	}
	finished, success := started.Add(10*time.Minute), DeliveryJobConclusionSuccess
	jobs := make([]DeliveryRunJob, len(unfinished))
	for i := range jobs {
		jobs[i] = DeliveryRunJob{Repo: "acme/widgets", RunID: 811, Name: fmt.Sprintf("job-%d", i), StartedAt: &started, CompletedAt: &finished, Conclusion: &success}
	}

	gate := takeChangeLock(t, ctx, pool)
	done := goTimed(func() error { return UpsertRunJobs(ctx, pool, "acme/widgets", 811, jobs, true) })
	// The jobs write waits for the change log's lock.
	storetest.WaitForLockWaiters(t, gate, 1)
	if !rowLockedByAnother(t, ctx, pool, `select 1 from delivery_runs where repo = 'acme/widgets' and run_id = 811`) {
		t.Error("the jobs write waits for the change log's lock holding no lock of its run's row: it takes the change log's lock before its rows")
	}
	if free := jobRowsFreeToAnother(t, ctx, pool, "acme/widgets", 811); free != 0 {
		t.Errorf("the jobs write waits for the change log's lock while another transaction can lock %d of its %d job rows: it takes the change log's lock before its jobs' rows", free, len(jobs))
	}
	released := time.Now()
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the lock go: %v", err)
	}
	answer := testwait.Receive(t, "the jobs write to finish", done)
	if answer.err != nil {
		t.Fatalf("UpsertRunJobs: %v", answer.err)
	}
	held := answer.at.Sub(released)
	stored, err := ListRunJobs(ctx, pool, "acme/widgets", 811)
	if err != nil || len(stored) != len(jobs) {
		t.Fatalf("jobs after the write = %d, %v; want %d", len(stored), err, len(jobs))
	}
	t.Logf("the jobs write of %d jobs answered at most %s after the change log's lock was let go", len(jobs), held.Round(time.Microsecond))
}

// jobRowsFreeToAnother is how many of the run's job rows another transaction can lock: it takes
// them `for no key update skip locked`, as an update of them would, and counts the rows no other
// transaction holds.
func jobRowsFreeToAnother(t *testing.T, ctx context.Context, pool *store.Pool, repo string, runID int64) int {
	t.Helper()
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe: %v", err)
	}
	defer func() { _ = probe.Rollback(context.Background()) }()
	var free int
	if err := probe.QueryRow(ctx, `
		select count(*) from (
			select 1 from delivery_run_jobs where repo = $1 and run_id = $2 for no key update skip locked
		) free
	`, repo, runID).Scan(&free); err != nil {
		t.Fatalf("probe the job rows' locks: %v", err)
	}
	return free
}

// TestTheDeployedWriteTakesTheChangeLogLockLastAndNeverDeadlocksWithAPullRequestWrite: the
// issue.deployed write of a pull request (recordDeployedEvent), sent while the change log's lock
// is held elsewhere, appends its event and locks the pull request's row in the marker's statement,
// its last, before it waits there for the lock; a one-statement write of the same pull request
// (UpsertPullRequest, its row's lock and then the change log's) waits behind it. Let go, both land,
// neither a deadlock; the time from the release to the deployed write's answer, which bounds its
// hold from above, is logged. Taking the lock first, it would wait for the pull request's row the
// other write holds while that write waits for the lock it holds.
func TestTheDeployedWriteTakesTheChangeLogLockLastAndNeverDeadlocksWithAPullRequestWrite(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	seedDeployedIssues(t, ctx, pool)
	merged := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	seedMergedPullRequest(t, ctx, pool, "acme/widgets", 21, merged, deployedIssue)
	broker := events.NewBroker()
	shipped := shippedPullRequest{Repo: "acme/widgets", Number: 21, IssueKey: deployedIssue, RunID: 901, CompletedAt: merged.Add(time.Hour)}
	issue := deployedIssue
	created := merged.Add(-time.Hour)
	retitled := DeliveryPullRequest{
		Repo: "acme/widgets", Number: 21, Title: deployedIssue + ": a widget, retitled", URL: "https://github.com/acme/widgets/pull/21",
		Author: "octocat", CreatedAt: &created, MergedAt: &merged, IssueKey: &issue,
	}

	gate := takeChangeLock(t, ctx, pool)
	var wrote bool
	deployed := goTimed(func() error {
		var err error
		wrote, err = recordDeployedEvent(ctx, pool, broker, shipped)
		return err
	})
	// The deployed write waits for the change log's lock.
	storetest.WaitForLockWaiters(t, gate, 1)
	if !rowLockedByAnother(t, ctx, pool, `select 1 from delivery_pull_requests where repo = 'acme/widgets' and number = 21`) {
		t.Error("the deployed write waits for the change log's lock holding no lock of its pull request's row: it takes the change log's lock before its rows")
	}
	retitle := goTimed(func() error { return UpsertPullRequest(ctx, pool, retitled) })
	// The pull request's write waits, behind the change log's lock or the deployed write.
	storetest.WaitForLockWaiters(t, gate, 2)
	released := time.Now()
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the lock go: %v", err)
	}
	deployedAnswer := testwait.Receive(t, "the deployed write to finish", deployed)
	retitleAnswer := testwait.Receive(t, "the retitle to finish", retitle)
	requireNotDeadlocked(t, "the deployed write", deployedAnswer.err)
	requireNotDeadlocked(t, "the pull request's write", retitleAnswer.err)
	if t.Failed() {
		return
	}
	held := deployedAnswer.at.Sub(released)
	if !wrote || deployedEventAt(t, ctx, pool, "acme/widgets", 21) == nil || len(deployedEvents(t, ctx, pool, deployedIssue)) != 1 {
		t.Errorf("the deployed write wrote %t; want the row marked and one issue.deployed on %s", wrote, deployedIssue)
	}
	var title string
	if err := pool.QueryRow(ctx, `select title from delivery_pull_requests where repo = 'acme/widgets' and number = 21`).Scan(&title); err != nil || title != retitled.Title {
		t.Errorf("the pull request's title is %q, %v; want %q", title, err, retitled.Title)
	}
	t.Logf("the deployed write waited for the change log's lock holding its pull request's row, the pull request's write behind it; it held the lock at most %s", held.Round(time.Microsecond))
}

// inGroupTransaction answers a call that runs write in a transaction of its own and commits it, as
// each session group route does (api/delivery_session_groups.go).
func inGroupTransaction(ctx context.Context, pool *store.Pool, write func(pgx.Tx) error) func() error {
	return func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := write(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
}

// clearSessionGroups removes the named groups from the clone's seed.
func clearSessionGroups(t *testing.T, pool *store.Pool, names ...string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `delete from delivery_session_groups where name = any($1)`, names); err != nil {
		t.Fatalf("clear the groups: %v", err)
	}
}

// TestASessionGroupWriteReadsBeforeItTakesTheChangeLogLock: a session group's create runs every
// read that decides it, the title rule's read of every pull request's sessions among them, under
// the session groups' own lock and before it takes the change log's lock, so it holds the change
// log's lock for its writes alone. The test holds delivery_pull_requests in access exclusive mode,
// which the title rule's read waits for: while the create waits there, it holds the session
// groups' lock, and the change log's lock is free to another transaction. Let on, the create lands;
// each hold of the change log's lock the samples saw from the release on is logged, with the
// statement its backend ran last, and asserted nowhere: the wall clock is the build farm's measure
// (k20).
func TestASessionGroupWriteReadsBeforeItTakesTheChangeLogLock(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	seedDeliverySettings(t, ctx, pool)
	clearSessionGroups(t, pool, "Reading crew")
	actor := model.Actor{Kind: "system", ID: "delivery-test"}

	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	defer func() { _ = gate.Rollback(context.Background()) }()
	if _, err := gate.Exec(ctx, `lock table delivery_pull_requests in access exclusive mode`); err != nil {
		t.Fatalf("hold the pull requests from every read: %v", err)
	}
	create := goTimed(inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		_, err := CreateSessionGroup(ctx, tx, "Reading crew", []string{"reading-s1"}, actor)
		return err
	}))
	// The group's create reads the pull requests' sessions.
	storetest.WaitForLockWaiters(t, gate, 1)
	probe, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the probe: %v", err)
	}
	var changesFree, groupsFree bool
	err = probe.QueryRow(ctx, `select pg_try_advisory_xact_lock(6900, 0), pg_try_advisory_xact_lock(hashtext('delivery-session-groups'))`).Scan(&changesFree, &groupsFree)
	_ = probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("probe the locks: %v", err)
	}
	if !changesFree {
		t.Error("the group's create reads the pull requests' sessions holding the change log's lock: it takes the lock before its reads")
	}
	if groupsFree {
		t.Error("the group's create reads the pull requests' sessions without the session groups' lock, which two writes' title checks need")
	}
	stop := storetest.SampleAdvisoryHolds(t, pool, changeLockClass, changeLockObject)
	released := time.Now()
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the reads on: %v", err)
	}
	answer := testwait.Receive(t, "the group create to finish", create)
	holds := stop()
	if answer.err != nil {
		t.Fatalf("the group's create: %v", answer.err)
	}
	for _, hold := range holds {
		t.Logf("the change log's lock: %s", hold)
	}
	t.Logf("the group's create waited for its read of the pull requests holding the session groups' lock and not the change log's; let on, it answered %s later", answer.at.Sub(released).Round(time.Microsecond))
}

// TestASessionGroupWriteTakesTheChangeLogLockBeforeItsRows: a session group's update, sent while the
// change log's lock is held elsewhere, waits for it holding the session groups' own lock and no row
// of the group tables, since it takes the change log's lock after its reads and before its first
// write: the group tables' one position for the lock. A run write of the intake (a one-statement
// write, its row's lock and then the change log's) waits behind the lock, and a second group's
// create behind the session groups' lock. Let go, all three land, none a deadlock; the time from
// the release to the update's answer, which bounds its hold of the lock from above, is logged.
func TestASessionGroupWriteTakesTheChangeLogLockBeforeItsRows(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	seedDeliverySettings(t, ctx, pool)
	names := []string{"Outbox crew", "Outbox night crew"}
	clearSessionGroups(t, pool, names...)
	actor := model.Actor{Kind: "system", ID: "delivery-test"}
	var crew SessionGroup
	if err := inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		var err error
		crew, err = CreateSessionGroup(ctx, tx, names[0], []string{"outbox-s1"}, actor)
		return err
	})(); err != nil {
		t.Fatalf("create the group: %v", err)
	}

	gate := takeChangeLock(t, ctx, pool)
	update := goTimed(inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		_, err := UpdateSessionGroup(ctx, tx, crew.ID, SessionGroupUpdate{Add: []string{"outbox-s2"}, Remove: []string{"outbox-s1"}}, actor)
		return err
	}))
	// The group's update waits for the change log's lock.
	storetest.WaitForLockWaiters(t, gate, 1)
	var groupsFree bool
	if err := pool.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtext('delivery-session-groups'))`).Scan(&groupsFree); err != nil {
		t.Fatal(err)
	}
	if groupsFree {
		t.Error("the update waits for the change log's lock without the session groups' lock")
	}
	if rowLockedByAnother(t, ctx, pool, `select 1 from delivery_session_groups where id = $1::uuid`, crew.ID) {
		t.Error("the group's update waits for the change log's lock holding its group's row: it writes before it takes the lock")
	}
	if rowLockedByAnother(t, ctx, pool, `select 1 from delivery_session_group_members where session_id = 'outbox-s1'`) {
		t.Error("the group's update waits for the change log's lock holding the member it removes: it writes before it takes the lock")
	}
	started := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	run := goTimed(func() error { return UpsertRun(ctx, pool, changeTestRun(812, DeliveryRunKindDeploy, started, started)) })
	second := goTimed(inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		_, err := CreateSessionGroup(ctx, tx, names[1], []string{"outbox-s3"}, actor)
		return err
	}))
	// The run write and the second create both wait, behind the change log's lock or the update.
	storetest.WaitForLockWaiters(t, gate, 3)
	released := time.Now()
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("let the lock go: %v", err)
	}
	updateAnswer := testwait.Receive(t, "the group update to finish", update)
	runAnswer := testwait.Receive(t, "the run write to finish", run)
	secondAnswer := testwait.Receive(t, "the second group create to finish", second)
	requireNotDeadlocked(t, "the group's update", updateAnswer.err)
	requireNotDeadlocked(t, "the run write", runAnswer.err)
	requireNotDeadlocked(t, "the second group's create", secondAnswer.err)
	if t.Failed() {
		return
	}
	held := updateAnswer.at.Sub(released)
	groups, err := ListSessionGroups(ctx, pool)
	if err != nil {
		t.Fatalf("read the groups: %v", err)
	}
	stored := map[string][]string{}
	for _, group := range groups {
		stored[group.Name] = group.Sessions
	}
	if !slices.Equal(stored[names[0]], []string{"outbox-s2"}) || !slices.Equal(stored[names[1]], []string{"outbox-s3"}) {
		t.Errorf("the groups hold %v; want %s holding outbox-s2 and %s holding outbox-s3", stored, names[0], names[1])
	}
	t.Logf("the group's update waited for the change log's lock holding the session groups' lock and no group row; it held the lock at most %s, the run write and the second create behind it", held.Round(time.Microsecond))
}

// TestASessionGroupEditThatChangesNothingWritesNothing: an edit naming the group's own name, a
// session it holds and one it does not hold, by another actor than the one that wrote it, changes
// nothing the group holds, so it writes nothing. The decision is a read under the session groups'
// lock, so with the change log's lock held elsewhere the edit answers without waiting for it, the
// group as it stands, and leaves the stored group as it was (its updated_by and updated_at
// included) with no session_group change row: the trigger logs none for an update that moves
// updated_at alone (migration 0100), so such an update would leave a live view answering the old
// updated_at while the read answered the new.
func TestASessionGroupEditThatChangesNothingWritesNothing(t *testing.T) {
	shared, ctx := deliveryTestPool(t)
	pool := wideTestPool(t, shared)
	clearSessionGroups(t, pool, "Steady crew")
	author := model.Actor{Kind: "system", ID: "delivery-test"}
	var crew SessionGroup
	if err := inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		var err error
		crew, err = CreateSessionGroup(ctx, tx, "Steady crew", []string{"steady-s1", "steady-s2"}, author)
		return err
	})(); err != nil {
		t.Fatalf("create the group: %v", err)
	}
	head, err := ChangesHead(ctx, pool)
	if err != nil {
		t.Fatalf("read the change log's head: %v", err)
	}
	seq := head.Seq

	gate := takeChangeLock(t, ctx, pool)
	name := crew.Name
	var answered SessionGroup
	edit := goTimed(inGroupTransaction(ctx, pool, func(tx pgx.Tx) error {
		var err error
		answered, err = UpdateSessionGroup(ctx, tx, crew.ID, SessionGroupUpdate{
			Name: &name, Add: []string{"steady-s1"}, Remove: []string{"steady-absent"},
		}, model.Actor{Kind: "user", ID: "another-editor"})
		return err
	}))
	if answer := testwait.Receive(t, "the unchanged edit to answer while the change log's lock is held", edit).err; answer != nil {
		t.Fatalf("the edit that changes nothing: %v", answer)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("let the change log's lock go: %v", err)
	}

	stored, err := getSessionGroup(ctx, pool, crew.ID)
	if err != nil {
		t.Fatalf("read the group: %v", err)
	}
	for label, group := range map[string]SessionGroup{"the edit's answer": answered, "the stored group": stored} {
		if group.Name != crew.Name || !slices.Equal(group.Sessions, crew.Sessions) || group.UpdatedBy.ID != author.ID || !group.UpdatedAt.Equal(crew.UpdatedAt) {
			t.Errorf("%s = %+v, want the group as created, %+v", label, group, crew)
		}
	}
	changes, err := ChangesSince(ctx, pool, seq, 100)
	if err != nil {
		t.Fatalf("read the changes after the edit: %v", err)
	}
	for _, change := range changes {
		if change.Kind == ChangeKindSessionGroup {
			t.Errorf("the edit that changes nothing logged a session_group change at seq %d", change.Seq)
		}
	}
	t.Logf("the edit answered with the change log's lock held elsewhere and logged none of the %d changes after seq %d", len(changes), seq)
}

func TestAFactsAndAFilingsWriteTakeNoChangeLogLock(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('OUT', 'Outbox', 'alice')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, sql string }{
		{ChangeKindIssueFacts, `insert into issue_facts (project_key, issue_key) values ('OUT', 'OUT-1')`},
		{ChangeKindIssueFilings, `insert into issue_filings (project_key, issue_key, filed_at, filer_role, class)
			values ('OUT', 'OUT-1', now(), 'architect', 'out_of_tree')`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			var locked bool
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from pg_locks where pid = pg_backend_pid() and locktype = 'advisory'
				and classid = $1 and objid = $2 and objsubid = 2 and granted
			)`, changeLockClass, changeLockObject).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			if locked {
				t.Errorf("%s trigger took the change log's lock (6900)", tc.kind)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var pending, logged int
			if err := pool.QueryRow(ctx, `
				select (select count(*) from delivery_changes_pending where kind = $1 and repo = 'OUT' and ref = 'OUT-1'),
				       (select count(*) from delivery_changes where kind = $1 and repo = 'OUT' and ref = 'OUT-1')`,
				tc.kind).Scan(&pending, &logged); err != nil {
				t.Fatal(err)
			}
			if pending != 1 || logged != 0 {
				t.Errorf("%s wrote %d pending rows and %d log rows; want 1 and 0", tc.kind, pending, logged)
			}
		})
	}
}

// deliverySource parses the production files in this package so inventory tests see new files too.
func deliverySource(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sources := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sources[path] = file
	}
	return sources
}

func TestEveryChangeKindIsAdmittedByTheChangeLog(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	// No writer in this slice records these reserved kinds.
	reserved := []string{"eval_readings", "eval_versions", "eval_days"}
	all, pending := slices.Clone(reserved), slices.Clone(reserved)
	for _, file := range deliverySource(t) {
		for _, decl := range file.Decls {
			decl, ok := decl.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				continue
			}
			for _, spec := range decl.Specs {
				spec := spec.(*ast.ValueSpec)
				for i, name := range spec.Names {
					if !strings.HasPrefix(name.Name, "ChangeKind") {
						continue
					}
					if i >= len(spec.Values) {
						t.Fatalf("%s must declare its value", name)
					}
					literal, ok := spec.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("%s must declare a string literal", name)
					}
					kind, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					all = append(all, kind)
					if name.Name == "ChangeKindIssueFacts" || name.Name == "ChangeKindIssueFilings" {
						pending = append(pending, kind)
					}
				}
			}
		}
	}
	literal := regexp.MustCompile(`'([^']+)'`)
	for _, tc := range []struct {
		table string
		want  []string
	}{
		{"delivery_changes", all},
		{"delivery_changes_pending", pending},
	} {
		var definition string
		if err := pool.QueryRow(ctx, `select pg_get_constraintdef(oid) from pg_constraint
			where conrelid = $1::text::regclass and conname = $1 || '_kind_check'`, tc.table).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, match := range literal.FindAllStringSubmatch(definition, -1) {
			got = append(got, match[1])
		}
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s admits %v; want %v", tc.table, got, tc.want)
		}
	}
}

func TestEveryTakerOfTheChangeLogLockIsListed(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	const (
		direct = "TestADirectChangeWaitsForTheWriterBeforeItSoNoCursorSkipsIt"
		drain  = "TestTheDrainMovesABurstABatchPerTransaction"
		jobs   = "TestARunsJobsWriteTakesTheChangeLogLockLast"
		pr     = "TestTheDeployedWriteTakesTheChangeLogLockLastAndNeverDeadlocksWithAPullRequestWrite"
		groups = "TestASessionGroupWriteTakesTheChangeLogLockBeforeItsRows"
	)
	remaining := map[string]string{
		"function delivery_change_log":                                                  direct,
		"function delivery_changes_drain":                                               drain,
		"function delivery_pull_requests_changed":                                       pr,
		"function delivery_runs_changed":                                                jobs,
		"function delivery_run_jobs_changed":                                            jobs,
		"function delivery_settings_changed":                                            direct,
		"function delivery_session_groups_changed":                                      groups,
		"trigger delivery_pull_requests.delivery_pull_requests_changed":                 pr,
		"trigger delivery_runs.delivery_runs_changed":                                   jobs,
		"trigger delivery_run_jobs.delivery_run_jobs_changed":                           jobs,
		"trigger delivery_settings.delivery_settings_changed":                           direct,
		"trigger delivery_session_groups.delivery_session_groups_changed":               groups,
		"trigger delivery_session_group_members.delivery_session_group_members_changed": groups,
		"Go changes.go:queueLockChanges -> lockChangesStatement":                        groups,
		"Go groups.go:writeSessionGroup -> queueLockChanges":                            groups,
		"Go groups.go:DeleteSessionGroup -> queueLockChanges":                           groups,
	}
	seen := map[string]bool{}
	check := func(taker string) {
		if _, listed := remaining[taker]; !listed && !seen[taker] {
			t.Errorf("unlisted change log lock taker: %s", taker)
		}
		delete(remaining, taker)
		seen[taker] = true
	}
	// The catalog does not record call dependencies within PL/pgSQL bodies.
	rows, err := pool.Query(ctx, `
		with recursive funcs as (
			select p.oid, p.proname, pg_get_functiondef(p.oid) as definition
			from pg_proc p join pg_namespace n on n.oid = p.pronamespace
			where n.nspname = any(current_schemas(false)) and p.prokind in ('f', 'p')
		), takers as (
			select oid, proname from funcs
			where definition ~* 'pg_(try_)?advisory_(xact_)?lock(_shared)?[[:space:]]*\([[:space:]]*6900[[:space:]]*,[[:space:]]*0[[:space:]]*\)'
			union
			select f.oid, f.proname from funcs f join takers t
			on f.oid <> t.oid and f.definition ~* ('\m' || t.proname || '[[:space:]]*\(')
		)
		select 'function ' || proname from takers
		union all
		select 'trigger ' || c.relname || '.' || g.tgname from pg_trigger g
		join pg_class c on c.oid = g.tgrelid join takers t on t.oid = g.tgfoid
		where not g.tgisinternal`)
	if err != nil {
		t.Fatal(err)
	}
	takers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, taker := range takers {
		check(taker)
	}
	for path, file := range deliverySource(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				id, ok := node.(*ast.Ident)
				if ok && (id.Name == "queueLockChanges" || id.Name == "lockChangesStatement") {
					check(fmt.Sprintf("Go %s:%s -> %s", path, fn.Name.Name, id.Name))
				}
				return true
			})
		}
	}
	for taker, test := range remaining {
		t.Errorf("listed change log lock taker not found: %s (%s)", taker, test)
	}
}
