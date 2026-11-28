package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/lockwaittest"
)

// shortLockTimeout is the Timeout a test gives the one store it runs (underLocking) where no
// verdict reads the lock watch: Postgres's own lock_timeout, a timer of the server that no clock of
// the module reaches, which INFRA-3149 lets a test shorten where no verdict depends on how short it
// is (dispatch://INFRA-3149/comment/769de508-777d-4a13-99c6-e857f3c9c9f5, item 3). A lock_timeout
// counts only the time a statement waits for a lock another session holds, and each such test runs
// on a database of its own, where the one lock its runner or census waits for is the lock the test
// holds until it has read the outcome. So a slow server only makes the timeout fire later: it
// neither lets the held wait through nor ends a wait the test did not set up, since none comes.
// Autovacuum, the one session the test does not start, takes no lock on these tables: a database
// copied from a template has no statistics, from which autovacuum reads what a table owes, and a
// table a test creates holds a few rows, short of autovacuum's thresholds. A row whose verdict
// reads the lock or the holder a *pgmigrate.LockTimeoutError names keeps production's Timeout
// (zero) instead (watchReadsTheWait says why).
const shortLockTimeout = 100 * time.Millisecond

// underLocking gives store the lock discipline a test runs it under (Store.locking): timeout as its
// Timeout, zero for production's, and a clocktest.Fake as its clock, which the runner's pause
// between two attempts, its lock watch and the bounds of a holder's diagnosis run on. It returns the
// fake, which moves only where the test moves it.
func underLocking(store *Store, timeout time.Duration) *clocktest.Fake {
	fake := clocktest.New(time.Unix(0, 0).UTC())
	store.clock = fake
	store.lockTimeout = timeout
	return fake
}

// awaitThePause waits until the runner, whose clock is fake, has armed its pause after attempt:
// pgmigrate.LockTimeoutRetryPause from the fake's moment. The runner arms it only once the attempt
// has returned a lock timeout (SQLSTATE 55P03) and rolled back, holding nothing, so the arming is the
// fact that the attempt gave up at the server's lock_timeout. A runner that never gives up, as one
// whose lock waits no lock_timeout bounds, fails this wait at the test binary's deadline less
// testwait's margin, naming the attempt.
func awaitThePause(t *testing.T, fake *clocktest.Fake, attempt int) {
	t.Helper()
	fake.AwaitArmedAt(t, fmt.Sprintf("the runner's pause after its attempt %d timed out", attempt), fake.Now().Add(pgmigrate.LockTimeoutRetryPause))
}

// movePastThePause waits for the runner's pause after attempt (awaitThePause) and moves fake past
// it, so the runner makes its next attempt.
func movePastThePause(t *testing.T, fake *clocktest.Fake, attempt int) {
	t.Helper()
	awaitThePause(t, fake, attempt)
	fake.Advance(pgmigrate.LockTimeoutRetryPause)
}

// movePastEveryPause moves fake past the runner's pause after each attempt but the last
// (movePastThePause), so the runner makes every attempt pgmigrate.LockTimeoutAttempts allows.
func movePastEveryPause(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	for attempt := 1; attempt < pgmigrate.LockTimeoutAttempts; attempt++ {
		movePastThePause(t, fake, attempt)
	}
}

// watchReadsTheWait has the runner's lock watch, which runs on the store's fake clock, take a
// reading while the runner waits for mode on relation. The watch reads only at a tick the test
// moves the fake to, and arms its first tick as the attempt starts (pgmigrate.Locking), so this
// waits through testwait until pg_locks shows the statement queued (waitForQueuedLock), then moves
// the fake to that tick. The reading must land before Postgres ends the wait at the Locking's
// Timeout, so a row that calls this runs under production's Timeout (zero): a row whose verdict
// needs a step to finish inside a shortened server bound keeps production's value, which no slowed
// step outlasts (dispatch://INFRA-3149/comment/44861a24-bbec-4ee1-8530-54bf97c7427e). The steps
// inside it are the test's read of pg_locks, this move and the watch's one dial and query.
func watchReadsTheWait(t *testing.T, store *Store, fake *clocktest.Fake, relation, mode string) {
	t.Helper()
	waitForQueuedLock(t, store, relation, mode)
	armed := fake.AwaitArmed(t, "the lock watch's first tick", 1)
	fake.Set(armed[0])
}

// everyAttemptTimesOut drives the runner, under production's Timeout on fake, through each of its
// attempts at a migration that waits for mode on relation until Postgres's lock_timeout ends the
// wait: in each, the lock watch reads the wait (watchReadsTheWait), and after each but the last the
// test moves the clock past the runner's pause (movePastThePause). The runner's error after the
// last attempt is the caller's to read.
func everyAttemptTimesOut(t *testing.T, store *Store, fake *clocktest.Fake, relation, mode string) {
	t.Helper()
	for attempt := 1; attempt <= pgmigrate.LockTimeoutAttempts; attempt++ {
		watchReadsTheWait(t, store, fake, relation, mode)
		if attempt < pgmigrate.LockTimeoutAttempts {
			movePastThePause(t, fake, attempt)
		}
	}
}

// lockTimeoutPastTheGiveUp is a Timeout under which no lock wait ends before every shared wait of t
// gives up (testwait.GiveUpAt): the span from now to that moment, in the whole milliseconds a
// pgmigrate.Locking takes, and the largest lock_timeout Postgres takes, 2147483647 ms, under
// -timeout 0, where no wait gives up. A wait the test holds open under it can end only where the
// test ends it, or after the test has failed by name.
func lockTimeoutPastTheGiveUp(t *testing.T) time.Duration {
	t.Helper()
	const largest = math.MaxInt32 * time.Millisecond
	giveUp, bounded := testwait.GiveUpAt(t)
	if !bounded {
		return largest
	}
	return min(max(time.Until(giveUp).Truncate(time.Millisecond)+time.Millisecond, time.Millisecond), largest)
}

// A Store Open returns runs production's lock discipline, the zero pgmigrate.Locking's: its
// Timeout is zero, which pgmigrate reads as LockTimeout, and its clock is clock.Real(), the clock of
// the runner's pauses and lock watch. Both are read from the store (locking) and from Postgres: a
// migration the store applies records the lock_timeout it ran under, 5s, and a census of the same
// database, the store's census under the zero Locking, counts a row wherever its own lock_timeout is
// not the one the migration recorded, so the runner and the census wait under one bound. The
// attempts and the pause between them are pgmigrate's, the same for every Locking: three attempts,
// two seconds apart. The row fails if Open gave the store another Timeout or another clock.
func TestAStoreOpenReturnsRunsProductionsLockDiscipline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openEmptyTestStore(t)
	locking := store.locking()
	if locking.Timeout != 0 {
		t.Errorf("the Timeout of a Store Open returned = %v, want zero, which pgmigrate reads as LockTimeout (%v)", locking.Timeout, pgmigrate.LockTimeout)
	}
	if locking.Clock != clock.Real() {
		t.Errorf("the lock discipline of a Store Open returned runs on %T, want clock.Real()", locking.Clock)
	}
	if pgmigrate.LockTimeout != 5*time.Second || pgmigrate.LockTimeoutAttempts != 3 || pgmigrate.LockTimeoutRetryPause != 2*time.Second {
		t.Errorf("the runner's discipline is LockTimeout %v, %d attempts and pauses of %v, want production's 5s, 3 and 2s", pgmigrate.LockTimeout, pgmigrate.LockTimeoutAttempts, pgmigrate.LockTimeoutRetryPause)
	}
	set := fstest.MapFS{
		"migrations/0001_seen.up.sql":     {Data: []byte("create table seen as select current_setting('lock_timeout') as lock_timeout")},
		"migrations/0002_note.up.sql":     {Data: []byte("alter table seen add column note text")},
		"migrations/0002_note.census.sql": {Data: []byte("select count(*) from seen where lock_timeout <> current_setting('lock_timeout')")},
	}
	if err := store.migrateThrough(ctx, set, 1); err != nil {
		t.Fatalf("migrate through 0001: %v", err)
	}
	var seen string
	if err := store.Pool.QueryRow(ctx, "select lock_timeout from seen").Scan(&seen); err != nil {
		t.Fatalf("read the lock_timeout 0001 ran under: %v", err)
	}
	if seen != "5s" {
		t.Errorf("a migration of a Store Open returned ran under lock_timeout %q, want 5s", seen)
	}
	report, err := census(ctx, store.Pool.Config().ConnString(), set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Pending) != 1 || report.Pending[0].Count == nil || *report.Pending[0].Count != 0 {
		t.Errorf("the census's count of rows whose lock_timeout is not its own = %+v, want one pending migration counting 0: the census waits under the lock_timeout the runner waited under", report.Pending)
	}
}

// #5951's defect, at the store's runner: a context that ends a migration after its lock wait ended,
// while the migration runs a statement that waits on no lock, cut no lock wait, so Migrate returns
// the context's error and no *pgmigrate.LockTimeoutError, and its error names no holder. One would
// name the lock the migration already holds and a holder that has left, and tell whoever reads the
// boot's log to end that holder. The lock watch keeps the wait it read after the migration got past
// it, so the wait it answers says nothing of whether the migration still waited when the context
// ended. (internal/pgmigrate's TestACancelAfterTheLockWaitEndedIsNoLockTimeout holds Locking.Exec
// to the same; this row holds the runner that applies it.)
//
// The store runs under production's Timeout on a fake clock, so the lock watch reads only where the
// test moves the fake to its tick: once while the runner waits on the holder's lock, which the
// watch records, and once after the runner holds it and sleeps, which reads no wait and keeps that
// record. The first reading must land inside the 5 s the runner waits before Postgres cancels it,
// the value no slowed step outlasts (watchReadsTheWait). The watch arms each tick only once the
// reading before it is done (clock.Ticker), so the test knows each reading has returned when it
// sees the next tick armed.
func TestACancelAfterTheRunnersLockWaitEndedIsNoLockTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openEmptyTestStore(t)
	fake := underLocking(store, 0)
	start := fake.Now()
	if _, err := store.Pool.Exec(ctx, "create table things (kind text)"); err != nil {
		t.Fatal(err)
	}
	holderConn := connectForTest(t, ctx, store)
	holder, err := holderConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, "lock table things in access share mode"); err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{
		// The sleep outlasts every step of the test; the test ends it once it has read the error.
		"migrations/0001_added.up.sql": {Data: []byte("alter table things add column added int; select pg_sleep(3600)")},
	}
	cut, cancel := context.WithCancel(ctx)
	defer cancel()
	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(cut, set) }()

	waitForQueuedLock(t, store, "things", "AccessExclusiveLock")
	var runner uint32
	if err := store.Pool.QueryRow(ctx, "select pid from pg_stat_activity where datname = current_database() and "+lockwaittest.Waiting).Scan(&runner); err != nil {
		t.Fatalf("read the runner's backend, which waits on the holder's lock: %v", err)
	}
	t.Cleanup(func() {
		// The sleep does not notice its client has gone; end it, and the lock and key it holds.
		_, _ = store.Pool.Exec(context.Background(), "select pg_terminate_backend($1)", runner)
	})
	first := fake.AwaitArmed(t, "the lock watch's first tick", 1)[0]
	interval := first.Sub(start)
	fake.Set(first)
	fake.AwaitArmedAt(t, "the lock watch's tick after its reading of the wait", first.Add(interval))
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("end the holder: %v", err)
	}
	testwait.Until(t, "the runner to hold the lock and sleep", func() (bool, bool) {
		var sleeping bool
		if err := store.Pool.QueryRow(ctx, `select coalesce(bool_or(wait_event = 'PgSleep'), false) from pg_stat_activity where pid = $1`, runner).Scan(&sleeping); err != nil {
			t.Fatalf("read whether the runner sleeps: %v", err)
		}
		return sleeping, sleeping
	})
	fake.Set(first.Add(interval))
	fake.AwaitArmedAt(t, "the lock watch's tick after its reading of no wait", first.Add(2*interval))
	cancel()
	err = testwait.Receive(t, "migrate to return once its context is cancelled", migrated)
	var named *pgmigrate.LockTimeoutError
	if errors.As(err, &named) {
		t.Errorf("migrate = %v\nwant no *pgmigrate.LockTimeoutError: the migration held its lock and waited on none when the context ended, yet the error names the wait on %+v", err, named.Wait)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("migrate = %v, want errors.Is(context.Canceled) to hold", err)
	}
	if holderPID := "pid " + strconv.FormatUint(uint64(holderConn.PgConn().PID()), 10); err != nil && strings.Contains(err.Error(), holderPID) {
		t.Errorf("migrate = %v, which names the holder (%s) that had left before the context ended", err, holderPID)
	}
}
