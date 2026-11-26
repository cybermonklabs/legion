package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// The owner lock beside an earlier build (ownerlock_fleet_test.go): the earlier build's mark, as
// the migration stamps it from an earlier build's publish and as it expires under writers in
// flight.

// ───── the earlier build's mark: the migration's, and its expiry ───────────

// ownerLockLegacyGateVersion is migration 20261115200014_owner_lock_legacy_gate's version.
const ownerLockLegacyGateVersion = 20261115200014

// The migration stamps the earlier build's mark at the newest dashboard publish of a build before
// it, when that publish is within the mark's window. An earlier build whose publish committed just
// before the migration ran made no arrival, since the trigger was not there yet, and it is not
// registered yet: a write of this build that starts then reads the mark and takes the owner's row
// beside its advisory lock, so while it is held between its owner lock and its write phase, the
// earlier build's write of the same owner - the row first - waits for it, and lands after it: both
// increments are kept. A publish older than the window, and no publish at all, which is what every
// test database has, leave no mark, and the write takes the advisory lock alone; a publish a minute
// inside the window is marked, so the migration's window is OwnerLockMarkWindow on both sides.
// Without the migration's insert the earlier build's write goes through while this build's is
// held, and is lost under it.
func TestTheMigrationMarksAnEarlierBuildThatPublishedBeforeIt(t *testing.T) {
	cases := []struct {
		name    string
		publish bool
		age     time.Duration
		legacy  bool
	}{
		{name: "an earlier build's publish just before the migration", publish: true, legacy: true},
		{name: "an earlier build's publish inside the mark's window", publish: true, age: OwnerLockMarkWindow - time.Minute, legacy: true},
		{name: "an earlier build's publish before the mark's window", publish: true, age: OwnerLockMarkWindow + time.Minute},
		{name: "no publish before the migration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := openEmptyTestStore(t)
			if err := s.migrate(ctx, migrationsThrough(t, ownerLockLegacyGateVersion-1)); err != nil {
				t.Fatalf("migrate through %d: %v", ownerLockLegacyGateVersion-1, err)
			}
			if _, err := s.Pool.Exec(ctx, `
				insert into projects (key, name, default_assignee) values ('FLT', 'Fleet', 'alice@example.com');
				insert into issues (key, number, project_key, rank, title, created_by) values ('FLT-1', 1, 'FLT', 'a', 'One', '{}')`); err != nil {
				t.Fatalf("seed the owner: %v", err)
			}
			issue := IssueOwner("FLT-1")
			if c.publish {
				// The earlier build's publish as legion-envoy-v30.24.0's dashboard.Publish writes it.
				if _, err := s.Pool.Exec(ctx, `
					insert into dashboard_builds (build, entry, commit) values ('earlier', '/assets/index-earlier.js', 'earlier0build')
					on conflict (build) do update set last_started_at = now()`); err != nil {
					t.Fatalf("publish as an earlier build: %v", err)
				}
				if _, err := s.Pool.Exec(ctx, `update dashboard_builds set last_started_at = now() - make_interval(secs => $1)`, c.age.Seconds()); err != nil {
					t.Fatalf("age the earlier build's publish: %v", err)
				}
			}
			if err := s.migrate(ctx, migrationFiles); err != nil {
				t.Fatalf("migrate from %d: %v", ownerLockLegacyGateVersion, err)
			}
			if marked := hasLegacyMark(t, s); marked != c.legacy {
				t.Errorf("after the migration the earlier build's mark is stamped: %t, want %t", marked, c.legacy)
			}

			writer, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if err := LockOwner(ctx, writer, issue); err != nil {
				t.Fatal(err)
			}
			want := ownerLockAdvisory
			if c.legacy {
				want = ownerLockAdvisoryAndRow
			}
			if mode := lastOwnerLockMode(); mode != want {
				t.Errorf("this build's write after the migration took the %s mode, want %s", mode, want)
			}
			if !c.legacy {
				return
			}
			var seq int
			if err := writer.QueryRow(ctx, `select last_seq from issues where key = $1`, issue.Key).Scan(&seq); err != nil {
				t.Fatal(err)
			}
			writerPID := writer.Conn().PgConn().PID()
			wrote := make(chan error, 1)
			go func() {
				_, err := raceWriter(ctx, t, s, rowFirstWriter, []Owner{issue}, []Owner{issue})
				wrote <- err
			}()
			// The earlier build's write either queues behind this build's write, which Postgres
			// records as backend writerPID blocking it, or goes through, which is the defect. The
			// wait ends on whichever comes first, and gives up only 30 s before the test binary's
			// deadline (testwait.Until), so a write that is merely slow to start is no failure.
			type earlierWrite struct {
				Ended  bool
				Err    error
				Queued bool
			}
			// A try that finds the earlier write neither ended nor queued reads every backend of the
			// database: what it is doing, what it waits on, and the backends that block it.
			var got earlierWrite
			testwait.Until(t, "the earlier build's write to go through or to queue behind this build's write", func() (string, bool) {
				select {
				case err := <-wrote:
					got = earlierWrite{Ended: true, Err: err}
					return fmt.Sprintf("ended: %v", err), true
				default:
				}
				rows, err := s.Pool.Query(ctx, `
					select pid, coalesce(state, ''), coalesce(wait_event_type || '/' || wait_event, ''), pg_blocking_pids(pid)
					from pg_stat_activity where datname = current_database() order by pid`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				queued := false
				var backends []string
				for rows.Next() {
					var pid int64
					var state, waits string
					var blocking []int64
					if err := rows.Scan(&pid, &state, &waits, &blocking); err != nil {
						t.Fatal(err)
					}
					queued = queued || slices.Contains(blocking, int64(writerPID))
					backends = append(backends, fmt.Sprintf("%d %s %s blocked by %v", pid, state, waits, blocking))
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				reading := fmt.Sprintf("this build's writer is backend %d; the database's backends: %v", writerPID, backends)
				if queued {
					got = earlierWrite{Queued: true}
				}
				return reading, queued
			})
			if got.Ended {
				if got.Err != nil {
					t.Fatalf("the earlier build's write: %v", got.Err)
				}
				t.Errorf("the earlier build's write went through while this build's write was between its owner lock and its write phase; want it to wait for the owner's row")
			}
			if _, err := writer.Exec(ctx, ownerLock, OwnerLockNamespace, issue.LockKey()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue.Key); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `update issues set last_seq = $2 where key = $1`, issue.Key, seq+1); err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if !got.Ended {
				if err := testwait.Receive(t, "the earlier build's write to end once this build's committed", wrote); err != nil {
					t.Fatalf("the earlier build's write after this build's committed: %v", err)
				}
			}
			if got := ownerSeqs(t, s, []Owner{issue})[issue]; got != seq+2 {
				t.Errorf("the issue's last_seq is %d after both writes from %d, want %d: the earlier build's write was lost under this build's", got, seq, seq+2)
			}
		})
	}
}

// The earlier build's mark expires while writers of this build are in flight in both modes:
// those whose fleet read found the mark take the owners' rows beside their advisory locks, and
// those that start once it has expired take the advisory locks alone, behind the gate, while
// the first are still committing. The earlier build's own writers, the rows first, write only
// while the mark is young, as an earlier build that has stopped writes nothing more: an earlier
// build that still writes past the mark is in the fleet only by its registration, and "What the
// fleet does not see" (cmd/dispatch/README.md) names the three cases where it is not. No update
// is lost, nothing deadlocks and nothing else fails, and this build's writers commit in both
// modes. The test expires the mark itself, as the passing of its window would (ageLegacyMark),
// once the two builds' writes have overlapped - an earlier build's writer has committed, and a
// writer of this build has committed beside the earlier build after it - and the earlier build's
// writers have stopped, each write of theirs in flight ended, so the expiry falls inside the run
// at a point the run has reached, however slow the machine: a window of real time could close
// before the earlier build's writers had written at all, or while an earlier build's write was
// still in flight. The mark's age is read by the database's clock, the clock the fleet read
// judges it by, and this build's writers run until one of them has committed in the advisory
// mode, which only a fleet read that no longer counts the mark answers.
func TestTheMarkExpiresWhileWritersOfBothModesAreInFlight(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	all := owners.all()
	start := ownerSeqs(t, s, all)
	ledger := newRaceLedger()
	publishAsAnEarlierBuild(t, s)
	// The earlier build's writers have a stop and a join of their own, apart from this build's: the
	// row stops them, and waits for their writes in flight to end, before it ages the mark. Each join
	// is the shared wait's (testwait.Returns), outside the once that closes the stop, since a
	// give-up inside a sync.OnceFunc becomes a panic with a nil argument; a second call closes
	// nothing and finds the writers already joined.
	earlierStop := make(chan struct{})
	var earlier sync.WaitGroup
	closeEarlierStop := sync.OnceFunc(func() { close(earlierStop) })
	stopEarlier := func() {
		closeEarlierStop()
		testwait.Returns(t, "the earlier build's writers to end their writes in flight once stopped", earlier.Wait)
	}
	t.Cleanup(stopEarlier)
	// commits is how many writers of each build the ledger has seen commit: the earlier build's,
	// and this build's beside the earlier build (its fleet read found the mark).
	commits := func() (earlierBuild, besideAnEarlierBuild int) {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if tally := ledger.tallies[string(rowFirstWriter)]; tally != nil {
			earlierBuild = tally.commits
		}
		return earlierBuild, ledger.modes[true]
	}
	committed := func(besideAnEarlierBuild bool) bool {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		return ledger.modes[besideAnEarlierBuild] > 0
	}

	stop := make(chan struct{})
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	var writers sync.WaitGroup
	// The writers stop and are joined however the wait below ends, so none outlives the test.
	closeStop := sync.OnceFunc(func() { close(stop) })
	end := func() {
		closeStop()
		testwait.Returns(t, "this build's writers to end their writes in flight once stopped", writers.Wait)
	}
	t.Cleanup(end)
	for range gateRaceWriters {
		writers.Go(func() {
			for !stopped() {
				ledger.write(ctx, t, s, fleetModeWriter, all)
			}
		})
	}
	for range 2 {
		earlier.Go(func() {
			for {
				select {
				case <-earlierStop:
					return
				default:
				}
				ledger.write(ctx, t, s, rowFirstWriter, all)
			}
		})
	}
	// The two builds' writes overlap before the earlier build stops: one of its writers commits,
	// and then a writer of this build commits beside it. A commit of this build's is counted only
	// once its transaction has returned, so each of the gateRaceWriters writers can hold one commit
	// made before the earlier build's that the ledger has not counted yet; the next commit counted
	// past those is one made after it.
	besideBefore := testwait.Until(t, "a writer of the earlier build to commit", func() (int, bool) {
		earlierBuild, besideAnEarlierBuild := commits()
		return besideAnEarlierBuild, earlierBuild > 0
	})
	testwait.Until(t, "a writer of this build to commit beside the earlier build after a writer of the earlier build committed", func() (string, bool) {
		earlierBuild, besideAnEarlierBuild := commits()
		return fmt.Sprintf("%d commits of the earlier build; %d of this build beside it, %d when the earlier build's first was counted",
			earlierBuild, besideAnEarlierBuild, besideBefore), besideAnEarlierBuild > besideBefore+gateRaceWriters
	})
	stopEarlier()
	ageLegacyMark(t, s, OwnerLockMarkWindow+time.Second)
	testwait.Eventually(t, "a writer of this build to commit in the advisory mode once the mark expired", func() bool {
		return committed(false)
	})
	end()

	// Both modes committed, which the waits above hold: each fails by name when no writer commits
	// in its mode.
	lost, deadlocks, failures := ledger.settle(t, s, all, start)
	t.Logf("the mark expired once a writer committed beside the earlier build: this build's writers committed %d in the mode beside an earlier build and %d in the advisory mode; %d increments lost",
		ledger.modes[true], ledger.modes[false], lost)
	if lost != 0 || deadlocks != 0 || failures != 0 {
		t.Errorf("%d increments lost, %d deadlocks, %d other failures; want none of each", lost, deadlocks, failures)
	}
}
