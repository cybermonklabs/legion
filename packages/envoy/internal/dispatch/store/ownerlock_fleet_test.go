package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The owner lock beside an earlier build (OwnerLockProtocol). A build before this one serialises
// an owner's writers on the owner's row, which it takes first and holds for its whole transaction,
// and never takes the advisory lock; so while one is in the fleet, every read-phase owner lock of
// this build takes the row too, right after the advisory lock and before the read it guards
// (lockOwnerRows), and while none is, it takes the owner lock gate shared before its first owner
// lock (OwnerLockGateNamespace), which an earlier build's arrival takes exclusively. These tests
// write that earlier build's process as the row it leaves in server_processes, owner_lock_protocol
// at its default 0, its boot's dashboard publish as the dashboard_builds row it leaves, the same,
// and the earlier build's writer as the statements it runs: the owner's row `for no key update`
// first, then the read, then the write.

// fleetOwners is the owners these tests lock, one of each kind and a second issue, as rows of a
// database of their own.
type fleetOwners struct {
	project, issueA, issueB, document Owner
}

// all is every owner of f.
func (f fleetOwners) all() []Owner { return []Owner{f.project, f.issueA, f.issueB, f.document} }

// openFleetStore opens a database of its own, migrated, and seeds a project, two issues and a
// project document: the owners of fleetOwners.
func openFleetStore(t *testing.T) (*Store, fleetOwners) {
	t.Helper()
	s := openEmptyTestStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('FLT', 'Fleet', 'alice@example.com');
		insert into issues (key, number, project_key, rank, title, created_by) values
			('FLT-1', 1, 'FLT', 'a', 'One', '{}'), ('FLT-2', 2, 'FLT', 'b', 'Two', '{}')`); err != nil {
		t.Fatalf("seed the owners: %v", err)
	}
	var document string
	if err := s.Pool.QueryRow(ctx, `
		insert into artifacts (created_by, kind, name, slug, project_key)
		values ('{}', 'doc', 'Notes', 'notes', 'FLT') returning id::text`).Scan(&document); err != nil {
		t.Fatalf("seed the project document: %v", err)
	}
	return s, fleetOwners{
		project:  ProjectOwner("FLT"),
		issueA:   IssueOwner("FLT-1"),
		issueB:   IssueOwner("FLT-2"),
		document: ArtifactOwner(document),
	}
}

// registerFleetProcess writes a server_processes row as a process of the given owner lock
// protocol leaves it, with a heartbeat age old, and returns its instance. A protocol of 0 is
// written by leaving the column out, as a build before the column writes its row, so its insert
// takes the owner lock gate and stamps the earlier build's mark (owner_lock_legacy_arrival).
func registerFleetProcess(t *testing.T, s *Store, protocol int, age time.Duration) string {
	t.Helper()
	var instance string
	var err error
	if protocol == 0 {
		err = s.Pool.QueryRow(context.Background(), `
			insert into server_processes (instance, signing_keys, heartbeat_at)
			values (gen_random_uuid(), '{fingerprint}', now() - make_interval(secs => $1))
			returning instance::text`, age.Seconds()).Scan(&instance)
	} else {
		err = s.Pool.QueryRow(context.Background(), `
			insert into server_processes (instance, signing_keys, heartbeat_at, owner_lock_protocol)
			values (gen_random_uuid(), '{fingerprint}', now() - make_interval(secs => $1), $2)
			returning instance::text`, age.Seconds(), protocol).Scan(&instance)
	}
	if err != nil {
		t.Fatalf("register a process of owner lock protocol %d: %v", protocol, err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, instance)
	})
	return instance
}

// ownerRowTable is the table and key column of owner's row.
func ownerRowTable(owner Owner) (table, column string) {
	switch owner.Kind {
	case OwnerArtifact:
		return "artifacts", "id"
	case OwnerIssue:
		return "issues", "key"
	}
	return "projects", "key"
}

// ownerRowHeld reports whether another transaction holds owner's row at a level `for no key
// update` conflicts with (`for share` or stronger), by trying it with nowait from the pool.
func ownerRowHeld(t *testing.T, s *Store, owner Owner) bool {
	t.Helper()
	table, column := ownerRowTable(owner)
	_, err := s.Pool.Exec(context.Background(),
		`select 1 from `+table+` where `+column+`::text = $1 for no key update nowait`, owner.Key)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.As(err, &pgErr) && pgErr.Code == "55P03":
		return true
	}
	t.Fatalf("probe the row of %s: %v", owner.LockKey(), err)
	return false
}

// publishAsAnEarlierBuild writes a dashboard_builds row as an earlier build's boot publishes it,
// with no owner_lock_protocol, so its insert takes the owner lock gate and stamps the earlier
// build's mark (owner_lock_legacy_arrival).
func publishAsAnEarlierBuild(t *testing.T, s *Store) {
	t.Helper()
	build := fmt.Sprintf("earlier-%d", time.Now().UnixNano())
	if _, err := s.Pool.Exec(context.Background(), `insert into dashboard_builds (build, entry) values ($1, '')`, build); err != nil {
		t.Fatalf("publish as an earlier build: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from dashboard_builds where build = $1`, build)
	})
}

// ageLegacyMark moves the earlier build's mark age into the past, as that much time passing after
// the arrival that stamped it would.
func ageLegacyMark(t *testing.T, s *Store, age time.Duration) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(),
		`update owner_lock_legacy_mark set seen_at = now() - make_interval(secs => $1)`, age.Seconds()); err != nil {
		t.Fatalf("age the earlier build's mark: %v", err)
	}
}

// forgetLegacyMark deletes the earlier build's mark when the test ends, so the next one starts
// from a fleet no earlier build has arrived in.
func forgetLegacyMark(t *testing.T, s *Store) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = s.Pool.Exec(context.Background(), `delete from owner_lock_legacy_mark`)
	})
}

// gateLock is the owner lock gate as backend pid holds or waits for it, by pg_locks: its mode
// ("ShareLock", "ExclusiveLock"; "" when pid neither holds nor waits for it) and whether it is
// granted.
func gateLock(t *testing.T, s *Store, pid uint32) (mode string, granted bool) {
	t.Helper()
	err := s.Pool.QueryRow(context.Background(), `
		select mode, granted from pg_locks
		where locktype = 'advisory' and database = (select oid from pg_database where datname = current_database())
		  and classid::bigint = $1 and objid::bigint = $2 and objsubid = 2 and pid = $3`,
		OwnerLockGateNamespace, OwnerLockGateKey, pid).Scan(&mode, &granted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the owner lock gate of backend %d: %v", pid, err)
	}
	return mode, granted
}

// fleetState is one state of the fleet a lock is taken beside.
type fleetState struct {
	name   string
	seed   func(t *testing.T, s *Store)
	legacy bool
}

// fleetStates is every fleet state the owner lock tells apart: an earlier build is in the fleet
// while a process of it has heartbeated within OwnerLockLegacyWindow, or its arrival - its
// dashboard publish or its registration - stamped the mark within it. A process of it whose
// heartbeat stopped within the window still counts, where the registry's own live window has
// long dropped it: a row counts until it is deregistered or pruned.
var fleetStates = []fleetState{
	{name: "no process registered", seed: func(*testing.T, *Store) {}},
	{name: "a live process of this build", seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, OwnerLockProtocol, 0) }},
	{name: "a process of an earlier build whose heartbeat stopped within the legacy window", legacy: true, seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, ProcessLiveWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
	}},
	{name: "a process of an earlier build whose heartbeat stopped before the legacy window", seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, OwnerLockLegacyWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
	}},
	{name: "a live process of an earlier build", legacy: true, seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, 0, 0) }},
	{name: "an earlier build's dashboard publish within the legacy window", legacy: true, seed: publishAsAnEarlierBuild},
	{name: "an earlier build's dashboard publish before the legacy window", seed: func(t *testing.T, s *Store) {
		publishAsAnEarlierBuild(t, s)
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
	}},
}

// fleetStateNamed is the state of fleetStates named name.
func fleetStateNamed(t *testing.T, name string) fleetState {
	t.Helper()
	for _, state := range fleetStates {
		if state.name == name {
			return state
		}
	}
	t.Fatalf("no fleet state is named %q", name)
	return fleetState{}
}

// Each read-phase owner lock takes its owners' rows, before it answers, exactly while an earlier
// build is in the fleet, and the owner lock gate, shared, exactly while none is: LockOwner,
// LockOwners over every kind, LockOwnerAndRead, LockOwnersAndRead, LockOwnersShared and
// TryLockOwners. A process of an earlier build whose heartbeat and mark are older than the window,
// or a live process of this build, makes it take no row.
func TestTheOwnerLockTakesTheOwnerRowWhileAnEarlierBuildIsInTheFleet(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	locks := []struct {
		name   string
		owners []Owner
		lock   func(tx pgx.Tx) error
	}{
		{"LockOwner", []Owner{owners.issueA}, func(tx pgx.Tx) error { return LockOwner(ctx, tx, owners.issueA) }},
		{"LockOwners", owners.all(), func(tx pgx.Tx) error { return LockOwners(ctx, tx, owners.all()...) }},
		{"LockOwnerAndRead", []Owner{owners.issueA}, func(tx pgx.Tx) error {
			var title string
			return LockOwnerAndRead(ctx, tx, owners.issueA, `select title from issues where key = $1`, []any{owners.issueA.Key}, &title)
		}},
		{"LockOwnersAndRead", []Owner{owners.issueB, owners.project}, func(tx pgx.Tx) error {
			var title string
			return LockOwnersAndRead(ctx, tx, []Owner{owners.issueB, owners.project}, `select title from issues where key = $1`, []any{owners.issueB.Key}, &title)
		}},
		{"LockOwnersShared", []Owner{owners.document, owners.issueA}, func(tx pgx.Tx) error {
			return LockOwnersShared(ctx, tx, owners.document, owners.issueA)
		}},
		{"TryLockOwners", []Owner{owners.issueB, owners.document}, func(tx pgx.Tx) error {
			taken, err := TryLockOwners(ctx, tx, owners.issueB, owners.document)
			if err == nil && len(taken) != 2 {
				err = fmt.Errorf("took %v, want both owners", lockKeys(taken))
			}
			return err
		}},
	}
	for _, state := range fleetStates {
		t.Run(state.name, func(t *testing.T) {
			forgetLegacyMark(t, s)
			state.seed(t, s)
			want := OwnerLockAdvisory
			if state.legacy {
				want = OwnerLockAdvisoryAndRow
			}
			for _, lock := range locks {
				tx, err := s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := lock.lock(tx); err != nil {
					_ = tx.Rollback(ctx)
					t.Fatalf("%s: %v", lock.name, err)
				}
				if mode := LastOwnerLockMode(); mode != want {
					t.Errorf("%s beside %s took the %s mode, want %s", lock.name, state.name, mode, want)
				}
				for _, owner := range owners.all() {
					held := ownerRowHeld(t, s, owner)
					named := slices.Contains(lock.owners, owner)
					if want := state.legacy && named; held != want {
						t.Errorf("%s of %v beside %s: the row of %s is held: %t, want %t",
							lock.name, lockKeys(lock.owners), state.name, owner.LockKey(), held, want)
					}
				}
				gate, granted := gateLock(t, s, tx.Conn().PgConn().PID())
				if wantGate := !state.legacy; (gate == "ShareLock" && granted) != wantGate || (!wantGate && gate != "") {
					t.Errorf("%s beside %s holds the owner lock gate as %q (granted %t), want it held shared: %t",
						lock.name, state.name, gate, granted, wantGate)
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// holdRowAsAnEarlierBuild holds owner's row `for no key update` in a transaction of its own, as
// a writer of an earlier build holds it from its first statement, and answers the transaction.
func holdRowAsAnEarlierBuild(t *testing.T, s *Store, owner Owner) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	table, column := ownerRowTable(owner)
	if _, err := holder.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
		t.Fatalf("hold the row of %s: %v", owner.LockKey(), err)
	}
	return holder
}

// awaitRowLockWaiter waits up to ten seconds for a backend to wait on a row lock in a statement
// of lockOwnerRows (an owner row's lock taken beside the advisory lock), or for answered.
func awaitRowLockWaiter(t *testing.T, s *Store, answered <-chan error) (error, bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-answered:
			return err, true
		default:
		}
		var waiting int
		if err := s.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
			  and query like '%= any($1::%order by%for %'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return nil, false
		}
		if time.Now().After(deadline) {
			t.Fatal("no owner lock waited on the owner's row within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The read a lock guards sees what a writer of an earlier build committed while it held the row:
// LockOwnerAndRead, sent with its read in one batch, waits on the row that writer holds and reads
// again behind it, so the read answers the title the writer wrote rather than the one before it.
// With no process of that build registered it does not wait, and reads the title as it stood.
func TestTheReadTheOwnerLockGuardsSeesTheCommitOfAWriterOfAnEarlierBuild(t *testing.T) {
	for _, state := range []fleetState{fleetStateNamed(t, "no process registered"), fleetStateNamed(t, "a live process of an earlier build")} {
		t.Run(state.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			state.seed(t, s)
			ctx := context.Background()
			holder := holdRowAsAnEarlierBuild(t, s, owners.issueA)
			if _, err := holder.Exec(ctx, `update issues set title = 'Closed by the earlier build' where key = $1`, owners.issueA.Key); err != nil {
				t.Fatal(err)
			}
			read := make(chan string, 1)
			answered := make(chan error, 1)
			go func() {
				tx, err := s.Pool.Begin(ctx)
				if err != nil {
					answered <- err
					return
				}
				defer tx.Rollback(ctx)
				var title string
				err = LockOwnerAndRead(ctx, tx, owners.issueA, `select title from issues where key = $1`, []any{owners.issueA.Key}, &title)
				read <- title
				answered <- err
			}()
			if state.legacy {
				if err, early := awaitRowLockWaiter(t, s, answered); early {
					t.Fatalf("LockOwnerAndRead answered (%v, %q) while a writer of an earlier build held the row; want it to wait for that row", err, <-read)
				}
			} else if err := <-answered; err != nil {
				t.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if state.legacy {
				if err := <-answered; err != nil {
					t.Fatal(err)
				}
			}
			want := "One"
			if state.legacy {
				want = "Closed by the earlier build"
			}
			if got := <-read; got != want {
				t.Errorf("the guarded read beside %s answered %q, want %q", state.name, got, want)
			}
		})
	}
}

// TryLockOwners beside a live process of an earlier build leaves out an owner whose row a writer of
// that build holds, as `for no key update skip locked` left it out, and takes the rows of the
// others; beside none, the row is not looked at and the owner is taken.
func TestTryLockOwnersSkipsAnOwnerWhoseRowAWriterOfAnEarlierBuildHolds(t *testing.T) {
	for _, state := range []fleetState{fleetStateNamed(t, "no process registered"), fleetStateNamed(t, "a live process of an earlier build")} {
		t.Run(state.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			state.seed(t, s)
			ctx := context.Background()
			holdRowAsAnEarlierBuild(t, s, owners.issueA)
			tx, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			taken, err := TryLockOwners(ctx, tx, owners.issueA, owners.issueB, owners.document)
			if err != nil {
				t.Fatal(err)
			}
			want := []Owner{owners.issueA, owners.issueB, owners.document}
			if state.legacy {
				want = []Owner{owners.issueB, owners.document}
			}
			if !slices.Equal(taken, want) {
				t.Errorf("TryLockOwners beside %s took %v, want %v", state.name, lockKeys(taken), lockKeys(want))
			}
			for _, owner := range []Owner{owners.issueB, owners.document} {
				if held := ownerRowHeld(t, s, owner); held != state.legacy {
					t.Errorf("beside %s the row of %s is held: %t, want %t", state.name, owner.LockKey(), held, state.legacy)
				}
			}
		})
	}
}

// ───── deadlocks and lost updates across the writers ───────────────────────

// writerShape is how one writer of the races below serialises: this build beside a process of an
// earlier build (advisory lock, then the rows), this build in the advisory mode (advisory lock,
// the rows only in the write phase), this build as its fleet read answers (either of those two,
// as LockOwners runs it), and the earlier build itself (the rows first, no advisory lock).
type writerShape string

const (
	legacyModeWriter writerShape = "this build, advisory and row"
	advisoryWriter   writerShape = "this build, advisory alone"
	fleetModeWriter  writerShape = "this build, as its fleet read answers"
	rowFirstWriter   writerShape = "earlier build, row first"
)

// raceWriter runs one transaction of shape over owners: the read phase reads each owner's
// last_seq, the write phase writes each one back plus one, in touch's order, as a read-modify-write
// whose base the read phase decided. It answers whether the transaction's fleet read found an
// earlier build in the fleet (fleetModeWriter alone; false for the others), and the transaction's
// error. This build's writers take their write phase's row locks in touch's order, an order of the
// handler's appends, which no rule fixes since the advisory locks they hold already exclude each
// other; the earlier build's take theirs sorted by `<kind>:<key>` up front.
func raceWriter(ctx context.Context, s *Store, shape writerShape, owners []Owner, touch []Owner) (legacy bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return false, err
	}
	switch shape {
	case legacyModeWriter:
		if err := LockOwners(ctx, tx, owners...); err != nil {
			return false, err
		}
	case advisoryWriter:
		if _, err := lockOwnerKeys(ctx, tx, owners); err != nil {
			return false, err
		}
	case fleetModeWriter:
		if legacy, err = lockOwnerKeys(ctx, tx, owners); err != nil {
			return legacy, err
		}
		if legacy {
			if err := lockOwnerRows(ctx, tx, owners, rowLockExclusive); err != nil {
				return legacy, err
			}
		}
	case rowFirstWriter:
		sorted := slices.Clone(owners)
		slices.SortFunc(sorted, func(a, b Owner) int { return strings.Compare(a.LockKey(), b.LockKey()) })
		for _, owner := range sorted {
			table, column := ownerRowTable(owner)
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return legacy, err
			}
		}
	}
	seen := map[Owner]int{}
	for _, owner := range owners {
		table, column := ownerRowTable(owner)
		var seq int
		if err := tx.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			return legacy, err
		}
		seen[owner] = seq
	}
	time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
	for _, owner := range touch {
		table, column := ownerRowTable(owner)
		if shape != rowFirstWriter {
			if _, err := tx.Exec(ctx, ownerLock, OwnerLockNamespace, owner.LockKey()); err != nil {
				return legacy, err
			}
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return legacy, err
			}
		}
		if _, err := tx.Exec(ctx, `update `+table+` set last_seq = $2 where `+column+`::text = $1`, owner.Key, seen[owner]+1); err != nil {
			return legacy, err
		}
	}
	return legacy, tx.Commit(ctx)
}

// raceTally is what one pair's rounds came to.
type raceTally struct {
	rounds, deadlocks, otherFailures, lost int
}

// raceShapes runs rounds of shape a and shape b at once over random subsets of owners, and counts
// the rounds Postgres broke with `deadlock detected` (40P01), the transactions that failed any
// other way, and the increments lost: every committed transaction adds one to each owner it
// touched, so an owner whose last_seq grew by less than its commits lost the difference.
func raceShapes(t *testing.T, s *Store, owners fleetOwners, a, b writerShape, rounds int) raceTally {
	t.Helper()
	ctx := context.Background()
	all := owners.all()
	start := map[Owner]int{}
	for _, owner := range all {
		table, column := ownerRowTable(owner)
		var seq int
		if err := s.Pool.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		start[owner] = seq
	}
	committed := map[Owner]int{}
	tally := raceTally{rounds: rounds}
	var mu sync.Mutex
	for range rounds {
		var wg sync.WaitGroup
		for _, shape := range []writerShape{a, b} {
			subset := slices.Clone(all)
			rand.Shuffle(len(subset), func(i, j int) { subset[i], subset[j] = subset[j], subset[i] })
			subset = subset[:2+rand.IntN(len(subset)-1)]
			touch := slices.Clone(subset)
			rand.Shuffle(len(touch), func(i, j int) { touch[i], touch[j] = touch[j], touch[i] })
			wg.Go(func() {
				_, err := raceWriter(ctx, s, shape, subset, touch)
				mu.Lock()
				defer mu.Unlock()
				var pgErr *pgconn.PgError
				switch {
				case err == nil:
					for _, owner := range touch {
						committed[owner]++
					}
				case errors.As(err, &pgErr) && pgErr.Code == "40P01":
					tally.deadlocks++
				default:
					tally.otherFailures++
					t.Logf("%s: %v", shape, err)
				}
			})
		}
		wg.Wait()
	}
	for _, owner := range all {
		table, column := ownerRowTable(owner)
		var seq int
		if err := s.Pool.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		tally.lost += start[owner] + committed[owner] - seq
	}
	return tally
}

// raceRounds is how many rounds each pair runs: each round is one transaction of each shape at
// once over random subsets of the four owners.
const raceRounds = 300

// This build's writer in the mode beside an earlier build deadlocks with neither this build's
// advisory writer nor the earlier build's row-first writer, and loses no update to either: it
// takes the advisory locks sorted by key, then the rows in the earlier build's own order, before
// it reads. The advisory writer beside the row-first writer is the pairing the mode exists to
// prevent - its read phase is not excluded by a writer that holds the row - and its count is
// reported rather than asserted: it loses updates, and it deadlocks where its write phase takes
// two rows in an order the row-first writer does not.
func TestTheOwnerLockModesRaceEachOtherAndTheEarlierBuildsWriter(t *testing.T) {
	s, owners := openFleetStore(t)
	registerFleetProcess(t, s, 0, 0)
	pairs := []struct {
		a, b   writerShape
		assert bool
	}{
		{legacyModeWriter, advisoryWriter, true},
		{legacyModeWriter, rowFirstWriter, true},
		{legacyModeWriter, legacyModeWriter, true},
		{advisoryWriter, rowFirstWriter, false},
	}
	for _, pair := range pairs {
		t.Run(fmt.Sprintf("%s against %s", pair.a, pair.b), func(t *testing.T) {
			tally := raceShapes(t, s, owners, pair.a, pair.b, raceRounds)
			t.Logf("%d rounds of %s against %s: %d deadlocks (40P01), %d other failures, %d increments lost",
				tally.rounds, pair.a, pair.b, tally.deadlocks, tally.otherFailures, tally.lost)
			if pair.assert && (tally.deadlocks > 0 || tally.otherFailures > 0 || tally.lost != 0) {
				t.Errorf("%s against %s: %d deadlocks, %d other failures, %d increments lost; want none of each",
					pair.a, pair.b, tally.deadlocks, tally.otherFailures, tally.lost)
			}
		})
	}
}

// ───── an earlier build's arrival and the owner lock gate ──────────────────

// registryLockKey is the registry's lock (processes.registryLock), which a registration takes
// before it reads the live rows and writes its own, and every heartbeat takes too; and
// dashboardAssetsLock the advisory lock every dashboard publish takes shared first
// (dashboard.lockName). Both spelled here because this package's tests cannot import those.
const (
	registryLockKey     int64 = 0x4450524F43524547
	dashboardAssetsLock       = "dispatch-dashboard-assets"
)

// earlierBuildArrival is one way an earlier build arrives in the fleet, its statements as its boot
// sends them (legion-envoy-v30.24.0's dashboard.Publish and processes.admit).
type earlierBuildArrival struct {
	name string
	// arrive runs the arrival in tx, which the caller commits, for key: the build's id, or the
	// process's instance.
	arrive func(ctx context.Context, tx pgx.Tx, key string) error
}

var earlierBuildArrivals = []earlierBuildArrival{
	{"its dashboard publish", func(ctx context.Context, tx pgx.Tx, build string) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock_shared(hashtext($1))`, dashboardAssetsLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select path from dashboard_build_assets where build = $1`, build); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into dashboard_builds (build, entry, commit) values ($1, $2, $3)
			on conflict (build) do update set last_started_at = now()`, build, "/assets/index-earlier.js", "")
		return err
	}},
	{"its registration", func(ctx context.Context, tx pgx.Tx, instance string) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLockKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `select instance from server_processes where heartbeat_at > now() - make_interval(secs => $1)`,
			ProcessLiveWindow.Seconds()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			insert into server_processes (instance, commit, dashboard_build, signing_keys,
				schema_version, schema_creatable, embedder_identity, file_store)
			values ($1, '', '', '{fingerprint}', 1, 1, '', false)
			on conflict (instance) do update set heartbeat_at = now()`, instance)
		return err
	}},
}

// arriveOn runs arrival for key in a transaction of its own on conn, bounded by a lock timeout so
// a wait that never ends fails rather than hangs, and commits it.
func arriveOn(ctx context.Context, conn *pgxpool.Conn, arrival earlierBuildArrival, key string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return err
	}
	if err := arrival.arrive(ctx, tx, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// arrive runs arrival for key on a connection of the pool's (arriveOn).
func arrive(ctx context.Context, s *Store, arrival earlierBuildArrival, key string) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return arriveOn(ctx, conn, arrival, key)
}

// An earlier build's arrival - its dashboard publish, the first thing its boot writes, or its
// registration - waits while a transaction of this build that read no earlier build in the fleet
// is between its owner lock and its write: that transaction holds the owner lock gate shared, and
// the arrival's insert takes it exclusively (owner_lock_legacy_arrival). It goes on once that
// transaction commits, and the next owner lock reads the earlier build in the fleet and takes the
// owner's row, without the gate; a second arrival then waits for nothing, though that writer is
// between its owner lock and its write too.
func TestAnEarlierBuildsArrivalWaitsForATransactionThatReadNoEarlierBuild(t *testing.T) {
	for _, arrival := range earlierBuildArrivals {
		t.Run(arrival.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			ctx := context.Background()
			writer, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(ctx)
			if err := LockOwner(ctx, writer, owners.issueA); err != nil {
				t.Fatal(err)
			}
			if mode := LastOwnerLockMode(); mode != OwnerLockAdvisory {
				t.Fatalf("the writer took the %s mode with no earlier build in the fleet, want %s", mode, OwnerLockAdvisory)
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
			go func() { arrived <- arriveOn(ctx, conn, arrival, uuid.NewString()) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if gate, granted := gateLock(t, s, arrivalPID); gate == "ExclusiveLock" && !granted {
					break
				}
				select {
				case err := <-arrived:
					t.Fatalf("%s ended (%v) while the writer held the gate shared; want it to wait for the gate", arrival.name, err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s did not wait for the gate within 10s", arrival.name)
				}
				time.Sleep(5 * time.Millisecond)
			}
			select {
			case err := <-arrived:
				t.Fatalf("%s ended (%v) while the writer was between its owner lock and its write", arrival.name, err)
			case <-time.After(300 * time.Millisecond):
			}

			if _, err := writer.Exec(ctx, ownerLock, OwnerLockNamespace, owners.issueA.LockKey()); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, `update issues set last_seq = $2 where key = $1`, owners.issueA.Key, seq+1); err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-arrived:
				if err != nil {
					t.Fatalf("%s after the writer committed: %v", arrival.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not end within 10s of the writer's commit", arrival.name)
			}

			next, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Rollback(ctx)
			if err := LockOwner(ctx, next, owners.issueA); err != nil {
				t.Fatal(err)
			}
			if mode := LastOwnerLockMode(); mode != OwnerLockAdvisoryAndRow {
				t.Errorf("the writer after %s took the %s mode, want %s", arrival.name, mode, OwnerLockAdvisoryAndRow)
			}
			if !ownerRowHeld(t, s, owners.issueA) {
				t.Errorf("the writer after %s holds no row of %s, want it held beside the advisory lock", arrival.name, owners.issueA.LockKey())
			}
			if gate, _ := gateLock(t, s, next.Conn().PgConn().PID()); gate != "" {
				t.Errorf("the writer after %s holds the gate as %q, want no gate in the mode beside an earlier build", arrival.name, gate)
			}
			again := make(chan error, 1)
			go func() { again <- arrive(ctx, s, arrival, uuid.NewString()) }()
			select {
			case err := <-again:
				if err != nil {
					t.Fatalf("a second arrival beside a writer in the mode beside an earlier build: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("a second arrival waited 5s beside a writer that holds no gate")
			}
		})
	}
}

// gateRace is what the race below sizes: how many times the earlier build arrives, writes and
// leaves, how many row-first writes it makes each time on each of its two writers, and how many
// writers of this build run all the while.
const (
	gateRaceCycles        = 100
	gateRaceEarlierWrites = 6
	gateRaceWriters       = 4
)

// gateRaceTally is what one role of the race came to.
type gateRaceTally struct {
	commits, deadlocks, gateDeadlocks, failures int
}

// randomOwners is a random subset of all, at least two, and the same owners in another random
// order: what a writer of the races reads, and the order it writes them in.
func randomOwners(all []Owner) (subset, touch []Owner) {
	subset = slices.Clone(all)
	rand.Shuffle(len(subset), func(i, j int) { subset[i], subset[j] = subset[j], subset[i] })
	subset = subset[:2+rand.IntN(len(subset)-1)]
	touch = slices.Clone(subset)
	rand.Shuffle(len(touch), func(i, j int) { touch[i], touch[j] = touch[j], touch[i] })
	return subset, touch
}

// heartbeatAsThisBuild stamps instance's heartbeat as processes' heartbeat does: under the
// registry's lock, in a transaction of its own.
func heartbeatAsThisBuild(ctx context.Context, s *Store, instance string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, registryLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update server_processes set heartbeat_at = now() where instance = $1::uuid`, instance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// An earlier build arrives, writes and leaves, over and over, while writers of this build run
// throughout, each in the mode its own fleet read answers, and this build's process heartbeats.
// Each time, the earlier build publishes its dashboard, starts two row-first writers as its boot's
// repairs and passes do, registers while they run (every other time), and leaves once they end:
// its row deregistered and its mark aged past the window, as that much time passing does. No
// update is lost, nothing deadlocks and nothing else fails: a writer of this build that read no
// earlier build holds the gate until it ends, so the arrival waits for it, and every writer after
// the arrival takes the owners' rows. Writers run in both modes, and the arrivals that waited for
// the gate, and how long, are logged. With the gate taken out of owner_lock_legacy_arrival, a
// writer that read no earlier build writes beside that build's writers, and updates are lost.
func TestTheOwnerLockGateRacesAnEarlierBuildArrivingWritingAndLeaving(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	mine := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	all := owners.all()
	seqs := func() map[Owner]int {
		seq := map[Owner]int{}
		for _, owner := range all {
			table, column := ownerRowTable(owner)
			var value int
			if err := s.Pool.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&value); err != nil {
				t.Fatal(err)
			}
			seq[owner] = value
		}
		return seq
	}
	start := seqs()

	var mu sync.Mutex
	committed := map[Owner]int{}
	tallies := map[string]*gateRaceTally{}
	modes := map[bool]int{}
	gateCycle := fmt.Sprintf(",%d,%d,2]", OwnerLockGateNamespace, OwnerLockGateKey)
	record := func(role string, err error, touched []Owner) {
		mu.Lock()
		defer mu.Unlock()
		tally := tallies[role]
		if tally == nil {
			tally = &gateRaceTally{}
			tallies[role] = tally
		}
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			tally.commits++
			for _, owner := range touched {
				committed[owner]++
			}
		case errors.As(err, &pgErr) && pgErr.Code == "40P01":
			tally.deadlocks++
			if strings.Contains(pgErr.Detail, gateCycle) {
				tally.gateDeadlocks++
			}
			t.Logf("%s: deadlock: %s", role, pgErr.Detail)
		default:
			tally.failures++
			t.Logf("%s: %v", role, err)
		}
	}

	stop := make(chan struct{})
	var thisBuild sync.WaitGroup
	for range gateRaceWriters {
		thisBuild.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				subset, touch := randomOwners(all)
				legacy, err := raceWriter(ctx, s, fleetModeWriter, subset, touch)
				if err == nil {
					mu.Lock()
					modes[legacy]++
					mu.Unlock()
				}
				record(string(fleetModeWriter), err, touch)
			}
		})
	}
	thisBuild.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			record("this build's heartbeat", heartbeatAsThisBuild(ctx, s, mine), nil)
		}
	})

	var waits []time.Duration
	timedArrival := func(arrival earlierBuildArrival, key string) {
		began := time.Now()
		err := arrive(ctx, s, arrival, key)
		waits = append(waits, time.Since(began))
		record("earlier build, "+arrival.name, err, nil)
	}
	for cycle := range gateRaceCycles {
		timedArrival(earlierBuildArrivals[0], uuid.NewString())
		var earlier sync.WaitGroup
		for range 2 {
			earlier.Go(func() {
				for range gateRaceEarlierWrites {
					subset, touch := randomOwners(all)
					_, err := raceWriter(ctx, s, rowFirstWriter, subset, touch)
					record(string(rowFirstWriter), err, touch)
				}
			})
		}
		instance := uuid.NewString()
		if cycle%2 == 0 {
			timedArrival(earlierBuildArrivals[1], instance)
		}
		earlier.Wait()
		if _, err := s.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
			t.Fatal(err)
		}
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
		time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
	}
	close(stop)
	thisBuild.Wait()

	end := seqs()
	lost := 0
	for _, owner := range all {
		lost += start[owner] + committed[owner] - end[owner]
	}
	slices.Sort(waits)
	deadlocks, failures := 0, 0
	for role, tally := range tallies {
		t.Logf("%s: %d committed, %d deadlocks (40P01), %d of them through the gate, %d other failures",
			role, tally.commits, tally.deadlocks, tally.gateDeadlocks, tally.failures)
		deadlocks += tally.deadlocks
		failures += tally.failures
	}
	t.Logf("%d cycles: this build's writers committed %d in the mode beside an earlier build and %d in the advisory mode; %d arrivals took %s at the median, %s at the 90th percentile and %s at most; %d increments lost",
		gateRaceCycles, modes[true], modes[false], len(waits), waits[len(waits)/2], waits[len(waits)*9/10], waits[len(waits)-1], lost)
	if lost != 0 || deadlocks != 0 || failures != 0 {
		t.Errorf("%d increments lost, %d deadlocks, %d other failures; want none of each", lost, deadlocks, failures)
	}
	if modes[true] == 0 || modes[false] == 0 {
		t.Errorf("this build's writers committed %d beside an earlier build and %d in the advisory mode; want both modes raced", modes[true], modes[false])
	}
}
