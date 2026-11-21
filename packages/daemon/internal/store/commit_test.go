package store

// Transact's commit policy, measured against the PostgreSQL LEGION_TEST_PG_DSN names through a
// proxy that holds or cuts a commit's reply (testdb.CommitFaults): a commit whose reply is lost
// runs its hooks if and only if the store says the transaction committed, and a store that cannot
// say leaves them to a watcher that runs them once it can.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The lines Transact and its watcher write, as the daemon's log carries them.
const (
	lostReplyCommitted = "store: a commit's reply was lost; the store says the transaction committed"
	lostReplyUnknown   = "store: a commit's reply was lost and the store cannot say whether it landed; its commit hooks wait until it can"
	unknownCommitted   = "store: a transaction whose outcome was unknown committed; its commit hooks run now"
	unknownAborted     = "store: a transaction whose outcome was unknown did not commit; its commit hooks are dropped"
	unknownPoolClosed  = "store: the pool closed before the store said whether a transaction committed; its commit hooks did not run"
	watchedHookPanic   = "store: a commit hook the watcher ran panicked"
)

// commitMatch is the SQL every commit test's body sends, which each fault is armed on.
const commitMatch = "insert into commit_probe"

// poolCloseBound is how long the rig's pool may take to close at the test's end. A pool closes
// once every connection it handed out is given back, so a transaction that kept its connection
// would hold the test's end for good; this bound names it instead.
const poolCloseBound = 10 * time.Second

// commitRig is one database holding commit_probe, the daemon's guarded pool reaching it through
// the fault proxy, a direct pool that observes what committed, and the count of hooks that ran.
type commitRig struct {
	pool     *Pool
	observer *pgxpool.Pool
	fault    *testdb.CommitFault
	hooks    atomic.Int32
}

func newCommitRig(t *testing.T) *commitRig {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.Database(t, "legion_store_commit_test")
	observer, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open the observer: %v", err)
	}
	t.Cleanup(observer.Close)
	if _, err := observer.Exec(ctx, "create table commit_probe (id int primary key)"); err != nil {
		t.Fatalf("create commit_probe: %v", err)
	}
	proxied, fault := testdb.CommitFaults(t, dsn)
	faulted, err := pgxpool.New(ctx, proxied)
	if err != nil {
		t.Fatalf("open the pool through the fault proxy: %v", err)
	}
	pool := NewPool(faulted)
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() {
			pool.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(poolCloseBound):
			t.Errorf("the pool did not close within %s: a connection it handed out was never given back", poolCloseBound)
		}
	})
	return &commitRig{pool: pool, observer: observer, fault: fault}
}

// write is the body of a commit test: one row, and a hook that counts.
func (r *commitRig) write(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "insert into commit_probe values (1)"); err != nil {
		return err
	}
	OnCommit(ctx, func() { r.hooks.Add(1) })
	return nil
}

// rows is how many rows of commit_probe the observer sees committed.
func (r *commitRig) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.observer.QueryRow(context.Background(), "select count(*) from commit_probe").Scan(&n); err != nil {
		t.Fatalf("count commit_probe: %v", err)
	}
	return n
}

// waitVisible waits until the observer sees the body's row committed.
func (r *commitRig) waitVisible(t *testing.T) {
	t.Helper()
	testwait.Eventually(t, "the body's row to be visible to the observer", func() bool { return r.rows(t) == 1 })
}

// transacted is what a Transact run on its own goroutine returned, and when.
type transacted struct {
	err error
	at  time.Time
}

// transact runs Transact on a goroutine of its own, so the test can meet its fault meanwhile.
func (r *commitRig) transact(ctx context.Context, write func(context.Context, pgx.Tx) error) <-chan transacted {
	done := make(chan transacted, 1)
	go func() {
		err := Transact(ctx, r.pool, write)
		done <- transacted{err: err, at: time.Now()}
	}()
	return done
}

// returned waits for the run transact started, failing the test rather than hanging it.
func returned(t *testing.T, done <-chan transacted) transacted {
	t.Helper()
	bound := testwait.Bounded(t, time.Minute, time.Second)
	select {
	case got := <-done:
		return got
	case <-time.After(bound):
		t.Fatalf("Transact did not return within %s", bound)
		return transacted{}
	}
}

// captureCommitLog sends the process's default logger into a locked buffer for the length of the
// test, which is where Transact and its watcher write: the watcher writes from a goroutine of its
// own while the test reads. No test of this package runs in parallel, so the swap is safe. A test
// that fails prints what the buffer holds.
func captureCommitLog(t *testing.T) *testlog.Buffer {
	t.Helper()
	var log testlog.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		if t.Failed() {
			t.Logf("the log:\n%s", log.String())
		}
	})
	return &log
}

// logged is how many lines of log carry message and every one of fields.
func logged(log *testlog.Buffer, message string, fields ...string) int {
	n := 0
	for line := range strings.SplitSeq(log.String(), "\n") {
		if !strings.Contains(line, message) {
			continue
		}
		all := true
		for _, field := range fields {
			all = all && strings.Contains(line, field)
		}
		if all {
			n++
		}
	}
	return n
}

// noCommitLine fails the test for any line of Transact's or its watcher's in log.
func noCommitLine(t *testing.T, log *testlog.Buffer) {
	t.Helper()
	for _, message := range []string{lostReplyCommitted, lostReplyUnknown, unknownCommitted, unknownAborted, unknownPoolClosed} {
		if n := logged(log, message); n != 0 {
			t.Errorf("the log holds %d lines %q, want none", n, message)
		}
	}
}

// wantHooks fails the test unless the hooks ran want times.
func (r *commitRig) wantHooks(t *testing.T, want int32) {
	t.Helper()
	if got := r.hooks.Load(); got != want {
		t.Fatalf("the hooks ran %d times, want %d", got, want)
	}
}

func TestACommitWhoseReplyComesAfterTheCancelRunsItsHooks(t *testing.T) {
	rig := newCommitRig(t)
	ctx, cancel := context.WithCancel(WithTransactionTracking(context.Background()))
	defer cancel()

	fired := rig.fault.HoldReply(commitMatch, 300*time.Millisecond)
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)
	rig.waitVisible(t)
	cancel()

	if got := returned(t, done); got.err != nil {
		t.Fatalf("Transact = %v, want nil: the commit landed and its reply came after the cancel", got.err)
	}
	rig.wantHooks(t, 1)
}

func TestACommitWhoseReplyNeverComesIsReadFromTheStore(t *testing.T) {
	rig := newCommitRig(t)
	ctx, cancel := context.WithCancel(WithTransactionTracking(context.Background()))
	defer cancel()

	fired := rig.fault.HoldReply(commitMatch, time.Hour)
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)
	rig.waitVisible(t)
	cancel()
	cancelled := time.Now()

	got := returned(t, done)
	if got.err != nil {
		t.Fatalf("Transact = %v, want nil: the store says the transaction committed", got.err)
	}
	rig.wantHooks(t, 1)
	if took, bound := got.at.Sub(cancelled), CommitSettleBound+500*time.Millisecond; took > bound {
		t.Fatalf("Transact returned %s after the cancel, want at most %s", took, bound)
	}
}

func TestACommitWithNoCancelWaitsForItsReply(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	ctx := WithTransactionTracking(context.Background())
	hold := CommitSettleBound + time.Second

	fired := rig.fault.HoldReply(commitMatch, hold)
	started := time.Now()
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)

	got := returned(t, done)
	if got.err != nil {
		t.Fatalf("Transact = %v, want nil: nothing cancelled the commit, whose reply came after %s", got.err, hold)
	}
	rig.wantHooks(t, 1)
	if took := got.at.Sub(started); took < hold {
		t.Fatalf("Transact returned %s after it started, before the reply held for %s: a commit nobody cancels waits for its reply", took, hold)
	}
	if n := logged(log, lostReplyCommitted); n != 0 {
		t.Fatalf("the log holds %d lines %q, want none: a commit nobody cancels reads no outcome", n, lostReplyCommitted)
	}
}

func TestACommitCutAfterItLandedIsReadFromTheStore(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	ctx := WithTransactionTracking(context.Background())

	fired := rig.fault.Cut(commitMatch, testdb.CutAfterCommit)
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)

	if got := returned(t, done); got.err != nil {
		t.Fatalf("Transact = %v, want nil: the store says the transaction committed", got.err)
	}
	rig.wantHooks(t, 1)
	if n := logged(log, lostReplyCommitted, "transaction="); n != 1 {
		t.Fatalf("the log holds %d lines %q, want 1", n, lostReplyCommitted)
	}
}

func TestACommitCutBeforeItLandedRunsNoHookAndItsRetryDoes(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())

	fired := rig.fault.Cut(commitMatch, testdb.CutBeforeCommit)
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)

	got := returned(t, done)
	if got.err == nil || errors.Is(got.err, ErrCommitOutcomeUnknown) {
		t.Fatalf("Transact = %v, want the error of a transaction the store says did not commit", got.err)
	}
	rig.wantHooks(t, 0)
	if n := rig.rows(t); n != 0 {
		t.Fatalf("commit_probe holds %d rows after a commit cut before it reached the server, want 0", n)
	}

	if err := Transact(ctx, rig.pool, rig.write); err != nil {
		t.Fatalf("the retry: Transact = %v, want nil", err)
	}
	rig.wantHooks(t, 1)
	if n := rig.rows(t); n != 1 {
		t.Fatalf("commit_probe holds %d rows after the retry, want 1", n)
	}
}

// The ERROR answer alone: a FATAL or PANIC answer is held at commitErrorKind.
func TestARollbackTheServerAnswersRunsNoHook(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())

	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "create temp table d (id int unique deferrable initially deferred) on commit drop; insert into d values (1),(1)"); err != nil {
			return err
		}
		OnCommit(ctx, func() { rig.hooks.Add(1) })
		return nil
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.Severity != "ERROR" {
		t.Fatalf("Transact = %v, want the server's ERROR 23505 at the commit", err)
	}
	rig.wantHooks(t, 0)
}

// unsentError is an error pgx would give for a commit that never reached the server.
type unsentError struct{}

func (unsentError) Error() string     { return "nothing reached the server" }
func (unsentError) SafeToRetry() bool { return true }

func TestACommitErrorIsSortedByWhetherTheServerAnswered(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want commitError
	}{
		{"a commit answered ROLLBACK", pgx.ErrTxCommitRollback, commitAnswered},
		{"an ERROR", &pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23505"}, commitAnswered},
		{"a wrapped ERROR", fmt.Errorf("commit: %w", &pgconn.PgError{Severity: "ERROR"}), commitAnswered},
		{"an ERROR under a German lc_messages", &pgconn.PgError{Severity: "FEHLER", SeverityUnlocalized: "ERROR"}, commitAnswered},
		{"a FATAL that ends the connection", &pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}, commitAmbiguous},
		{"a PANIC", &pgconn.PgError{Severity: "PANIC", SeverityUnlocalized: "PANIC"}, commitAmbiguous},
		{"an error safe to retry", unsentError{}, commitUnsent},
		{"a connection that ended", io.ErrUnexpectedEOF, commitAmbiguous},
		{"a reply the deadline abandoned", fmt.Errorf("timeout: %w", context.DeadlineExceeded), commitAmbiguous},
	} {
		if got := commitErrorKind(row.err); got != row.want {
			t.Errorf("commitErrorKind(%s: %v) = %d, want %d", row.name, row.err, got, row.want)
		}
	}
}

func TestAnUnreachableStoreLeavesTheOutcomeUnknownUntilItAnswers(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	// parent stays live for the whole test; child, which shares its hold mark, is the only
	// context Transact sees.
	parent := WithTransactionTracking(context.Background())
	child, cancel := context.WithCancel(parent)
	defer cancel()

	var xid string
	fired := rig.fault.HoldReply(commitMatch, time.Hour)
	done := rig.transact(child, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into commit_probe values (1)"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "select pg_current_xact_id()::text").Scan(&xid); err != nil {
			return err
		}
		OnCommit(ctx, func() { rig.hooks.Add(1) })
		return nil
	})
	testdb.Met(t, fired)
	rig.waitVisible(t)
	cancel()
	rig.fault.Refuse(true)

	got := returned(t, done)
	if !errors.Is(got.err, ErrCommitOutcomeUnknown) {
		t.Fatalf("Transact = %v, want %v", got.err, ErrCommitOutcomeUnknown)
	}
	if !strings.Contains(got.err.Error(), "transaction "+xid) {
		t.Fatalf("Transact = %v, want an error naming transaction %s", got.err, xid)
	}
	// Why the store could not answer: the error of the status read, which readOutcome writes.
	statusRead := "read the status of transaction " + xid + ":"
	if !strings.Contains(got.err.Error(), statusRead) {
		t.Fatalf("Transact = %v, want an error that carries the status read's error (%q)", got.err, statusRead)
	}
	rig.wantHooks(t, 0)
	if n := logged(log, lostReplyUnknown, "transaction="+xid+" ", `status_error="`+statusRead); n != 1 {
		t.Fatalf("when Transact returned, the log held %d lines %q naming transaction %s with the status read's error, want 1; the log:\n%s", n, lostReplyUnknown, xid, log.String())
	}

	// The control transaction: the caller's next transaction, open on the caller's mark before the
	// store answers any read of the watcher's, and for the whole wait. The proxy refuses every
	// connection until it is open, so it opens on the direct pool: the mark it sets is the
	// caller's context's, whichever pool it holds a connection of, and that mark is what the
	// watcher's reads must not wait on.
	held, err := NewPool(rig.observer).BeginTx(parent, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the control transaction on the live parent: %v", err)
	}
	defer func() { _ = held.Rollback(parent) }()
	if _, err := held.Exec(parent, "select 1"); err != nil {
		t.Fatalf("the control transaction: %v", err)
	}
	if !holdsConnection(parent) {
		t.Fatal("the control transaction does not hold the caller's mark")
	}
	rig.fault.Refuse(false)

	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the watcher runs the hooks while the caller's mark holds a connection", func() bool {
		return rig.hooks.Load() == 1
	})

	if !holdsConnection(parent) {
		t.Fatal("the control transaction stopped holding the caller's mark during the wait")
	}
	if _, err := held.Exec(parent, "select 1"); err != nil {
		t.Fatalf("the control transaction after the wait: %v", err)
	}
	if err := held.Commit(parent); err != nil {
		t.Fatalf("commit the control transaction: %v", err)
	}

	time.Sleep(2 * outcomeRetryFirst)
	rig.wantHooks(t, 1)
	if n := logged(log, lostReplyUnknown); n != 1 {
		t.Fatalf("the log holds %d lines %q, want 1", n, lostReplyUnknown)
	}
	if n := logged(log, unknownCommitted, "transaction="+xid+" "); n != 1 {
		t.Fatalf("the log holds %d lines %q naming transaction %s, want 1", n, unknownCommitted, xid)
	}
}

// A hook that panics on the watcher's goroutine is recovered and logged with its transaction,
// and the transaction's later hooks still run. Nothing above the watcher recovers, as net/http
// does for a route's goroutine, so a panic there would end the daemon.
func TestAHookThatPanicsOnTheWatchersGoroutineIsLoggedAndTheRestRun(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	parent := WithTransactionTracking(context.Background())
	child, cancel := context.WithCancel(parent)
	defer cancel()

	var xid string
	fired := rig.fault.HoldReply(commitMatch, time.Hour)
	done := rig.transact(child, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into commit_probe values (1)"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "select pg_current_xact_id()::text").Scan(&xid); err != nil {
			return err
		}
		OnCommit(ctx, func() { panic("a hook that panics") })
		OnCommit(ctx, func() { rig.hooks.Add(1) })
		return nil
	})
	testdb.Met(t, fired)
	rig.waitVisible(t)
	cancel()
	rig.fault.Refuse(true)
	if got := returned(t, done); !errors.Is(got.err, ErrCommitOutcomeUnknown) {
		t.Fatalf("Transact = %v, want %v", got.err, ErrCommitOutcomeUnknown)
	}
	rig.fault.Refuse(false)

	testwait.WithinPolling(t, 10*time.Second, testwait.Poll, "the watcher runs the hook after the one that panicked", func() bool {
		return rig.hooks.Load() == 1
	})
	if n := logged(log, watchedHookPanic, "transaction="+xid+" ", `panic="a hook that panics"`); n != 1 {
		t.Fatalf("the log holds %d lines %q naming transaction %s and the panic, want 1", n, watchedHookPanic, xid)
	}
}

// A run of transactUntilCommitted whose outcome is unknown stops waiting for it once the pool
// closes, as the watcher does, rather than reading a closed pool until its context ends: its
// context here ends only after a minute, and it answers well before that.
func TestARetryWhoseOutcomeIsUnknownEndsWhenThePoolCloses(t *testing.T) {
	rig := newCommitRig(t)
	ctx, cancel := context.WithTimeout(WithTransactionTracking(context.Background()), time.Minute)
	defer cancel()
	fired := rig.fault.Cut(commitMatch, testdb.CutAfterCommitRefusing)
	done := make(chan error, 1)
	go func() { done <- transactUntilCommitted(ctx, rig.pool, rig.write) }()
	testdb.Met(t, fired)
	rig.pool.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCommitOutcomeUnknown) {
			t.Fatalf("transactUntilCommitted = %v, want %v", err, ErrCommitOutcomeUnknown)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("transactUntilCommitted still waits 20s after its pool closed, for a context that ends at a minute")
	}
}

// The body's own cancel: the id read already refuses the ended context.
func TestAContextEndedBeforeTheCommitCommitsNothing(t *testing.T) {
	rig := newCommitRig(t)
	ctx, cancel := context.WithCancel(WithTransactionTracking(context.Background()))
	defer cancel()

	err := Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.write(ctx, tx); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Transact = %v, want %v", err, context.Canceled)
	}
	rig.wantHooks(t, 0)
	if n := rig.rows(t); n != 0 {
		t.Fatalf("commit_probe holds %d rows, want 0: nothing commits once the caller's context has ended", n)
	}
}

// A cancel that lands between the id read and the commit, which only the guard before the commit
// refuses.
func TestACancelAfterTheIDReadCommitsNothing(t *testing.T) {
	rig := newCommitRig(t)
	ctx, cancel := context.WithCancel(WithTransactionTracking(context.Background()))
	defer cancel()
	pool := testdb.CancelAfterScan(rig.pool, "pg_current_xact_id_if_assigned", cancel)

	err := Transact(ctx, pool, rig.write)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Transact = %v, want %v", err, context.Canceled)
	}
	if n := pool.Commits(); n != 0 {
		t.Fatalf("Transact sent %d commits after the caller's context ended, want 0", n)
	}
	if n := rig.rows(t); n != 0 {
		t.Fatalf("commit_probe holds %d rows, want 0", n)
	}
	rig.wantHooks(t, 0)
}

func TestATransactionThatWroteNothingNeedsNoRead(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	ctx := WithTransactionTracking(context.Background())

	// The store refuses every connection once the reply is lost, so a read of the outcome fails.
	fired := rig.fault.Cut("select 1", testdb.CutAfterCommitRefusing)
	done := rig.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			return err
		}
		OnCommit(ctx, func() { rig.hooks.Add(1) })
		return nil
	})
	testdb.Met(t, fired)

	if got := returned(t, done); got.err != nil {
		t.Fatalf("Transact = %v, want nil: a transaction that wrote nothing holds the same rows either way", got.err)
	}
	rig.wantHooks(t, 1)
	noCommitLine(t, log)
}

func TestAnOutcomeStillInProgressIsUnknownUntilItEnds(t *testing.T) {
	rig := newCommitRig(t)
	ctx := context.Background()

	open, err := rig.observer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the direct transaction: %v", err)
	}
	defer func() { _ = open.Rollback(ctx) }()
	if _, err := open.Exec(ctx, "insert into commit_probe values (1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var xid string
	if err := open.QueryRow(ctx, "select pg_current_xact_id()::text").Scan(&xid); err != nil {
		t.Fatalf("read the direct transaction's id: %v", err)
	}

	reading, stop := context.WithTimeout(ForConcurrentWorker(ctx), 200*time.Millisecond)
	got, err := readOutcome(reading, rig.pool, xid)
	stop()
	if got != outcomeUnknown || err == nil {
		t.Fatalf("readOutcome of a transaction in progress = %d, %v; want outcomeUnknown and an error", got, err)
	}

	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit the direct transaction: %v", err)
	}
	reading, stop = context.WithTimeout(ForConcurrentWorker(ctx), outcomeReadBound)
	defer stop()
	if got, err := readOutcome(reading, rig.pool, xid); got != outcomeCommitted || err != nil {
		t.Fatalf("readOutcome of a committed transaction = %d, %v; want outcomeCommitted", got, err)
	}
}

// A transaction still in progress when its reply is lost, which then aborts: the outcome is
// unknown at first, and the watcher reads the abort and drops the hooks. Every status is
// PostgreSQL's own; only the lost reply's error is given.
func TestTheWatcherDropsTheHooksOfATransactionThatAborts(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	ctx := context.Background()
	open, err := rig.observer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the direct transaction: %v", err)
	}
	defer func() { _ = open.Rollback(ctx) }()
	if _, err := open.Exec(ctx, "insert into commit_probe values (1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var xid string
	if err := open.QueryRow(ctx, "select pg_current_xact_id()::text").Scan(&xid); err != nil {
		t.Fatalf("read the direct transaction's id: %v", err)
	}
	err = settleLostReply(ctx, rig.pool, xid, []func(){func() { rig.hooks.Add(1) }}, io.ErrUnexpectedEOF)
	if !errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("the reply lost while the transaction is in progress = %v, want %v", err, ErrCommitOutcomeUnknown)
	}
	rig.wantHooks(t, 0)
	if n := logged(log, lostReplyUnknown, "transaction="+xid+" "); n != 1 {
		t.Fatalf("the log holds %d lines %q naming transaction %s, want 1", n, lostReplyUnknown, xid)
	}
	if err := open.Rollback(ctx); err != nil {
		t.Fatalf("roll the direct transaction back: %v", err)
	}
	testwait.WithinPolling(t, 5*time.Second, testwait.Poll, "the watcher to read the abort and drop the hooks", func() bool {
		return logged(log, unknownAborted, "transaction="+xid+" ") == 1
	})
	rig.wantHooks(t, 0)
	if n := rig.rows(t); n != 0 {
		t.Fatalf("the rolled-back transaction left %d rows, want 0", n)
	}
}

func TestTheOutcomeWatchEndsWhenThePoolCloses(t *testing.T) {
	rig := newCommitRig(t)
	log := captureCommitLog(t)
	ctx, cancel := context.WithCancel(WithTransactionTracking(context.Background()))
	defer cancel()

	fired := rig.fault.HoldReply(commitMatch, time.Hour)
	done := rig.transact(ctx, rig.write)
	testdb.Met(t, fired)
	rig.waitVisible(t)
	cancel()
	rig.fault.Refuse(true)

	got := returned(t, done)
	if !errors.Is(got.err, ErrCommitOutcomeUnknown) {
		t.Fatalf("Transact = %v, want %v", got.err, ErrCommitOutcomeUnknown)
	}
	if n := logged(log, lostReplyUnknown); n != 1 {
		t.Fatalf("when Transact returned, the log held %d lines %q, want 1; the log:\n%s", n, lostReplyUnknown, log.String())
	}

	rig.pool.Close()
	testwait.Eventually(t, "the watcher's line that the pool closed", func() bool {
		return logged(log, unknownPoolClosed) == 1
	})
	rig.wantHooks(t, 0)
}

// CommitScoped is true only while a TransactWith body runs: on the body's context kept past its
// return it is false, and OnCommit panics there, since that transaction's hooks were taken when
// the body returned and a hook appended later would never run.
func TestCommitScopedIsTrueOnlyWithinTransact(t *testing.T) {
	if CommitScoped(context.Background()) {
		t.Fatal("CommitScoped(context.Background()) = true, want false")
	}
	rig := newCommitRig(t)
	var scoped bool
	var kept context.Context
	if err := Transact(WithTransactionTracking(context.Background()), rig.pool, func(ctx context.Context, _ pgx.Tx) error {
		scoped, kept = CommitScoped(ctx), ctx
		return nil
	}); err != nil {
		t.Fatalf("Transact: %v", err)
	}
	if !scoped {
		t.Fatal("CommitScoped inside Transact's body = false, want true")
	}
	if CommitScoped(kept) {
		t.Fatal("CommitScoped on the body's context after Transact returned = true, want false")
	}
	if recovered := onCommitPanic(kept); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "store.Transact") {
		t.Fatalf("OnCommit on the body's context after Transact returned panicked with %v, want a panic naming store.Transact", recovered)
	}
}

// onCommitPanic is what OnCommit on ctx panics with, or nil.
func onCommitPanic(ctx context.Context) (recovered any) {
	defer func() { recovered = recover() }()
	OnCommit(ctx, func() {})
	return nil
}

func TestOnCommitPanicsOutsideATransaction(t *testing.T) {
	if recovered := onCommitPanic(context.Background()); recovered == nil || !strings.Contains(fmt.Sprint(recovered), "store.Transact") {
		t.Fatalf("OnCommit outside a transaction panicked with %v, want a panic naming store.Transact", recovered)
	}
}

func TestAPanicInTheBodyGivesItsConnectionBack(t *testing.T) {
	rig := newCommitRig(t)
	ctx := WithTransactionTracking(context.Background())

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = Transact(ctx, rig.pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := rig.write(ctx, tx); err != nil {
				return err
			}
			panic("the body's panic")
		})
	}()
	if recovered != "the body's panic" {
		t.Fatalf("recovered %v, want the body's panic", recovered)
	}
	if n := rig.rows(t); n != 0 {
		t.Fatalf("commit_probe holds %d rows after the body panicked, want 0", n)
	}
	if holdsConnection(ctx) {
		t.Fatal("the caller still holds a connection after its body panicked")
	}
	if err := Transact(ctx, rig.pool, rig.write); err != nil {
		t.Fatalf("the next Transact on the same context = %v, want nil", err)
	}
	rig.wantHooks(t, 1)
}
