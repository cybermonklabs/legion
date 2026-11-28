package pgmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testpg"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lockTimeoutSetting is the zero Locking's lock_timeout as Postgres takes it, production's 5000ms,
// which the census's tests pass as a migration's bound and expect where a migration sets none.
var lockTimeoutSetting = Locking{}.timeoutSetting()

// The zero Locking is production's discipline: Postgres's lock_timeout at LockTimeout, five
// seconds, which Pin, PinSession and Census set as 5000ms, the runner's timers on the wall clock,
// and the lock watch reading every 200 ms, a 25th of that timeout, a ratio a Locking of its own
// keeps (20 ms at 500 ms). The attempts and the pause between them are the same for every Locking:
// three attempts, two seconds apart.
func TestAZeroLockingIsProductionsDiscipline(t *testing.T) {
	t.Parallel()
	var zero Locking
	if got := zero.timeout(); got != LockTimeout || LockTimeout != 5*time.Second {
		t.Errorf("the zero Locking's timeout is %v and LockTimeout is %v, want both 5s", got, LockTimeout)
	}
	if got := zero.timeoutSetting(); got != "5000ms" {
		t.Errorf("the zero Locking's lock_timeout setting is %q, want 5000ms", got)
	}
	for name, settings := range map[string][]setting{"Pin's and PinSession's": zero.pinSettings(), "Census's": zero.censusSettings()} {
		if !slices.Contains(settings, setting{"lock_timeout", "5000ms"}) {
			t.Errorf("the zero Locking's %s settings %v hold no lock_timeout of 5000ms", name, settings)
		}
	}
	if got := zero.clock(); got != clock.Real() {
		t.Errorf("the zero Locking's clock is %#v, want clock.Real()", got)
	}
	if got := zero.watchInterval(); got != 200*time.Millisecond {
		t.Errorf("the zero Locking's watch reads every %v, want 200ms", got)
	}
	fake := clocktest.New(time.Unix(0, 0).UTC())
	own := Locking{Timeout: 500 * time.Millisecond, Clock: fake}
	if got := own.timeoutSetting(); got != "500ms" {
		t.Errorf("a Locking of 500ms has the lock_timeout setting %q, want 500ms", got)
	}
	if got := own.watchInterval(); got != 20*time.Millisecond {
		t.Errorf("a Locking of 500ms has its watch read every %v, want 20ms", got)
	}
	if got := own.clock(); got != clock.Clock(fake) {
		t.Errorf("a Locking's clock is %#v, want the Clock it was given", got)
	}
	if LockTimeoutAttempts != 3 || LockTimeoutRetryPause != 2*time.Second {
		t.Errorf("LockTimeoutAttempts is %d and LockTimeoutRetryPause %v, want 3 and 2s", LockTimeoutAttempts, LockTimeoutRetryPause)
	}
}

// A Locking of any Timeout bounds every lock wait of a migration it applies, or refuses to apply
// it. Postgres takes lock_timeout in whole milliseconds and reads 0 as no bound at all, so a
// Timeout under a millisecond, written as 0ms, would leave the migration queued behind a holder
// for as long as the holder lives, which is what LockTimeout is for; and a Timeout under 25 ns
// makes the lock watch's interval, a 25th of it, zero, which clock.NewTicker refuses with a panic
// on the watch's goroutine, ending the process. For each such Timeout, Pin either refuses it or
// pins a lock_timeout that is a bound, and Exec of a migration that waits on a table another
// session holds either returns Pin's refusal or gives up with a *LockTimeoutError. The Locking's
// fake clock is never moved, so the lock watch never reads and never dials.
//
// Beside those rows are one of 1.5 ms, which lock_timeout would take as 1ms while the errors said
// 1.5ms, and one of 1 ms, which Pin takes. pg_settings shows the lock_timeout that Pin and
// PinSession pin as a whole number of milliseconds. Under a Locking that Pin refuses, PinSession,
// ExecNonTransactional and Census return the same refusal, none of them a *LockTimeoutError:
// PinSession leaves the session's lock_timeout as it was, ExecNonTransactional builds no index,
// and Census returns no report. Under a Locking that Pin takes, PinSession pins the same positive
// number of milliseconds, and that bound is the one the errors name: Exec's *LockTimeoutError holds
// it as its Timeout, the census refuses a table that another session holds in ACCESS EXCLUSIVE as
// not read within it, and ExecNonTransactional either builds the index or gives up with a
// *LockTimeoutError of that Timeout.
//
// One more row is a whole number of milliseconds above the largest lock_timeout Postgres takes,
// 2147483647 ms (INT_MAX): a Locking of 2147483648 ms is one Postgres cannot take either, so the
// seam refuses it as it refuses the others, before PinSession resets anything of the session: the
// row moves the session's search_path off the value RESET gives it, and a refused PinSession
// leaves it there.
func TestALockingOfAnyTimeoutBoundsTheMigrationsLockWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := testpg.URL(t)
	conn := connect(t, ctx, url)
	if _, err := conn.Exec(ctx, "create table held (id integer)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "create table schema_migrations (version bigint primary key); insert into schema_migrations values (1); create table guarded (id integer); create table things (kind text)"); err != nil {
		t.Fatal(err)
	}
	holder, err := connect(t, ctx, url).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, "lock table held in access share mode"); err != nil {
		t.Fatal(err)
	}
	guard, err := connect(t, ctx, url).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Rollback(context.Background()) })
	if _, err := guard.Exec(ctx, "lock table guarded in access exclusive mode"); err != nil {
		t.Fatal(err)
	}
	// lockTimeoutMillis reads the lock_timeout in force as pg_settings shows it, in milliseconds.
	const lockTimeoutMillis = "select setting::int from pg_settings where name = 'lock_timeout'"
	isLockTimeout := func(err error) bool {
		var timedOut *LockTimeoutError
		return errors.As(err, &timedOut)
	}
	for _, timeout := range []time.Duration{500 * time.Microsecond, 10 * time.Nanosecond, -time.Nanosecond, 1500 * time.Microsecond, time.Millisecond, 2147483648 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			locking := Locking{Timeout: timeout, Clock: clocktest.New(time.Unix(0, 0).UTC())}
			var pinned string
			var pinnedMillis int
			pinErr := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if err := locking.Pin(ctx, tx); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, lockTimeoutMillis).Scan(&pinnedMillis); err != nil {
					return err
				}
				return tx.QueryRow(ctx, "select current_setting('lock_timeout')").Scan(&pinned)
			})
			if pinErr == nil && pinned == "0" {
				t.Fatalf("a Locking of %v pins lock_timeout %q, which Postgres reads as no bound at all: want a bound, or Pin's refusal", timeout, pinned)
			}
			// pinnedTimeout is the bound Pin gave Postgres, which the errors of a Locking that Pin
			// takes must name.
			pinnedTimeout := time.Duration(pinnedMillis) * time.Millisecond
			session := connect(t, ctx, url)
			// The session's search_path is moved off the value RESET gives it, so a PinSession that
			// resets it before it refuses the Timeout shows in the session.
			if _, err := session.Exec(ctx, "set search_path = pg_catalog"); err != nil {
				t.Fatal(err)
			}
			var before, after int
			var pathBefore, pathAfter string
			if err := session.QueryRow(ctx, lockTimeoutMillis).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := session.QueryRow(ctx, "select current_setting('search_path')").Scan(&pathBefore); err != nil {
				t.Fatal(err)
			}
			sessionErr := locking.PinSession(ctx, session)
			if err := session.QueryRow(ctx, lockTimeoutMillis).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if err := session.QueryRow(ctx, "select current_setting('search_path')").Scan(&pathAfter); err != nil {
				t.Fatal(err)
			}
			if pinErr != nil {
				if !errors.Is(pinErr, errUnpinnableTimeout) {
					t.Errorf("Pin under a Locking of %v returned %v, want its refusal of a Timeout Postgres cannot take", timeout, pinErr)
				}
				if !errors.Is(sessionErr, errUnpinnableTimeout) || after != before || pathAfter != pathBefore {
					t.Errorf("PinSession under a Locking of %v, which Pin refused, returned %v and moved the session's lock_timeout from %d ms to %d ms and its search_path from %q to %q, want that refusal and the session as it was", timeout, sessionErr, before, after, pathBefore, pathAfter)
				}
			} else {
				if pinnedMillis <= 0 {
					t.Errorf("a Locking of %v pins lock_timeout %d ms, want a positive number of milliseconds", timeout, pinnedMillis)
				}
				if sessionErr != nil || after != pinnedMillis {
					t.Errorf("PinSession under a Locking of %v returned %v and pinned %d ms, want %d ms, as Pin pinned", timeout, sessionErr, after, pinnedMillis)
				}
			}
			done := make(chan error, 1)
			go func() {
				done <- pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
					return locking.Exec(ctx, tx, Migration{Name: "0001_note.up.sql", SQL: "alter table held add column note text"}, nil)
				})
			}()
			err := testwait.Receive(t, "Exec under a Locking of "+timeout.String()+" to return", done)
			var timedOut *LockTimeoutError
			switch {
			case pinErr != nil && (err == nil || errors.As(err, &timedOut)):
				t.Errorf("Pin refused a Locking of %v (%v), yet Exec under it returned %v, want that refusal", timeout, pinErr, err)
			case pinErr == nil && !errors.As(err, &timedOut):
				t.Errorf("Exec under a Locking of %v, which pins lock_timeout %q, returned %v, want a *LockTimeoutError: the wait on the held table is bounded", timeout, pinned, err)
			}
			if pinErr != nil && !errors.Is(err, errUnpinnableTimeout) {
				t.Errorf("Exec under a Locking of %v, which Pin refused, returned %v, want Pin's refusal of a Timeout Postgres cannot take", timeout, err)
			}
			if pinErr == nil && timedOut != nil && timedOut.Timeout != pinnedTimeout {
				t.Errorf("Exec under a Locking of %v, which pins lock_timeout %d ms, gave up with a *LockTimeoutError whose Timeout is %v, want %v, the bound Postgres ran under", timeout, pinnedMillis, timedOut.Timeout, pinnedTimeout)
			}
			report, censusErr := locking.Census(ctx, conn, []Migration{{Version: 2, Name: "0002_note.up.sql", SQL: "alter table guarded add column note text"}}, "schema_migrations", CensusOptions{})
			if pinErr != nil {
				if !errors.Is(censusErr, errUnpinnableTimeout) || isLockTimeout(censusErr) || report != nil {
					t.Errorf("Census under a Locking of %v, which Pin refused, returned %v and a report %v, want Pin's refusal and no report", timeout, censusErr, report != nil)
				}
			} else {
				want := "could not read guarded within " + pinnedTimeout.String()
				if censusErr != nil {
					t.Errorf("Census under a Locking of %v, which pins lock_timeout %d ms: %v", timeout, pinnedMillis, censusErr)
				} else if len(report.Pending) != 1 || !slices.ContainsFunc(report.Pending[0].Refusals, func(refusal string) bool { return strings.Contains(refusal, want) }) {
					t.Errorf("Census under a Locking of %v, which pins lock_timeout %d ms, read %+v, want one pending migration with a refusal containing %q", timeout, pinnedMillis, report.Pending, want)
				}
			}
			buildErr := locking.ExecNonTransactional(ctx, conn, indexMigration(), nil)
			if pinErr != nil {
				if !errors.Is(buildErr, errUnpinnableTimeout) || isLockTimeout(buildErr) {
					t.Errorf("ExecNonTransactional under a Locking of %v, which Pin refused, returned %v, want Pin's refusal", timeout, buildErr)
				}
				if got := indexState(t, ctx, conn, "things_kind"); got != (relation{}) {
					t.Errorf("ExecNonTransactional under a Locking of %v, which Pin refused, left things_kind %+v, want no relation of that name", timeout, got)
				}
				return
			}
			var buildTimedOut *LockTimeoutError
			switch {
			case buildErr == nil:
				if got := indexState(t, ctx, conn, "things_kind"); got != (relation{OID: got.OID, Index: true, Valid: true, Table: "things"}) {
					t.Errorf("ExecNonTransactional under a Locking of %v returned nil and left things_kind %+v, want a valid index on things", timeout, got)
				}
			case !errors.As(buildErr, &buildTimedOut) || buildTimedOut.Timeout != pinnedTimeout:
				t.Errorf("ExecNonTransactional under a Locking of %v, which pins lock_timeout %d ms, returned %v, want the index built or a *LockTimeoutError whose Timeout is %v", timeout, pinnedMillis, buildErr, pinnedTimeout)
			}
			// The next row starts with no index of that name, built or left INVALID.
			if _, err := conn.Exec(ctx, "drop index if exists things_kind"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// RetryLockTimeouts and RetryVersionTableLockTimeouts pause LockTimeoutRetryPause on their
// Locking's Clock between attempts: after each timed-out attempt but the last, the fake holds a
// pause armed two seconds past its moment, and the next attempt runs only once the test moves the
// fake past it. An attempt that always times out runs LockTimeoutAttempts times, and the error that
// comes back says so and still holds the attempt's timeout. The version table's retry also takes
// Postgres's own SQLSTATE 55P03, which a runner's step on that table meets outside Exec.
func TestALockingsRetriesPauseOnItsClock(t *testing.T) {
	t.Parallel()
	lockTimeout := &LockTimeoutError{Migration: "0001_held.up.sql", Timeout: LockTimeout, Err: &pgconn.PgError{Code: lockNotAvailable}}
	isLockTimeout := func(err error) bool {
		var timedOut *LockTimeoutError
		return errors.As(err, &timedOut) && timedOut == lockTimeout
	}
	pgTimeout := &pgconn.PgError{Code: lockNotAvailable}
	isPgTimeout := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr == pgTimeout
	}
	for _, row := range []struct {
		name  string
		retry func(Locking, context.Context, string, func(context.Context) error) error
		// timedOut is what every attempt fails with, and found says whether an error holds it.
		timedOut error
		found    func(error) bool
	}{
		{"RetryLockTimeouts of a *LockTimeoutError", Locking.RetryLockTimeouts, lockTimeout, isLockTimeout},
		{"RetryVersionTableLockTimeouts of a *LockTimeoutError", Locking.RetryVersionTableLockTimeouts, lockTimeout, isLockTimeout},
		{"RetryVersionTableLockTimeouts of SQLSTATE 55P03", Locking.RetryVersionTableLockTimeouts, pgTimeout, isPgTimeout},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fake := clocktest.New(time.Unix(0, 0).UTC())
			var attempts atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- row.retry(Locking{Clock: fake}, context.Background(), "held", func(context.Context) error {
					attempts.Add(1)
					return row.timedOut
				})
			}()
			for n := int32(1); n < LockTimeoutAttempts; n++ {
				fake.AwaitArmedAt(t, fmt.Sprintf("the pause after attempt %d", n), fake.Now().Add(LockTimeoutRetryPause))
				if got := attempts.Load(); got != n {
					t.Fatalf("%d attempts ran before pause %d, want %d", got, n, n)
				}
				fake.Advance(LockTimeoutRetryPause)
			}
			err := testwait.Receive(t, "the retry to give up after its last attempt", done)
			if got := attempts.Load(); got != LockTimeoutAttempts {
				t.Errorf("%d attempts ran, want %d", got, LockTimeoutAttempts)
			}
			if want := fmt.Sprintf("after %d attempts: ", LockTimeoutAttempts); err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Errorf("the retry returned %v, want an error starting %q", err, want)
			}
			if !row.found(err) {
				t.Errorf("the retry returned %v, in which errors.As does not find the attempts' timeout %v", err, row.timedOut)
			}
		})
	}
}

// A Locking's Timeout is the lock_timeout Postgres runs the migration under, in both apply modes,
// and the zero Locking's is production's five seconds. Exec's migration writes the lock_timeout it
// runs under into a table, and ExecNonTransactional's build runs under an event trigger that
// refuses it where its session's lock_timeout is not the row's. The Locking of 300 ms runs on a
// fake clock that the row never moves, so its lock watch never dials the row's database: a
// concurrent build waits for each backend there that holds an older snapshot, and a watch's would
// be one. Each row and each control has a database of its own, so no other row's session holds a
// snapshot where a build runs. Postgres lets only a superuser create an event trigger, so the build
// rows need the test database's login to be one, as CI's is.
func TestALockingPinsItsTimeoutOnTheMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, row := range []struct {
		name    string
		locking Locking
		// want is current_setting('lock_timeout') under the row's Locking, as Postgres shows it.
		want string
	}{
		{"a Locking of 300 ms", Locking{Timeout: 300 * time.Millisecond, Clock: clocktest.New(time.Unix(0, 0).UTC())}, "300ms"},
		{"the zero Locking", Locking{}, "5s"},
	} {
		t.Run("Exec under "+row.name, func(t *testing.T) {
			t.Parallel()
			conn := connect(t, ctx, testpg.URL(t))
			if _, err := conn.Exec(ctx, "create table seen (lock_timeout text)"); err != nil {
				t.Fatal(err)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := row.locking.Exec(ctx, tx, Migration{Version: 1, Name: "0001_seen.up.sql", SQL: "insert into seen select current_setting('lock_timeout')"}, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			var seen string
			if err := conn.QueryRow(ctx, "select lock_timeout from seen").Scan(&seen); err != nil {
				t.Fatalf("read the lock_timeout the migration ran under: %v", err)
			}
			if seen != row.want {
				t.Errorf("the migration ran under lock_timeout %q, want %q", seen, row.want)
			}
		})
		t.Run("ExecNonTransactional under "+row.name, func(t *testing.T) {
			t.Parallel()
			conn := connect(t, ctx, testpg.URL(t))
			if _, err := conn.Exec(ctx, `create table things (kind text);
				create function check_build_lock_timeout() returns event_trigger language plpgsql as $fn$
				begin
					if current_setting('lock_timeout') <> '`+row.want+`' then
						raise exception 'build ran under lock_timeout %', current_setting('lock_timeout');
					end if;
				end $fn$;
				create event trigger check_build_lock_timeout on ddl_command_start when tag in ('CREATE INDEX') execute function check_build_lock_timeout()`); err != nil {
				t.Fatal(err)
			}
			if err := row.locking.ExecNonTransactional(ctx, conn, indexMigration(), nil); err != nil {
				t.Fatalf("ExecNonTransactional: %v", err)
			}
			got := indexState(t, ctx, conn, "things_kind")
			if want := (relation{OID: got.OID, Index: true, Valid: true, Table: "things"}); got != want {
				t.Errorf("things_kind = %+v, want %+v", got, want)
			}
		})
	}
}

// A Locking's census reads under the Locking's Timeout, as its runner waits under it: a table that
// another session holds in ACCESS EXCLUSIVE, which the census's reads of the table's size and rows
// wait behind, is refused once that wait outlasts the Locking's 300 ms, and the refusal and the
// table's Unread name 300 ms, not production's 5 s. The census runs no lock watch, and the held
// lock is the one lock any of its statements waits for.
func TestALockingsCensusReadsItsTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := testpg.URL(t)
	conn := connect(t, ctx, url)
	if _, err := conn.Exec(ctx, "create table schema_migrations (version bigint primary key); insert into schema_migrations values (1); create table held (id integer)"); err != nil {
		t.Fatal(err)
	}
	holder := connect(t, ctx, url)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, "lock table held in access exclusive mode"); err != nil {
		t.Fatal(err)
	}
	short := Locking{Timeout: 300 * time.Millisecond}
	report, err := short.Census(ctx, conn, []Migration{{Version: 2, Name: "0002_note.up.sql", SQL: "alter table held add column note text"}}, "schema_migrations", CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Pending) != 1 {
		t.Fatalf("the census read %d pending migrations, want 1: %+v", len(report.Pending), report.Pending)
	}
	entry := report.Pending[0]
	if want := "could not read held within 300ms (SQLSTATE 55P03)"; len(entry.Refusals) != 1 || !strings.HasPrefix(entry.Refusals[0], want) {
		t.Errorf("refusals = %q, want one starting %q", entry.Refusals, want)
	}
	i := slices.IndexFunc(entry.Tables, func(table TableCensus) bool { return table.Name == "held" })
	if want := "its lock was not granted within 300ms"; i < 0 || entry.Tables[i].Unread != want {
		t.Errorf("tables = %+v, want held with Unread %q", entry.Tables, want)
	}
}

// A Locking's census runs under the Locking's Timeout, the lock_timeout its runner sets, and does
// not only name it: the migration's own census, which runs inside the census's transaction, counts
// a row wherever current_setting('lock_timeout') is not the Locking's. A census whose transaction
// kept production's 5 s under a Locking of 300 ms counts 1 here, and still passes
// TestALockingsCensusReadsItsTimeout, which writes its refusal's words from the Locking and waits
// the held table out at whatever lock_timeout Postgres was given.
//
// A Locking of 1 ms runs its census under 1ms, the shortest lock_timeout Postgres takes as a
// bound. A Locking of 500 µs, 10 ns or -1 ns, which lock_timeout would take as 0, no bound at all,
// or of 1.5 ms, which it would take as 1ms, is one Postgres cannot take: Census returns its
// refusal and no report, so no census runs under another bound than the one its refusals name.
func TestALockingsCensusRunsUnderItsTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := connect(t, ctx, testpg.URL(t))
	if _, err := conn.Exec(ctx, "create table schema_migrations (version bigint primary key); insert into schema_migrations values (1)"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		locking Locking
		want    string
	}{
		{Locking{Timeout: 300 * time.Millisecond}, "300ms"}, {Locking{}, "5s"}, {Locking{Timeout: time.Millisecond}, "1ms"},
		// A Timeout that Postgres cannot take has no lock_timeout to run under: want is empty.
		{Locking{Timeout: 500 * time.Microsecond}, ""}, {Locking{Timeout: 10 * time.Nanosecond}, ""},
		{Locking{Timeout: -time.Nanosecond}, ""}, {Locking{Timeout: 1500 * time.Microsecond}, ""},
	} {
		census := fmt.Sprintf("select count(*) from (select 1) as one where current_setting('lock_timeout') <> '%s'", row.want)
		report, err := row.locking.Census(ctx, conn, []Migration{{Version: 2, Name: "0002_note.up.sql", SQL: "select 1", Census: census, CensusName: "0002_note.census.sql"}}, "schema_migrations", CensusOptions{})
		if row.want == "" {
			if !errors.Is(err, errUnpinnableTimeout) || report != nil {
				t.Errorf("the census under a Locking of %v returned %v and a report %v, want its refusal of a Timeout Postgres cannot take and no report", row.locking.Timeout, err, report != nil)
			}
			continue
		}
		if err != nil {
			t.Fatalf("census under lock_timeout %s: %v", row.want, err)
		}
		if len(report.Pending) != 1 {
			t.Fatalf("the census under lock_timeout %s read %d pending migrations, want 1", row.want, len(report.Pending))
		}
		if entry := report.Pending[0]; entry.Count == nil || *entry.Count != 0 || len(entry.Refusals) != 0 {
			t.Errorf("a census that counts a row where lock_timeout is not %s refused %q: the census's transaction did not run under the Locking's Timeout", row.want, entry.Refusals)
		}
	}
}

// The zero Locking's timeout error names production's five seconds, the bound every migration of
// a production runner waits under unless it sets its own. Here the migration sets its own, 100 ms,
// with SET LOCAL as a migration does, and then waits on a table another session holds, so Postgres
// cancels it at 100 ms; the *LockTimeoutError that the package function Exec returns still names
// the runner's bound, LockTimeout, in its Timeout and in its text.
func TestTheZeroLockingsTimeoutErrorNamesFiveSeconds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := testpg.URL(t)
	runner := connect(t, ctx, url)
	if _, err := runner.Exec(ctx, "create table held (id integer)"); err != nil {
		t.Fatal(err)
	}
	holder := connect(t, ctx, url)
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Rollback(context.Background()) })
	if _, err := held.Exec(ctx, "lock table held in access share mode"); err != nil {
		t.Fatal(err)
	}
	tx, err := runner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	err = Exec(ctx, tx, Migration{Version: 1, Name: "0001_note.up.sql", SQL: "set local lock_timeout = '100ms'; alter table held add column note text"}, nil)
	var timedOut *LockTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("Exec = %v, want a *LockTimeoutError", err)
	}
	if timedOut.Timeout != LockTimeout {
		t.Errorf("the error's Timeout is %v, want LockTimeout, %v", timedOut.Timeout, LockTimeout)
	}
	if want := "(5s unless the migration sets its own)"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name production's timeout as %q", err, want)
	}
}

// What the watch reports after a sequence of readings. One empty reading of a wait that named its
// holders is the cancellation race between pg_locks and pg_blocking_pids and is held back; two in a
// row mean nobody holding the lock can be named - the holder left, or only a prepared transaction,
// which pg_blocking_pids reports as pid 0, still blocks - and a departed holder must not be named.
// Each row is the sequence that tells one term of the rule from its absence.
func TestObserveNamesOnlyAHolderTheLatestReadingsSupport(t *testing.T) {
	t.Parallel()
	named := func(pid uint32) *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages", Holders: []Session{{PID: pid}}}
	}
	empty := func() *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages"}
	}
	otherLock := &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "asks"}
	otherMode := &LockWait{LockType: "relation", Mode: "ShareLock", Object: "messages"}
	for _, tc := range []struct {
		name     string
		readings []*LockWait
		want     *LockWait
	}{
		{"one empty reading as the cancellation lands keeps the holder", []*LockWait{named(97), empty()}, named(97)},
		{"two empty readings in a row name nobody", []*LockWait{named(97), empty(), empty()}, empty()},
		{"a holder named after a held-back reading replaces it, and the next empty reading is held back again", []*LockWait{named(97), empty(), named(98), empty()}, named(98)},
		{"a holder that takes over replaces the one before it", []*LockWait{named(97), named(98)}, named(98)},
		{"an empty reading of another lock replaces at once", []*LockWait{named(97), otherLock}, otherLock},
		{"an empty reading of another mode on the same table replaces at once", []*LockWait{named(97), otherMode}, otherMode},
		{"a reading of not waiting ends the run, so the next empty one is held back", []*LockWait{named(97), empty(), nil, empty()}, named(97)},
		{"a first reading that names nobody is recorded, so the refusal names the lock", []*LockWait{empty()}, empty()},
		{"a reading of not waiting after a named one keeps it, and the next empty one is held back", []*LockWait{named(97), nil, empty()}, named(97)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w watch
			for _, reading := range tc.readings {
				w.observe(reading)
			}
			if !reflect.DeepEqual(w.wait, tc.want) {
				t.Errorf("after %d readings the watch reports %+v, want %+v", len(tc.readings), w.wait, tc.want)
			}
		})
	}
}

// stallingForwarder passes bytes between one client and the Postgres server dsn names, on a
// loopback port of its own, until stall stops it: from then on it passes nothing in either
// direction and holds both connections open, so a statement written to it neither completes nor
// fails. That is the transport failure a diagnosis's cleanup must not wait on without a bound.
// Only the cleanup this test registers releases it, by closing every connection, and that runs
// after the test's own checks: a call that came back at all came back while the stall still held.
// Every goroutine the forwarder starts has ended when the test returns.
type stallingForwarder struct {
	// addr is the address to dial in the server's place.
	addr *net.TCPAddr

	mu        sync.Mutex
	stalled   bool
	swallowed bool
	conns     []net.Conn
	closing   bool
	wg        sync.WaitGroup
}

// stall stops the forwarder passing bytes, in both directions, for the rest of the test.
func (f *stallingForwarder) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stalled = true
}

// holding reports whether the stall has taken bytes the client wrote and passed them to nobody:
// that something the client sent is waiting on this transport and waits on it still.
func (f *stallingForwarder) holding() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.swallowed
}

// track holds every connection the forwarder opens: a pump blocked reading one ends only when
// that connection closes, which the test's cleanup does for all of them.
func (f *stallingForwarder) track(conn net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		conn.Close()
		return false
	}
	f.conns = append(f.conns, conn)
	return true
}

// pump passes what it reads from src on to dst until either end stops or the stall begins. The
// chunk the stall meets reaches nobody, and the forwarder records that it swallowed it.
func (f *stallingForwarder) pump(dst, src net.Conn) {
	defer f.wg.Done()
	buf := make([]byte, 16*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			f.mu.Lock()
			halted := f.stalled
			if halted {
				f.swallowed = true
			}
			f.mu.Unlock()
			if halted {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// startForwarder puts a stallingForwarder in front of the server dsn names and returns it,
// listening and passing everything on until something calls stall.
func startForwarder(t *testing.T, dsn string) *stallingForwarder {
	t.Helper()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	network, address := pgconn.NetworkAddress(config.Host, config.Port)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &stallingForwarder{addr: listener.Addr().(*net.TCPAddr)}
	t.Cleanup(func() {
		listener.Close()
		f.mu.Lock()
		f.closing = true
		for _, conn := range f.conns {
			conn.Close()
		}
		f.mu.Unlock()
		testwait.Returns(t, "the forwarder's goroutines to end once its listener and connections are closed", f.wg.Wait)
	})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			if !f.track(client) {
				return
			}
			server, err := net.Dial(network, address)
			if err != nil {
				t.Errorf("dial the test database: %v", err)
				client.Close()
				return
			}
			if !f.track(server) {
				return
			}
			f.wg.Add(2)
			go f.pump(server, client)
			go f.pump(client, server)
		}
	}()
	return f
}

// stallAtTheCleanup stalls the forwarder at the first statement run on a context other than the
// one the diagnosis's reads ran on. Every statement of the diagnosis - its transaction, each
// holder's savepoint, the diagnosis query and that savepoint's rollback - runs on the one context
// diagnoseHolders reads under, and the cleanup's rollback is the first that does not, because
// diagnoseHolders gives the rollback and the close a context of their own
// (diagnosisCleanupTimeout). So the stall begins once the diagnosis has been read and before any
// byte of the cleanup leaves pgx, which traces a statement before it writes it. Nothing here
// reads the wire: what the cleanup sends, and whether pgx sends anything at all, does not decide
// when the forwarder stops.
type stallAtTheCleanup struct {
	forwarder *stallingForwarder

	mu    sync.Mutex
	armed bool
	seen  bool
	reads <-chan struct{}
}

// arm starts the watch, once the connection is up: every statement from here is the diagnosis's
// or its cleanup's, and none is signing in.
func (s *stallAtTheCleanup) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *stallAtTheCleanup) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.armed:
	case !s.seen:
		s.seen, s.reads = true, ctx.Done()
	case ctx.Done() != s.reads:
		s.forwarder.stall()
	}
	return ctx
}

func (s *stallAtTheCleanup) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A diagnosis whose cleanup cannot finish returns anyway, with what it read. diagnoseHolders rolls
// its read-only transaction back and closes its connection on a context of their own, bounded by
// diagnosisCleanupTimeout, so a transport that takes the rollback and answers it never - neither
// completing nor failing - holds the runner up a second at most, not for as long as that
// transport lives, and the holder the diagnosis read before the stall is still named. The stall
// outlives the answer: the forwarder is released only by its own cleanup, after the checks below,
// and those checks fail a call that came back with nothing of its cleanup still held.
//
// diagnoseHolders runs on a fake clock, which arms both of its bounds and fires neither until the
// test moves it. Once the forwarder holds the cleanup, one of the armed deadlines must lie within
// three seconds of the fake's moment, and the test then moves the fake those three seconds: the
// cleanup's own bound is what ends the call, and no load on the machine can make it miss.
func TestDiagnoseHoldersReturnsWhenTheCleanupCannotFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := testpg.URL(t)
	// The session the diagnosis reads. It stays connected, so pg_stat_activity has a row for it.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	var pid uint32
	if err := holder.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read the holder's pid: %v", err)
	}

	forwarder := startForwarder(t, dsn)
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	config.Host, config.Port = forwarder.addr.IP.String(), uint16(forwarder.addr.Port)
	// Everything the diagnosis sends has to cross the forwarder: a fallback dials the address the
	// URL named and would reach the server behind its back, and TLS over a loopback hop buys
	// nothing.
	config.TLSConfig, config.Fallbacks = nil, nil
	tracer := &stallAtTheCleanup{forwarder: forwarder}
	config.Tracer = tracer
	dial := func(ctx context.Context) (*pgx.Conn, error) {
		conn, err := pgx.ConnectConfig(ctx, config)
		if err == nil {
			tracer.arm()
		}
		return conn, err
	}

	// The bound the cleanup must keep: diagnosisCleanupTimeout's one second and two more, written
	// out rather than read from diagnosisCleanupTimeout so that raising that bound reddens this
	// row instead of moving it: a cleanup left to LockTimeout's five seconds has to fail here.
	const bound = 3 * time.Second
	fake := clocktest.New(time.Unix(0, 0).UTC())
	done := make(chan []string, 1)
	go func() { done <- diagnoseHolders(ctx, fake, dial, []Session{{PID: pid}}) }()
	testwait.Until(t, "the forwarder to hold the bytes of the diagnosis's cleanup", func() (bool, bool) {
		held := forwarder.holding()
		if !held {
			select {
			case diagnoses := <-done:
				t.Fatalf("diagnoseHolders returned %q and the forwarder had swallowed nothing: the cleanup was never held open, so this row says nothing about its bound", diagnoses)
			default:
			}
		}
		return held, held
	})
	// The cleanup arms its bound before it sends the rollback the forwarder now holds.
	now, armed := fake.Now(), fake.Armed()
	if !slices.ContainsFunc(armed, func(deadline time.Time) bool { return !deadline.After(now.Add(bound)) }) {
		t.Fatalf("with the cleanup held, the fake clock holds the deadlines %v, none within %v of its moment %v: the cleanup waits on a transport that passes nothing with no bound of %v", armed, bound, now, bound)
	}
	fake.Advance(bound)
	diagnoses := testwait.Receive(t, "diagnoseHolders to return once the fake clock has passed its cleanup's bound", done)
	// The forwarder is released by its own cleanup, which runs after this check, so a forwarder
	// still holding the bytes of the rollback is one this call never waited for.
	if !forwarder.holding() {
		t.Fatalf("diagnoseHolders returned and the forwarder had swallowed nothing: the cleanup was never held open, so this row says nothing about its bound")
	}
	want := fmt.Sprintf("diagnosis of pid %d, read when refused: ", pid)
	if len(diagnoses) != 1 || !strings.HasPrefix(diagnoses[0], want) {
		t.Fatalf("diagnoseHolders returned %q, want one diagnosis starting %q", diagnoses, want)
	}
}

// Exec reads a migration as the census read it, whatever the runner's connection carries, because
// it sets the text settings (textSettings) transaction-local before the migration runs. Each row
// gives the runner's connection one setting at a value the census does not read under: a session
// SET, which an earlier migration can leave on a pooled connection, a connection option, or a
// database default. The row's migration is one insert of one string constant to the census's
// parser, and under that setting Postgres reads it as two inserts, or refuses it. Each row applies
// the migration through Exec in a transaction and commits, then reads the table on a connection of
// its own in UTF-8: it must hold one row, the constant the census's parser read. A connection left
// in SJIS cannot read Á back, so the runner's connection does not read the table. The runner's
// connection must still carry the row's setting after the commit, since Exec's settings end with
// its transaction.
func TestExecReadsAMigrationAsTheCensusReadsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The census's parser reads each text below as one insert of the one constant after it. Under
	// standard_conforming_strings off, \' escapes the quote after it: 'a\'' is a', and the rest of
	// the text is a second insert and a comment.
	const backslashBeforeQuote = `insert into r values ('a\'');insert into r values ($$x$$);--')`
	const backslashBeforeQuoteValue = `a\');insert into r values ($$x$$);--`
	// Under client_encoding SJIS, the second byte of Á (C3 81 in UTF-8) and the backslash after it
	// are one character, so the backslash escapes nothing and the quote after it ends the literal.
	const backslashAfterNonASCII = `insert into r values (E'Á\');insert into r values ($$x$$);--')`
	const backslashAfterNonASCIIValue = `Á');insert into r values ($$x$$);--`
	// Under backslash_quote off, Postgres refuses the \' (SQLSTATE 22P06).
	const escapedQuote = `insert into r values (E'a\'')`
	const escapedQuoteValue = `a'`
	// sessionSet connects to url and runs set on the connection, for the session.
	sessionSet := func(set string) func(*testing.T, string) *pgx.Conn {
		return func(t *testing.T, url string) *pgx.Conn {
			conn := connect(t, ctx, url)
			if _, err := conn.Exec(ctx, set); err != nil {
				t.Fatalf("%s: %v", set, err)
			}
			return conn
		}
	}
	for _, row := range []struct {
		name string
		// runner connects to the row's database, which url names, and gives the connection the
		// row's setting.
		runner func(t *testing.T, url string) *pgx.Conn
		// setting is the setting the row changes, and value its value on the runner's connection.
		setting, value string
		// want is the one constant the census's parser reads in sql.
		sql, want string
	}{
		{"a session set of standard_conforming_strings off", sessionSet("set standard_conforming_strings = off"), "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a connection option of standard_conforming_strings off", func(t *testing.T, url string) *pgx.Conn {
			config, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			config.RuntimeParams["options"] = "-c standard_conforming_strings=off"
			conn, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatalf("connect with standard_conforming_strings off: %v", err)
			}
			t.Cleanup(func() { conn.Close(context.Background()) })
			return conn
		}, "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a database default of standard_conforming_strings off", func(t *testing.T, url string) *pgx.Conn {
			config, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			if _, err := connect(t, ctx, url).Exec(ctx, "alter database "+pgx.Identifier{config.Database}.Sanitize()+" set standard_conforming_strings = off"); err != nil {
				t.Fatalf("set the database's default: %v", err)
			}
			return connect(t, ctx, url)
		}, "standard_conforming_strings", "off", backslashBeforeQuote, backslashBeforeQuoteValue},
		{"a session set of client_encoding SJIS", sessionSet("set client_encoding = 'SJIS'"), "client_encoding", "SJIS", backslashAfterNonASCII, backslashAfterNonASCIIValue},
		{"a session set of backslash_quote off", sessionSet("set backslash_quote = off"), "backslash_quote", "off", escapedQuote, escapedQuoteValue},
	} {
		t.Run(row.name, func(t *testing.T) {
			statements, ok := parseStatements(row.sql)
			if !ok || len(statements) != 1 {
				t.Fatalf("the census's parser read %d statements (ok %v), want one", len(statements), ok)
			}
			var tree any
			if err := json.Unmarshal(statements[0].body, &tree); err != nil {
				t.Fatalf("read the parse tree: %v", err)
			}
			var census []string
			stringConstants(tree, &census)
			if !reflect.DeepEqual(census, []string{row.want}) {
				t.Fatalf("the census's parser read the string constants %q, want %q", census, row.want)
			}

			url := testpg.URL(t)
			readerConfig, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatalf("parse the test database's URL: %v", err)
			}
			readerConfig.RuntimeParams["client_encoding"] = "UTF8"
			reader, err := pgx.ConnectConfig(ctx, readerConfig)
			if err != nil {
				t.Fatalf("connect the reader: %v", err)
			}
			t.Cleanup(func() { reader.Close(context.Background()) })
			if _, err := reader.Exec(ctx, "create table r (v text)"); err != nil {
				t.Fatal(err)
			}

			runner := row.runner(t, url)
			carries := func(when string) {
				t.Helper()
				var value string
				if err := runner.QueryRow(ctx, "select current_setting($1)", row.setting).Scan(&value); err != nil || value != row.value {
					t.Fatalf("%s, the runner's connection has %s = %q, %v; want %q", when, row.setting, value, err, row.value)
				}
			}
			carries("before Exec")
			tx, err := runner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := Exec(ctx, tx, Migration{Version: 1, Name: "0001_r.up.sql", SQL: row.sql}, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			carries("after the commit")

			rows, err := reader.Query(ctx, "select v from r")
			if err != nil {
				t.Fatal(err)
			}
			stored, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, census) {
				t.Fatalf("r holds %q, want %q, the one constant the census's parser read", stored, census)
			}
		})
	}
}

// Exec resolves a migration's names under the values that RESET gives session_authorization,
// role and search_path (resetSettings), as the census resolves them, whatever the runner's
// connection carries. An earlier migration can leave each of them changed on a pooled connection,
// by a session SET or through stored code that calls set_config, and a name can then resolve to
// another object than the census read: search_path lists the schemas a lookup searches, "$user"
// in it names the schema of the session user, and the current user's privileges decide which
// schemas a lookup can use. Each row changes one or all three for the session of the runner's
// connection: a SET of search_path, a SET ROLE, and a superuser's SET SESSION AUTHORIZATION to
// another role. The last row signs in as a login that is no superuser and makes the first two,
// as a deployment's login can. The row's migration writes the search_path, current_user and
// session_user it runs under into public.seen, and seen must hold the values that the connection
// read when it was new, before the row's SETs: the values that RESET gives. The connection must
// still carry the row's settings after the commit, since Exec's settings end with its
// transaction. Postgres lets only a session whose login is a superuser set session
// authorization, so the server is asked whether the test database's login is one (rolsuper), and
// the two rows that set it skip when it is not, except on GitHub Actions, where they fail
// (testdb.SkipOutsideCI).
func TestExecResolvesNamesUnderTheValuesResetGives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	url := testpg.URL(t)
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	admin := connect(t, ctx, url)
	var superuser bool
	if err := admin.QueryRow(ctx, "select rolsuper from pg_roles where rolname = session_user").Scan(&superuser); err != nil {
		t.Fatalf("ask the server whether the test database's login is a superuser: %v", err)
	}
	// A role spans the cluster that every test shares, so each role takes the name of the test's
	// own database, which is random. other and login are members of group. login signs in with a
	// password, as a deployment's login does. The test database's own login is granted group with
	// SET, for the row whose runner signs in as it and sets the role: a login that is no superuser
	// but creates roles (CREATEROLE) is given ADMIN alone on a role it creates (Postgres 16 and
	// later), which lets it grant that role but not set it.
	group, other, login := config.Database+"_group", config.Database+"_other", config.Database+"_login"
	if _, err := admin.Exec(ctx, "create role "+group+"; create role "+other+" in role "+group+"; create role "+login+" login password 'exec-test' in role "+group+"; "+
		"grant "+group+" to current_user with set true; "+
		"create schema x; create table seen (search_path text, current_user_name text, session_user_name text); grant insert on seen to public"); err != nil {
		t.Fatal(err)
	}
	// Registered before any runner connects, so every runner's connection closes first.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop role "+other+", "+login+", "+group); err != nil {
			t.Errorf("drop the test's roles: %v", err)
		}
	})
	type names struct{ searchPath, currentUser, sessionUser string }
	read := func(t *testing.T, conn *pgx.Conn) names {
		t.Helper()
		var got names
		if err := conn.QueryRow(ctx, "select current_setting('search_path'), current_user::text, session_user::text").Scan(&got.searchPath, &got.currentUser, &got.sessionUser); err != nil {
			t.Fatalf("read search_path, current_user and session_user: %v", err)
		}
		return got
	}
	for _, row := range []struct {
		name string
		// user is the login the runner's connection signs in as, "" for the test database's own.
		user string
		set  string
		// superuser is whether set needs the runner's login to be a superuser.
		superuser bool
	}{
		{"a session set of search_path", "", "set search_path = x", false},
		{"a set role", "", "set role " + group, false},
		{"a set session authorization", "", "set session authorization " + other, true},
		{"all three", "", "set session authorization " + other + "; set role " + group + "; set search_path = x", true},
		{"a set role and a set of search_path on a login that is no superuser", login, "set role " + group + "; set search_path = x", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.superuser && !superuser {
				testdb.SkipOutsideCI(t, "%q needs the test database's login, %s, to be a superuser, which the server says it is not (rolsuper): Postgres lets only a superuser's session set session authorization", row.set, config.User)
			}
			runnerConfig := config.Copy()
			if row.user != "" {
				runnerConfig.User, runnerConfig.Password = row.user, "exec-test"
			}
			runner, err := pgx.ConnectConfig(ctx, runnerConfig)
			if err != nil {
				t.Fatalf("connect the runner: %v", err)
			}
			t.Cleanup(func() { runner.Close(context.Background()) })
			reset := read(t, runner)
			if _, err := runner.Exec(ctx, row.set); err != nil {
				t.Fatalf("%s: %v", row.set, err)
			}
			changed := read(t, runner)
			if changed == reset {
				t.Fatalf("%s changed none of search_path, current_user and session_user (%+v), so the row proves nothing", row.set, changed)
			}
			tx, err := runner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := Exec(ctx, tx, Migration{Version: 1, Name: "0001_seen.up.sql", SQL: "insert into public.seen select current_setting('search_path'), current_user, session_user"}, nil); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if after := read(t, runner); after != changed {
				t.Fatalf("after the commit the runner's connection has %+v, want %+v, as the row set it", after, changed)
			}
			var seen names
			if err := admin.QueryRow(ctx, "delete from seen returning search_path, current_user_name, session_user_name").Scan(&seen.searchPath, &seen.currentUser, &seen.sessionUser); err != nil {
				t.Fatalf("read what the migration ran under: %v", err)
			}
			if seen != reset {
				t.Errorf("the migration ran under %+v, want %+v, the values that RESET gives", seen, reset)
			}
		})
	}
}

// stringConstants appends to found every string constant of a parse tree, read from libpg_query's
// JSON: an A_Const writes one as {"sval": {"sval": "…"}}.
func stringConstants(node any, found *[]string) {
	switch node := node.(type) {
	case map[string]any:
		for key, value := range node {
			if s, ok := value.(string); ok && key == "sval" {
				*found = append(*found, s)
				continue
			}
			stringConstants(value, found)
		}
	case []any:
		for _, value := range node {
			stringConstants(value, found)
		}
	}
}
