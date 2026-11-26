package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The owner lock beside an earlier build (ownerlock_fleet_test.go): the earlier build's arrival
// and the owner lock gate a writer of this build holds or tries.

// ───── an earlier build's arrival and the owner lock gate ──────────────────

// An earlier build's arrival - its dashboard publish, the first thing its boot writes, or its
// registration - waits while a transaction of this build that read no earlier build in the fleet
// is between its owner lock and its write: that transaction holds the owner lock gate shared, and
// the arrival's insert takes it exclusively (owner_lock_legacy_arrival,
// owner_lock_legacy_registration). It goes on once that transaction commits, and the next owner
// lock reads the earlier build in the fleet and takes the owner's row, without the gate; a second
// arrival then waits for nothing, though that writer is between its owner lock and its write too.
//
// The arrival is read waiting when the writer is first between its lock and its write and again
// as the writer lets the gate go, and its gate function catches no error (gateWaitsEndOnlyByTheLock),
// so the wait ends only when the gate is granted, or by an error the Receive below fails on: an
// arrival cannot give its wait up and go through.
func TestAnEarlierBuildsArrivalWaitsForATransactionThatReadNoEarlierBuild(t *testing.T) {
	for _, arrival := range earlierBuildArrivals {
		t.Run(arrival.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			gateWaitsEndOnlyByTheLock(t, s)
			ctx := context.Background()
			waitRegistrationsOut(ctx, t, s)
			writer, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if err := LockOwner(ctx, writer, owners.issueA); err != nil {
				t.Fatal(err)
			}
			if mode := lastOwnerLockMode(); mode != ownerLockAdvisory {
				t.Fatalf("the writer took the %s mode with no earlier build in the fleet, want %s", mode, ownerLockAdvisory)
			}
			if gate, granted := gateLock(t, s, writer.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
				t.Fatalf("the writer holds the gate as %q (granted %t), want it held shared", gate, granted)
			}
			var seq int
			if err := writer.QueryRow(ctx, `select last_seq from issues where key = $1`, owners.issueA.Key).Scan(&seq); err != nil {
				t.Fatal(err)
			}

			conn, err := s.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			arrivalPID := conn.Conn().PgConn().PID()
			arrived := make(chan error, 1)
			go func() { arrived <- arriveOn(ctx, t, conn, arrival, uuid.NewString()) }()
			awaitGateWait(t, s, arrivalPID, arrived, arrival.name)
			// The arrival waits for the gate the writer holds shared (awaitGateWait read it
			// waiting), so it cannot end before the writer commits: that state is what this reads.
			select {
			case err := <-arrived:
				t.Fatalf("%s ended (%v) while the writer was between its owner lock and its write", arrival.name, err)
			default:
			}

			if _, err := writer.Exec(ctx, ownerLock, OwnerLockNamespace, owners.issueA.LockKey()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `update issues set last_seq = $2 where key = $1`, owners.issueA.Key, seq+1); err != nil {
				t.Fatal(err)
			}
			if gate, granted := gateLock(t, s, arrivalPID); gate != "ExclusiveLock" || granted {
				t.Fatalf("%s holds the gate as %q (granted %t) as the writer is about to commit, want it still waiting for it exclusively: it gave its wait up", arrival.name, gate, granted)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := testwait.Receive(t, arrival.name+" to end once the writer committed", arrived); err != nil {
				t.Fatalf("%s after the writer committed: %v", arrival.name, err)
			}

			next, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Rollback(ctx)
			if err := LockOwner(ctx, next, owners.issueA); err != nil {
				t.Fatal(err)
			}
			if mode := lastOwnerLockMode(); mode != ownerLockAdvisoryAndRow {
				t.Errorf("the writer after %s took the %s mode, want %s", arrival.name, mode, ownerLockAdvisoryAndRow)
			}
			if !ownerRowHeld(t, s, owners.issueA) {
				t.Errorf("the writer after %s holds no row of %s, want it held beside the advisory lock", arrival.name, owners.issueA.LockKey())
			}
			if gate, _ := gateLock(t, s, next.Conn().PgConn().PID()); gate != "" {
				t.Errorf("the writer after %s holds the gate as %q, want no gate in the mode beside an earlier build", arrival.name, gate)
			}
			againConn, err := s.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer againConn.Release()
			again := make(chan error, 1)
			go func() { again <- arriveOn(ctx, t, againConn, arrival, uuid.NewString()) }()
			answer := awaitUnblockedAnswer(t, s, againConn.Conn().PgConn().PID(), again, "a second arrival")
			if !answer.answered {
				t.Fatalf("a second arrival waited for backend(s) %v beside a writer that holds no gate", answer.blockers)
			}
			if answer.err != nil {
				t.Fatalf("a second arrival beside a writer in the mode beside an earlier build: %v", answer.err)
			}
		})
	}
}

// gateWaitsEndOnlyByTheLock fails t unless every function that takes the owner lock gate for an
// earlier build's arrival (owner_lock_legacy_arrival, owner_lock_legacy_registration) takes it
// with the waiting pg_advisory_xact_lock and catches no error: its wait then ends only when the
// gate is granted, or with the error its lock_timeout raises (55P03), and never by giving the
// wait up and going on without the gate, which a wait read as an order of events cannot see.
func gateWaitsEndOnlyByTheLock(t *testing.T, s *Store) {
	t.Helper()
	for _, function := range []string{"owner_lock_legacy_arrival", "owner_lock_legacy_registration"} {
		var source string
		if err := s.Pool.QueryRow(context.Background(), `select prosrc from pg_proc where proname = $1`, function).Scan(&source); err != nil {
			t.Fatalf("read %s's body: %v", function, err)
		}
		body := strings.ToLower(source)
		if !strings.Contains(body, "pg_catalog.pg_advisory_xact_lock(") || strings.Contains(body, "pg_try_advisory") || strings.Contains(body, "exception") {
			t.Fatalf("%s takes the owner lock gate in a way that can end its wait without the gate: want pg_catalog.pg_advisory_xact_lock and no exception handler or try lock in\n%s", function, source)
		}
	}
}

// fleetReadPauseNamespace and fleetReadPauseKey are the two keys of the advisory lock a test holds
// to stop a writer inside its first fleet read ("PAUS", 0; pauseFleetReadsAt). No two-key lock the
// module's code takes has that first key - OwnerLockNamespace's comment lists those, and this
// test's lock, which no code outside this file takes, is not one of them - so the pause and the
// module's own locks never wait on each other.
const (
	fleetReadPauseNamespace int32 = 0x50415553
	fleetReadPauseKey       int32 = 0
)

// pauseFleetReadsAt makes every fleet read (public.owner_lock_fleet_read) of a transaction running
// as the role it answers stop at the server_processes row of instance until the transaction it
// answers ends, which holds the pause; the caller commits it to let the reads go on. The pause is
// a row-level security policy on server_processes, which Postgres checks for each row a scan
// reads, so a fleet read stops inside its statement, after that statement's snapshot is taken.
// The policy then hides the row, which the caller leaves a live process of an earlier build to
// every other condition of the fleet read, so the policy reaches it in whatever order the scan
// checks them. Superusers and the table's owner - the test's login, which migrated the database -
// skip row-level security, so the writer runs as the role (`set local role`), which may read the
// fleet read's two tables and take an issue's row.
//
// The login need not be a superuser: one with CREATEROLE creates the role and grants it to itself
// with SET, which Postgres 16 and later give such a creator only on a grant of its own (the create
// gives it ADMIN alone), and with INHERIT, which `drop owned by` of the role needs when the
// cleanup makes one. A login that cannot create a role skips the test, and fails it on GitHub
// Actions, whose server has that privilege (testdb.SkipOutsideCI). A role spans the cluster, so its
// cleanup is registered as soon as it exists (dropRoleAfterTestDatabase): it drops the test
// database first, which takes the role's grants and everything it owns there with it, and then
// the role, whatever failed first.
func pauseFleetReadsAt(t *testing.T, s *Store, instance string) (role string, pause pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	// Taken before the role exists, so the database it names outlives the role's cleanup.
	adminURL := testdb.DispatchServer.AdminURL(t)
	role = "fleet_read_pause_" + randomDatabaseSuffix(t)
	if _, err := s.Pool.Exec(ctx, "create role "+role+" nologin"); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			testdb.SkipOutsideCI(t, "the test server's login cannot create the role the fleet read's pause runs as (it needs CREATEROLE or superuser): %v", err)
		}
		t.Fatalf("create the fleet read's pause role: %v", err)
	}
	t.Cleanup(func() { dropRoleAfterTestDatabase(t, s, role, adminURL) })
	if _, err := s.Pool.Exec(ctx, fmt.Sprintf(`
		grant %s to current_user with set true, inherit true;
		create function public.fleet_read_pause(instance uuid) returns boolean language plpgsql as $$
		begin
			if instance = '%s'::uuid then
				perform pg_catalog.pg_advisory_xact_lock_shared(%d, %d);
				return false;
			end if;
			return true;
		end $$;
		alter table public.server_processes enable row level security;
		create policy fleet_read_pause on public.server_processes for select
			using (public.fleet_read_pause(instance));
		grant select on public.server_processes, public.owner_lock_legacy_mark to %s;
		grant select, update on public.issues to %s;`,
		role, instance, fleetReadPauseNamespace, fleetReadPauseKey, role, role)); err != nil {
		t.Fatalf("set the fleet read's pause up: %v", err)
	}
	pause, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pause.Rollback(context.Background()) })
	if _, err := pause.Exec(ctx, `select pg_advisory_xact_lock($1, $2)`, fleetReadPauseNamespace, fleetReadPauseKey); err != nil {
		t.Fatalf("hold the fleet read's pause: %v", err)
	}
	return role, pause
}

// dropRoleAfterTestDatabase is the cleanup of a role a test creates in s, a database of the
// test's own (openEmptyTestStore), and is registered as soon as the role exists
// (pauseFleetReadsAt, and the census's roles in census_test.go). It drops that database and then
// the role, each a statement of its own on a connection of its own to adminURL, another database
// of the test's own on the same server (testdb.Server.AdminURL), taken before the role existed so
// that it outlives this cleanup, and runs the role's drop whatever the database's did. A role
// spans the cluster, and what a test grants it or lets it own is in the test database, so once
// that database is gone nothing depends on the role and its drop takes it, whatever the test left
// there: a table the role owns that a view of the login depends on refuses `drop owned by` of the
// role (2BP01) and a drop of the role, and not the database's drop. The test database's own drop
// (testdb.DatabaseOn) was registered before the role existed, so it runs after this cleanup and
// finds the database gone. A database whose drop fails is still there, and so are the role's
// grants in it, which refuse the role's drop: the cleanup then drops them first (`drop owned by`,
// in that database).
func dropRoleAfterTestDatabase(t *testing.T, s *Store, role, adminURL string) {
	t.Helper()
	ctx := context.Background()
	database := s.Pool.Config().ConnConfig.Database
	// The role's drop runs on the connection the database's drop ran on, or through the test
	// database's own pool when no connection to adminURL could be made.
	exec := s.Pool.Exec
	dropped := false
	if admin, err := pgx.Connect(ctx, adminURL); err != nil {
		t.Errorf("connect to the test's admin database to drop the database %s and then the role %s: %v", database, role, err)
	} else {
		defer admin.Close(ctx)
		exec = admin.Exec
		statement := "drop database if exists " + database + " with (force)"
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Errorf("%s: %v", statement, err)
		} else {
			dropped = true
		}
	}
	if !dropped {
		if _, err := s.Pool.Exec(ctx, "drop owned by "+role); err != nil {
			t.Errorf("drop owned by %s: %v", role, err)
		}
	}
	statement := "drop role if exists " + role
	if _, err := exec(ctx, statement); err != nil {
		t.Errorf("%s: %v", statement, err)
	}
}

// awaitFleetReadPause waits for backend pid to stop at the fleet read's pause (pauseFleetReadsAt),
// and fails the test when its owner lock answers first (done closes). The wait has no bound of its
// own (testwait.Eventually).
func awaitFleetReadPause(t *testing.T, s *Store, pid uint32, done <-chan struct{}) {
	t.Helper()
	testwait.Eventually(t, "the writer's fleet read to stop at the pause", func() bool {
		var waiting bool
		if err := s.Pool.QueryRow(context.Background(), `
			select exists (
				select from pg_locks
				where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
				  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3 and not granted)`,
			fleetReadPauseNamespace, fleetReadPauseKey, pid).Scan(&waiting); err != nil {
			t.Fatalf("read the fleet read's pause: %v", err)
		}
		if waiting {
			return true
		}
		select {
		case <-done:
			t.Fatal("the writer's owner lock answered before its fleet read stopped at the pause")
		default:
		}
		return false
	})
}

// The fleet read owner_lock_fleet makes after its try reads under a snapshot of its own, so an
// earlier build whose arrival commits after the first read and before the try is in the fleet by
// that second read alone. A writer of this build stops inside its first fleet read, after that
// read's snapshot is taken and before the try (pauseFleetReadsAt), holding no gate; the earlier
// build arrives - its dashboard publish, or its registration - and commits, taking the gate, which
// it finds free; then the writer goes on. Its first read cannot see the arrival, and its try takes
// the gate, which the writer then holds shared, so neither answered that an earlier build is in
// the fleet; the writer takes the owner's row all the same, as beside an earlier build, which only
// the second read can have told it. With no arrival the writer, stopped and let go alike, takes the
// advisory lock alone: the row the pause stops at counts for nothing. A second read that answered
// the first read's result again would leave the writer in the advisory mode beside the arrival.
func TestTheFleetReadAfterTheGateSeesAnArrivalCommittedAfterTheFirstRead(t *testing.T) {
	cases := []struct {
		name    string
		arrival *earlierBuildArrival
	}{
		{name: "no arrival"},
		{name: earlierBuildArrivals[0].name, arrival: &earlierBuildArrivals[0]},
		{name: earlierBuildArrivals[1].name, arrival: &earlierBuildArrivals[1]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			ctx := context.Background()
			// The pause's row: registered at this build's protocol, which makes no arrival and
			// stamps no mark, then moved to 0, which no trigger watches, so that every condition
			// of the fleet read but the pause's policy holds for it.
			paused := registerFleetProcess(t, s, OwnerLockProtocol, 0)
			if _, err := s.Pool.Exec(ctx, `update server_processes set owner_lock_protocol = 0 where instance = $1::uuid`, paused); err != nil {
				t.Fatal(err)
			}
			role, pause := pauseFleetReadsAt(t, s, paused)

			writer, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, "set local role "+role); err != nil {
				_ = writer.Rollback(ctx)
				t.Fatal(err)
			}
			writerPID := writer.Conn().PgConn().PID()
			locked := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				locked <- LockOwner(ctx, writer, owners.issueA)
				close(done)
			}()
			defer func() {
				_ = pause.Rollback(ctx)
				testwait.Receive(t, "the writer's LockOwner to return once the pause rolled back", done)
				_ = writer.Rollback(ctx)
			}()
			awaitFleetReadPause(t, s, writerPID, done)
			if gate, granted := gateLock(t, s, writerPID); gate != "" {
				t.Fatalf("the writer stopped at the pause holds the gate as %q (granted %t), want no gate: the pause is in its first fleet read, before its try", gate, granted)
			}
			if c.arrival != nil {
				arrivalConn, err := s.Pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer arrivalConn.Release()
				arrived := make(chan error, 1)
				go func() { arrived <- arriveOn(ctx, t, arrivalConn, *c.arrival, uuid.NewString()) }()
				answer := awaitUnblockedAnswer(t, s, arrivalConn.Conn().PgConn().PID(), arrived, c.arrival.name)
				if !answer.answered {
					t.Fatalf("%s waited for backend(s) %v beside a writer stopped before its try, which holds no gate", c.arrival.name, answer.blockers)
				}
				if answer.err != nil {
					t.Fatalf("%s while the writer was stopped inside its first fleet read: %v", c.arrival.name, answer.err)
				}
			}
			if err := pause.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			testwait.Receive(t, "the writer's owner lock to answer once the pause ended", done)
			if err := <-locked; err != nil {
				t.Fatalf("the writer's owner lock: %v", err)
			}
			want := ownerLockAdvisory
			if c.arrival != nil {
				want = ownerLockAdvisoryAndRow
			}
			if mode := lastOwnerLockMode(); mode != want {
				t.Errorf("the writer after %s took the %s mode, want %s", c.name, mode, want)
			}
			if held := ownerRowHeld(t, s, owners.issueA); held != (c.arrival != nil) {
				t.Errorf("after %s the writer holds the row of %s: %t, want %t", c.name, owners.issueA.LockKey(), held, c.arrival != nil)
			}
			if gate, granted := gateLock(t, s, writerPID); gate != "ShareLock" || !granted {
				t.Errorf("after %s the writer holds the gate as %q (granted %t), want it held shared: its try took the gate and answered no earlier build", c.name, gate, granted)
			}
		})
	}
}

// lockAnswer is what a transaction's lock came to: its answer, or, before it answered, the
// backends Postgres records its backend as waiting for, and what that backend was doing then
// (its pid, state and wait event in pg_stat_activity), which a give-up prints.
type lockAnswer struct {
	answered bool
	err      error
	blockers []int32
	backend  string
}

// awaitUnblockedAnswer waits for what runs on backend pid - an owner lock, an arrival - to answer
// on answered, and reads between its questions whom Postgres records that backend as waiting for
// (pg_blocking_pids): one that queues behind another transaction is seen waiting there, and the
// wait ends with those backends rather than at any span of the clock. Its one bound is the test
// binary's deadline less testwait's margin (testwait.Until). A try that finds neither reads what
// the backend is doing, so a give-up names it.
func awaitUnblockedAnswer(t *testing.T, s *Store, pid uint32, answered <-chan error, what string) lockAnswer {
	t.Helper()
	return testwait.Until(t, what+" to answer or to wait for another backend", func() (lockAnswer, bool) {
		select {
		case err := <-answered:
			return lockAnswer{answered: true, err: err}, true
		default:
		}
		var blockers []int32
		var backend string
		if err := s.Pool.QueryRow(context.Background(), `
			select pg_blocking_pids($1),
				coalesce((select format('backend %s %s (wait %s/%s)', pid, state, wait_event_type, wait_event)
					from pg_stat_activity where pid = $1), format('backend %s not in pg_stat_activity', $1))`,
			int32(pid)).Scan(&blockers, &backend); err != nil {
			t.Fatalf("read whom backend %d waits for: %v", pid, err)
		}
		return lockAnswer{blockers: blockers, backend: backend}, len(blockers) != 0
	})
}

// A writer of this build that starts while an earlier build's arrival waits for the owner lock gate
// never queues behind that arrival. The arrival waits for a transaction that read no earlier build
// and holds the gate shared; the writer's owner lock tries the gate shared, which Postgres refuses
// while the arrival's exclusive request waits, and so takes its owners' rows, as it does beside an
// earlier build, without waiting. Each read-phase owner lock - LockOwner, LockOwnersShared,
// TryLockOwners - answers in the mode beside an earlier build, holding its owner's row and no gate,
// and the arrival is still waiting after them. No clock decides the row: while each writer runs,
// Postgres's record of whom its backend waits for is read (awaitUnblockedAnswer), and a writer seen
// waiting for another backend fails the row naming it. With the gate waited for rather than tried,
// the first of them queues behind the arrival until the transaction it waits for ends, and that
// read sees it.
func TestAWriterStartedWhileAnArrivalWaitsForTheGateTakesTheOwnersRowsAtOnce(t *testing.T) {
	for _, arrival := range earlierBuildArrivals {
		t.Run(arrival.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			ctx := context.Background()
			waitRegistrationsOut(ctx, t, s)
			holder, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback(ctx)
			if err := LockOwner(ctx, holder, owners.project); err != nil {
				t.Fatal(err)
			}
			if gate, granted := gateLock(t, s, holder.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
				t.Fatalf("the transaction in flight holds the gate as %q (granted %t), want it held shared", gate, granted)
			}
			conn, err := s.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			arrivalPID := conn.Conn().PgConn().PID()
			arrived := make(chan error, 1)
			go func() { arrived <- arriveOn(ctx, t, conn, arrival, uuid.NewString()) }()
			awaitGateWait(t, s, arrivalPID, arrived, arrival.name)

			writers := []struct {
				name  string
				owner Owner
				lock  func(tx pgx.Tx) error
			}{
				{"LockOwner", owners.issueA, func(tx pgx.Tx) error { return LockOwner(ctx, tx, owners.issueA) }},
				{"LockOwnersShared", owners.issueB, func(tx pgx.Tx) error { return LockOwnersShared(ctx, tx, owners.issueB) }},
				{"TryLockOwners", owners.document, func(tx pgx.Tx) error {
					taken, err := TryLockOwners(ctx, tx, owners.document)
					if err == nil && len(taken) != 1 {
						err = fmt.Errorf("took %v, want %s", lockKeys(taken), owners.document.LockKey())
					}
					return err
				}},
			}
			writerConn, err := s.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writerConn.Release()
			writerPID := writerConn.Conn().PgConn().PID()
			for _, writer := range writers {
				tx, err := writerConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				locked := make(chan error, 1)
				go func() { locked <- writer.lock(tx) }()
				answer := awaitUnblockedAnswer(t, s, writerPID, locked, writer.name)
				if !answer.answered {
					t.Errorf("%s waited for backend(s) %v while %s (backend %d) waited for the gate; want it to try the gate, not queue behind the arrival",
						writer.name, answer.blockers, arrival.name, arrivalPID)
					_ = holder.Rollback(ctx)
					testwait.Receive(t, writer.name+"'s owner lock once the transaction the arrival waited for ended", locked)
					_ = tx.Rollback(ctx)
					t.FailNow()
				}
				if answer.err != nil {
					t.Fatalf("%s: %v", writer.name, answer.err)
				}
				if mode := lastOwnerLockMode(); mode != ownerLockAdvisoryAndRow {
					t.Errorf("%s while %s waited for the gate took the %s mode, want %s", writer.name, arrival.name, mode, ownerLockAdvisoryAndRow)
				}
				if !ownerRowHeld(t, s, writer.owner) {
					t.Errorf("%s while %s waited for the gate holds no row of %s, want it held beside the advisory lock", writer.name, arrival.name, writer.owner.LockKey())
				}
				if gate, granted := gateLock(t, s, writerPID); gate != "" {
					t.Errorf("%s while %s waited for the gate holds it as %q (granted %t), want no gate", writer.name, arrival.name, gate, granted)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if gate, granted := gateLock(t, s, arrivalPID); gate != "ExclusiveLock" || granted {
				t.Errorf("after the writers %s holds the gate as %q (granted %t), want it still waiting for the transaction in flight", arrival.name, gate, granted)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := testwait.Receive(t, arrival.name+" to end once the transaction it waited for committed", arrived); err != nil {
				t.Fatalf("%s after the transaction in flight committed: %v", arrival.name, err)
			}
		})
	}
}
