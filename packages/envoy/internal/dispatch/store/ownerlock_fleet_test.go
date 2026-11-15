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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The owner lock beside a process of an earlier build (OwnerLockProtocol). A build before this one
// serialises an owner's writers on the owner's row, which it takes first and holds for its whole
// transaction, and never takes the advisory lock; so while one of its processes is live, every
// read-phase owner lock of this build takes the row too, right after the advisory lock and before
// the read it guards (lockOwnerRows). These tests write that earlier build's process as the row it
// leaves in server_processes, owner_lock_protocol at its default 0, and write the earlier build's
// writer as the statements it runs: the owner's row `for no key update` first, then the read,
// then the write.

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
// written by leaving the column out, as a build before the column writes its row.
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

// fleetState is one state of the registry a lock is taken beside.
type fleetState struct {
	name   string
	seed   func(t *testing.T, s *Store)
	legacy bool
}

// fleetStates is every registry state the owner lock tells apart: only a process of an earlier
// build whose heartbeat is live makes it take the row.
var fleetStates = []fleetState{
	{name: "no process registered", seed: func(*testing.T, *Store) {}},
	{name: "a live process of this build", seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, OwnerLockProtocol, 0) }},
	{name: "a lapsed process of an earlier build", seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, ProcessLiveWindow+time.Minute)
	}},
	{name: "a live process of an earlier build", legacy: true, seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, 0, 0) }},
}

// Each read-phase owner lock takes its owners' rows, before it answers, exactly while a live
// process of an earlier build is registered: LockOwner, LockOwners over every kind, LockOwnerAndRead
// and LockOwnersShared. A lapsed row of that build, or a live row of this one, takes none.
func TestTheOwnerLockTakesTheOwnerRowWhileAProcessOfAnEarlierBuildIsLive(t *testing.T) {
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
	}
	for _, state := range fleetStates {
		t.Run(state.name, func(t *testing.T) {
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
	for _, state := range []fleetState{fleetStates[0], fleetStates[3]} {
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
	for _, state := range []fleetState{fleetStates[0], fleetStates[3]} {
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

// ───── deadlocks and lost updates across the three writers ─────────────────

// writerShape is how one writer of the race below serialises: this build beside a process of an
// earlier build (advisory lock, then the rows), this build in the advisory mode (advisory lock,
// the rows only in the write phase), and the earlier build itself (the rows first, no advisory
// lock).
type writerShape string

const (
	legacyModeWriter writerShape = "this build, advisory and row"
	advisoryWriter   writerShape = "this build, advisory alone"
	rowFirstWriter   writerShape = "earlier build, row first"
)

// raceWriter runs one transaction of shape over owners: the read phase reads each owner's
// last_seq, the write phase writes each one back plus one, in touch's order, as a read-modify-write
// whose base the read phase decided. It answers the transaction's error. This build's writers take
// their write phase's row locks in touch's order, an order of the handler's appends, which no rule
// fixes since the advisory locks they hold already exclude each other; the earlier build's take
// theirs sorted by `<kind>:<key>` up front.
func raceWriter(ctx context.Context, s *Store, shape writerShape, owners []Owner, touch []Owner) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `set local lock_timeout = '20s'`); err != nil {
		return err
	}
	switch shape {
	case legacyModeWriter:
		if err := LockOwners(ctx, tx, owners...); err != nil {
			return err
		}
	case advisoryWriter:
		if _, err := lockOwnerKeys(ctx, tx, owners); err != nil {
			return err
		}
	case rowFirstWriter:
		sorted := slices.Clone(owners)
		slices.SortFunc(sorted, func(a, b Owner) int { return strings.Compare(a.LockKey(), b.LockKey()) })
		for _, owner := range sorted {
			table, column := ownerRowTable(owner)
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return err
			}
		}
	}
	seen := map[Owner]int{}
	for _, owner := range owners {
		table, column := ownerRowTable(owner)
		var seq int
		if err := tx.QueryRow(ctx, `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&seq); err != nil {
			return err
		}
		seen[owner] = seq
	}
	time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
	for _, owner := range touch {
		table, column := ownerRowTable(owner)
		if shape != rowFirstWriter {
			if _, err := tx.Exec(ctx, ownerLock, OwnerLockNamespace, owner.LockKey()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `select 1 from `+table+` where `+column+`::text = $1 for no key update`, owner.Key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `update `+table+` set last_seq = $2 where `+column+`::text = $1`, owner.Key, seen[owner]+1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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
				err := raceWriter(ctx, s, shape, subset, touch)
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
