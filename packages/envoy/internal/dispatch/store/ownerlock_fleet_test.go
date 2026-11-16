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
// (lockOwnerRows), and while none is, it tries the owner lock gate shared before its first owner
// lock (OwnerLockGateNamespace), which an earlier build's arrival takes exclusively, and takes the
// row as beside an earlier build when the try fails. These tests write that earlier build's
// process as the row it leaves in server_processes, owner_lock_protocol at its default 0, its
// boot's dashboard publish as the dashboard_builds row it leaves, the same, and the earlier build's
// writer as the statements it runs: the owner's row `for no key update` first, then the read, then
// the write.

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
// takes the owner lock gate and stamps the earlier build's mark (owner_lock_legacy_registration).
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
// dashboard publish or its registration - stamped the mark within OwnerLockMarkWindow. A process
// of it whose heartbeat stopped within its window still counts, where the registry's own live
// window has long dropped it: a row counts until it is deregistered or pruned. A mark older than a
// row's window and younger than its own still counts.
var fleetStates = []fleetState{
	{name: "no process registered", seed: func(*testing.T, *Store) {}},
	{name: "a live process of this build", seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, OwnerLockProtocol, 0) }},
	{name: "a process of an earlier build whose heartbeat stopped within the legacy window", legacy: true, seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, ProcessLiveWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
	}},
	{name: "a process of an earlier build whose heartbeat stopped before the legacy window", seed: func(t *testing.T, s *Store) {
		registerFleetProcess(t, s, 0, OwnerLockLegacyWindow+time.Minute)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
	}},
	{name: "a live process of an earlier build", legacy: true, seed: func(t *testing.T, s *Store) { registerFleetProcess(t, s, 0, 0) }},
	{name: "an earlier build's dashboard publish just now", legacy: true, seed: publishAsAnEarlierBuild},
	{name: "an earlier build's dashboard publish older than the legacy window, within the mark's", legacy: true, seed: func(t *testing.T, s *Store) {
		publishAsAnEarlierBuild(t, s)
		ageLegacyMark(t, s, OwnerLockLegacyWindow+time.Minute)
	}},
	{name: "an earlier build's dashboard publish before the mark's window", seed: func(t *testing.T, s *Store) {
		publishAsAnEarlierBuild(t, s)
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
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
// the arrival's insert takes it exclusively (owner_lock_legacy_arrival,
// owner_lock_legacy_registration). It goes on once that transaction commits, and the next owner
// lock reads the earlier build in the fleet and takes the owner's row, without the gate; a second
// arrival then waits for nothing, though that writer is between its owner lock and its write too.
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
			awaitGateWait(t, s, arrivalPID, arrived, arrival.name)
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

// awaitGateWait waits up to ten seconds for backend pid to wait for the owner lock gate
// exclusively, as an earlier build's arrival waits for it, and fails the test when what it runs,
// named what, ends first (ended).
func awaitGateWait(t *testing.T, s *Store, pid uint32, ended <-chan error, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if gate, granted := gateLock(t, s, pid); gate == "ExclusiveLock" && !granted {
			return
		}
		select {
		case err := <-ended:
			t.Fatalf("%s ended (%v) while a transaction held the gate shared; want it to wait for the gate", what, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not wait for the gate within 10s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writerGateBound is how long an owner lock of this build may take while an arrival waits for the
// gate, once its statements are prepared on its connection: its statement and the owners' rows
// after it, two round trips with no wait in them.
const writerGateBound = 50 * time.Millisecond

// A writer of this build that starts while an earlier build's arrival waits for the owner lock gate
// never queues behind that arrival. The arrival waits for a transaction that read no earlier build
// and holds the gate shared; the writer's owner lock tries the gate shared, which Postgres refuses
// while the arrival's exclusive request waits, and so takes its owners' rows, as it does beside an
// earlier build, at once. Each read-phase owner lock - LockOwner, LockOwnersShared, TryLockOwners -
// answers in the mode beside an earlier build, holding its owner's row and no gate, and the arrival
// is still waiting after them. Each runs twice on one connection: the first run prepares its
// statements there, which is no cost of the gate, and the second answers within writerGateBound.
// With the gate waited for rather than tried, the first of them queues behind the arrival until
// the transaction it waits for ends.
func TestAWriterStartedWhileAnArrivalWaitsForTheGateTakesTheOwnersRowsAtOnce(t *testing.T) {
	for _, arrival := range earlierBuildArrivals {
		t.Run(arrival.name, func(t *testing.T) {
			s, owners := openFleetStore(t)
			ctx := context.Background()
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
			go func() { arrived <- arriveOn(ctx, conn, arrival, uuid.NewString()) }()
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
			for _, measured := range []bool{false, true} {
				for _, writer := range writers {
					tx, err := writerConn.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					locked := make(chan error, 1)
					began := time.Now()
					go func() { locked <- writer.lock(tx) }()
					select {
					case err := <-locked:
						took := time.Since(began)
						if err != nil {
							t.Fatalf("%s: %v", writer.name, err)
						}
						if measured {
							t.Logf("%s answered in %s while %s waited for the gate", writer.name, took, arrival.name)
							if took > writerGateBound {
								t.Errorf("%s took %s while %s waited for the gate, want at most %s: no wait on the gate", writer.name, took, arrival.name, writerGateBound)
							}
						}
					case <-time.After(5 * time.Second):
						t.Errorf("%s waited 5s while %s waited for the gate; want it to try the gate, not queue behind the arrival", writer.name, arrival.name)
						_ = holder.Rollback(ctx)
						<-locked
						_ = tx.Rollback(ctx)
						t.FailNow()
					}
					if mode := LastOwnerLockMode(); mode != OwnerLockAdvisoryAndRow {
						t.Errorf("%s while %s waited for the gate took the %s mode, want %s", writer.name, arrival.name, mode, OwnerLockAdvisoryAndRow)
					}
					if !ownerRowHeld(t, s, writer.owner) {
						t.Errorf("%s while %s waited for the gate holds no row of %s, want it held beside the advisory lock", writer.name, arrival.name, writer.owner.LockKey())
					}
					if gate, granted := gateLock(t, s, writerConn.Conn().PgConn().PID()); gate != "" {
						t.Errorf("%s while %s waited for the gate holds it as %q (granted %t), want no gate", writer.name, arrival.name, gate, granted)
					}
					if err := tx.Rollback(ctx); err != nil {
						t.Fatal(err)
					}
				}
			}
			if gate, granted := gateLock(t, s, arrivalPID); gate != "ExclusiveLock" || granted {
				t.Errorf("after the writers %s holds the gate as %q (granted %t), want it still waiting for the transaction in flight", arrival.name, gate, granted)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-arrived:
				if err != nil {
					t.Fatalf("%s after the transaction in flight committed: %v", arrival.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not end within 10s of the commit of the transaction it waited for", arrival.name)
			}
		})
	}
}

// registrationGateBound bounds an earlier build's registration's wait for the owner lock gate:
// owner_lock_legacy_registration's lock_timeout, spelled again here.
const registrationGateBound = 5 * time.Second

// hasLegacyMark reports whether owner_lock_legacy_mark holds its row.
func hasLegacyMark(t *testing.T, s *Store) bool {
	t.Helper()
	var marked bool
	if err := s.Pool.QueryRow(context.Background(), `select exists (select from owner_lock_legacy_mark)`).Scan(&marked); err != nil {
		t.Fatalf("read the earlier build's mark: %v", err)
	}
	return marked
}

// An earlier build's registration waits for the owner lock gate at most registrationGateBound. It
// holds the registry's lock while it waits, and every process's heartbeat takes that lock too, so
// behind a transaction of this build that holds the gate shared for longer, the registration
// fails with 55P03 lock_not_available once the bound has passed, stamps no mark, and lets go of the
// registry's lock: a heartbeat of this build queued on it goes through. A dashboard publish keeps
// waiting past the bound, since the earlier build bounds its publish itself, and goes through once
// that transaction commits. Without the bound the registration waits for that transaction as
// long as the publish does, and the heartbeat with it.
func TestAnEarlierBuildsRegistrationWaitsForTheGateNoLongerThanItsBound(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	mine := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	holder, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if err := LockOwner(ctx, holder, owners.issueA); err != nil {
		t.Fatal(err)
	}
	if gate, granted := gateLock(t, s, holder.Conn().PgConn().PID()); gate != "ShareLock" || !granted {
		t.Fatalf("the transaction in flight holds the gate as %q (granted %t), want it held shared", gate, granted)
	}

	registration, publish := earlierBuildArrivals[1], earlierBuildArrivals[0]
	registrationConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer registrationConn.Release()
	registered := make(chan error, 1)
	began := time.Now()
	go func() { registered <- arriveOn(ctx, registrationConn, registration, uuid.NewString()) }()
	awaitGateWait(t, s, registrationConn.Conn().PgConn().PID(), registered, registration.name)
	beat := make(chan error, 1)
	go func() { beat <- heartbeatAsThisBuild(ctx, s, mine) }()
	select {
	case err := <-registered:
		took := time.Since(began)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("%s behind a transaction holding the gate past the bound ended with %v after %s, want 55P03 lock_not_available", registration.name, err, took)
		}
		t.Logf("%s was refused after %s: %s", registration.name, took.Round(time.Millisecond), pgErr.Message)
	case <-time.After(registrationGateBound + 3*time.Second):
		t.Errorf("%s still waited for the gate %s after it started, past its bound of %s", registration.name, time.Since(began).Round(time.Millisecond), registrationGateBound)
		_ = holder.Rollback(ctx)
		<-registered
		<-beat
		t.FailNow()
	}
	select {
	case err := <-beat:
		if err != nil {
			t.Fatalf("this build's heartbeat after %s was refused: %v", registration.name, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("this build's heartbeat did not go through within 5s of %s's refusal", registration.name)
	}
	if hasLegacyMark(t, s) {
		t.Errorf("%s was refused and the earlier build's mark is stamped, want none", registration.name)
	}

	publishConn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publishConn.Release()
	published := make(chan error, 1)
	go func() { published <- arriveOn(ctx, publishConn, publish, uuid.NewString()) }()
	awaitGateWait(t, s, publishConn.Conn().PgConn().PID(), published, publish.name)
	select {
	case err := <-published:
		t.Fatalf("%s ended (%v) within %s while the transaction it waits for was still under way; want it to wait as long as the earlier build's publish lets it",
			publish.name, err, registrationGateBound+time.Second)
	case <-time.After(registrationGateBound + time.Second):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("%s after the transaction it waited for committed: %v", publish.name, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end within 10s of the commit of the transaction it waited for", publish.name)
	}
	if !hasLegacyMark(t, s) {
		t.Errorf("%s went through and the earlier build's mark is not stamped", publish.name)
	}
}

// ownerLockLegacyGateVersion is migration 20261115200014_owner_lock_legacy_gate's version.
const ownerLockLegacyGateVersion = 20261115200014

// The migration stamps the earlier build's mark at the newest dashboard publish of a build before
// it, when that publish is within the mark's window. An earlier build whose publish committed just
// before the migration ran made no arrival, since the trigger was not there yet, and it is not
// registered yet: a write of this build that starts then reads the mark and takes the owner's row
// beside its advisory lock, so while it is held between its owner lock and its write phase, the
// earlier build's write of the same owner - the row first - waits for it, and lands after it: both
// increments are kept. A publish older than the window, and no publish at all, which is what every
// test database has, leave no mark, and the write takes the advisory lock alone. Without the
// migration's insert the earlier build's write goes through while this build's is held, and is
// lost under it.
func TestTheMigrationMarksAnEarlierBuildThatPublishedBeforeIt(t *testing.T) {
	cases := []struct {
		name    string
		publish bool
		age     time.Duration
		legacy  bool
	}{
		{name: "an earlier build's publish just before the migration", publish: true, legacy: true},
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
			want := OwnerLockAdvisory
			if c.legacy {
				want = OwnerLockAdvisoryAndRow
			}
			if mode := LastOwnerLockMode(); mode != want {
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
				_, err := raceWriter(ctx, s, rowFirstWriter, []Owner{issue}, []Owner{issue})
				wrote <- err
			}()
			ended := false
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("the earlier build's write: %v", err)
					}
					t.Errorf("the earlier build's write went through while this build's write was between its owner lock and its write phase; want it to wait for the owner's row")
					ended = true
				default:
				}
				if ended {
					break
				}
				var queued bool
				if err := s.Pool.QueryRow(ctx, `
					select exists (select from pg_stat_activity where datname = current_database() and $1::integer = any(pg_blocking_pids(pid)))`,
					writerPID).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				if queued {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the earlier build's write neither went through nor queued behind this build's write within 10s")
				}
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
			if !ended {
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("the earlier build's write after this build's committed: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the earlier build's write did not end within 10s of this build's commit")
				}
			}
			if got := ownerSeqs(t, s, []Owner{issue})[issue]; got != seq+2 {
				t.Errorf("the issue's last_seq is %d after both writes from %d, want %d: the earlier build's write was lost under this build's", got, seq, seq+2)
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

// gateRaceTally is what one role of the races came to.
type gateRaceTally struct {
	commits, deadlocks, gateDeadlocks, failures int
}

// gateCycle is how a deadlock's detail names the owner lock gate, as one of the locks of its cycle.
var gateCycle = fmt.Sprintf(",%d,%d,2]", OwnerLockGateNamespace, OwnerLockGateKey)

// raceLedger is what the writers of a race committed: each role's tally, every increment they
// committed by owner, and how many of this build's writers committed in each mode (true: beside an
// earlier build). The race's goroutines share it.
type raceLedger struct {
	mu        sync.Mutex
	committed map[Owner]int
	tallies   map[string]*gateRaceTally
	modes     map[bool]int
}

func newRaceLedger() *raceLedger {
	return &raceLedger{committed: map[Owner]int{}, tallies: map[string]*gateRaceTally{}, modes: map[bool]int{}}
}

// record counts one transaction of role that touched touched: its commit, or its failure, a
// deadlock (40P01) and whether its cycle ran through the gate, or any other, logged.
func (l *raceLedger) record(t *testing.T, role string, err error, touched []Owner) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tally := l.tallies[role]
	if tally == nil {
		tally = &gateRaceTally{}
		l.tallies[role] = tally
	}
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		tally.commits++
		for _, owner := range touched {
			l.committed[owner]++
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

// write runs one writer of shape over a random subset of all and records it, and for this
// build's writer as its fleet read answers, the mode it committed in.
func (l *raceLedger) write(ctx context.Context, t *testing.T, s *Store, shape writerShape, all []Owner) {
	subset, touch := randomOwners(all)
	legacy, err := raceWriter(ctx, s, shape, subset, touch)
	if err == nil && shape == fleetModeWriter {
		l.mu.Lock()
		l.modes[legacy]++
		l.mu.Unlock()
	}
	l.record(t, string(shape), err, touch)
}

// settle logs each role's tally and answers the increments lost, every owner's last_seq read
// against start and the increments committed, and the deadlocks and other failures of every role.
// It is called once the race's goroutines have ended.
func (l *raceLedger) settle(t *testing.T, s *Store, all []Owner, start map[Owner]int) (lost, deadlocks, failures int) {
	t.Helper()
	end := ownerSeqs(t, s, all)
	for _, owner := range all {
		lost += start[owner] + l.committed[owner] - end[owner]
	}
	for role, tally := range l.tallies {
		t.Logf("%s: %d committed, %d deadlocks (40P01), %d of them through the gate, %d other failures",
			role, tally.commits, tally.deadlocks, tally.gateDeadlocks, tally.failures)
		deadlocks += tally.deadlocks
		failures += tally.failures
	}
	return lost, deadlocks, failures
}

// ownerSeqs is every owner's last_seq.
func ownerSeqs(t *testing.T, s *Store, owners []Owner) map[Owner]int {
	t.Helper()
	seq := map[Owner]int{}
	for _, owner := range owners {
		table, column := ownerRowTable(owner)
		var value int
		if err := s.Pool.QueryRow(context.Background(), `select last_seq from `+table+` where `+column+`::text = $1`, owner.Key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		seq[owner] = value
	}
	return seq
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
// earlier build holds the gate until it ends, so the arrival waits for it, and every writer that
// starts while the arrival waits, or after it, takes the owners' rows. Writers run in both modes,
// and the arrivals that waited for the gate, and how long, are logged. With the gate taken out of
// the two arrival triggers, a writer that read no earlier build writes beside that build's
// writers, and updates are lost.
func TestTheOwnerLockGateRacesAnEarlierBuildArrivingWritingAndLeaving(t *testing.T) {
	s, owners := openFleetStore(t)
	ctx := context.Background()
	mine := registerFleetProcess(t, s, OwnerLockProtocol, 0)
	all := owners.all()
	start := ownerSeqs(t, s, all)
	ledger := newRaceLedger()

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
				ledger.write(ctx, t, s, fleetModeWriter, all)
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
			ledger.record(t, "this build's heartbeat", heartbeatAsThisBuild(ctx, s, mine), nil)
		}
	})

	var waits []time.Duration
	timedArrival := func(arrival earlierBuildArrival, key string) {
		began := time.Now()
		err := arrive(ctx, s, arrival, key)
		waits = append(waits, time.Since(began))
		ledger.record(t, "earlier build, "+arrival.name, err, nil)
	}
	for cycle := range gateRaceCycles {
		timedArrival(earlierBuildArrivals[0], uuid.NewString())
		var earlier sync.WaitGroup
		for range 2 {
			earlier.Go(func() {
				for range gateRaceEarlierWrites {
					ledger.write(ctx, t, s, rowFirstWriter, all)
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
		ageLegacyMark(t, s, OwnerLockMarkWindow+time.Minute)
		time.Sleep(time.Duration(10+rand.IntN(40)) * time.Millisecond)
	}
	close(stop)
	thisBuild.Wait()

	lost, deadlocks, failures := ledger.settle(t, s, all, start)
	slices.Sort(waits)
	t.Logf("%d cycles: this build's writers committed %d in the mode beside an earlier build and %d in the advisory mode; %d arrivals took %s at the median, %s at the 90th percentile and %s at most; %d increments lost",
		gateRaceCycles, ledger.modes[true], ledger.modes[false], len(waits), waits[len(waits)/2], waits[len(waits)*9/10], waits[len(waits)-1], lost)
	if lost != 0 || deadlocks != 0 || failures != 0 {
		t.Errorf("%d increments lost, %d deadlocks, %d other failures; want none of each", lost, deadlocks, failures)
	}
	if ledger.modes[true] == 0 || ledger.modes[false] == 0 {
		t.Errorf("this build's writers committed %d beside an earlier build and %d in the advisory mode; want both modes raced", ledger.modes[true], ledger.modes[false])
	}
}

// markExpiryWindow is the earlier build's mark's window in the race below, short enough for the
// mark to expire inside it.
const markExpiryWindow = 2 * time.Second

// The earlier build's mark expires while writers of this build are in flight in both modes:
// those whose fleet read found the mark take the owners' rows beside their advisory locks, and
// those that start once it has expired take the advisory locks alone, behind the gate, while
// the first are still committing. The earlier build's own writers, the rows first, run only while
// the mark is young, as an earlier build that has stopped writes nothing more: an earlier build
// that still writes past the mark is in the fleet only by its registration (the two windows that
// stay open, cmd/dispatch/README.md). No update is lost, nothing deadlocks and nothing else
// fails, and this build's writers commit in both modes. The mark's window is shortened to
// markExpiryWindow so that it expires inside the run.
func TestTheMarkExpiresWhileWritersOfBothModesAreInFlight(t *testing.T) {
	s, owners := openFleetStore(t)
	window := OwnerLockMarkWindow
	OwnerLockMarkWindow = markExpiryWindow
	t.Cleanup(func() { OwnerLockMarkWindow = window })
	ctx := context.Background()
	all := owners.all()
	start := ownerSeqs(t, s, all)
	ledger := newRaceLedger()
	publishAsAnEarlierBuild(t, s)
	stamped := time.Now()

	var writers sync.WaitGroup
	for range gateRaceWriters {
		writers.Go(func() {
			for time.Since(stamped) < 2*markExpiryWindow {
				ledger.write(ctx, t, s, fleetModeWriter, all)
			}
		})
	}
	for range 2 {
		writers.Go(func() {
			for time.Since(stamped) < markExpiryWindow/2 {
				ledger.write(ctx, t, s, rowFirstWriter, all)
			}
		})
	}
	writers.Wait()

	lost, deadlocks, failures := ledger.settle(t, s, all, start)
	t.Logf("the mark expired %s after its stamp: this build's writers committed %d in the mode beside an earlier build and %d in the advisory mode; %d increments lost",
		markExpiryWindow, ledger.modes[true], ledger.modes[false], lost)
	if lost != 0 || deadlocks != 0 || failures != 0 {
		t.Errorf("%d increments lost, %d deadlocks, %d other failures; want none of each", lost, deadlocks, failures)
	}
	if ledger.modes[true] == 0 || ledger.modes[false] == 0 {
		t.Errorf("this build's writers committed %d beside an earlier build and %d in the advisory mode; want both modes across the mark's expiry", ledger.modes[true], ledger.modes[false])
	}
}
